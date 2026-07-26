# Troubleshooting

## Receiver playback stalls (silent pauses)

**Symptom:** a stream that's been playing fine freezes on the TV. The backend
logs show no error — streamlink and ffmpeg are alive, segments are fresh, and
the receiver is still fetching. Historically the only fix was a manual recast.
Observed frequency: roughly once every ~5 hours of playback.

These stalls are **receiver-side**: the custom CAF receiver's MSE (Media Source
Extensions) player freezes while the server-side pipeline stays healthy. Most of
them throw no JavaScript error, which is why nothing showed up in the logs.

### How detection + recovery works

The receiver (`static/receiver.html`) POSTs a heartbeat to
`/receiver-heartbeat/{deviceId}` **every 5 seconds** while a stream is active.
Each heartbeat carries the playhead position plus a diagnostic snapshot of the
MSE player. Handled server-side by `ReceiverHeartbeat` in `endpoints/twitch.go`.

- **Healthy heartbeats are silent** — nothing is logged.
- **Errors are logged** if the receiver reports any (see below).

Recovery happens in two tiers, cheapest first:

1. **Gap jump (receiver-side, primary).** The dominant cause of these stalls is
   a *buffer-timeline hole*: MSE parks the playhead at the end of a buffered
   range and waits forever when the next range starts past a small gap (a
   segment timestamp discontinuity — usually an upstream Twitch ad break or
   reconnect, not a lost file). The receiver runs a watchdog (`checkStall` in
   `static/receiver.html`) that, after **2.5s** of no progress (`GAP_STALL_MS`),
   seeks just across the hole to the next buffered range. The viewer sees at
   most a sub-second skip; the buffer that was already there is kept. Each jump
   rides the next heartbeat and is logged server-side as `receiver: gap-jump …`
   (a recovery, not an error). Holes larger than **15s** (`MAX_GAP_JUMP_S`) are
   left to the rebuild instead.
2. **Player rebuild (server-side, fallback).** If the playhead stays frozen for
   **15s** (`playbackStallThreshold`) while the newest HLS segment is still
   fresh (< 12s old, `segmentFreshThreshold`) — i.e. the gap jump didn't apply
   or didn't recover it (a decoder wedge, MediaSource teardown, etc.) — the
   server calls `forcePlayerRebuild`, bumping the published `Seq`. The
   receiver's `/current-stream` poll sees a "new" stream and rebuilds its player
   against the still-running proxy — the same MSE reset a manual recast does,
   without restarting streamlink/ffmpeg.

- If playback is frozen but **segments are stale**, that's a pipeline problem,
  not a receiver stall — the server logs it and leaves it to the existing
  exit/idle watchdogs rather than rebuilding the player.

Two receiver-side guards reduce how often those tiers fire at all:

- **Segment fetch retry.** A failed segment fetch (network error *or* non-200
  response) is retried on subsequent manifest polls, up to 3 total attempts
  (`MAX_SEG_FETCH_ATTEMPTS`), before the segment is skipped for good — only
  then does it leave a hole for the gap jumper. Giving up is logged as
  `receiver error: segment N skipped after 3 failed fetches`.
- **Quota recovery.** An `appendBuffer` that throws `QuotaExceededError` no
  longer drops the segment: the receiver trims the back buffer to ~5s behind
  the playhead and retries the same append once the trim completes. Only a
  quota error with nothing left to trim drops the segment (logged as
  `appendBuffer failed: QuotaExceededError …`).

Tuning constants: `playbackStallThreshold` / `segmentFreshThreshold` at the top
of `endpoints/twitch.go`; `GAP_STALL_MS`, `MAX_GAP_JUMP_S`, `STALL_POLL_MS`,
`MAX_SEG_FETCH_ATTEMPTS`, and the `HEARTBEAT_MS` cadence in
`static/receiver.html`.

### Log lines to look for

Gap jumps (the common case now — a recovery, logged as it happens):

```
[192.168.1.233] receiver: gap-jump 2177.6s -> 2178.72s (gap 0.62s)
```

A run of these is the fingerprint of upstream discontinuities. Watch their size
and frequency; a sudden burst tends to line up with an ad break or reconnect.

Thrown errors (reported by the receiver, logged as they arrive):

```
[192.168.1.233] receiver error: appendBuffer failed: QuotaExceededError ...
```

A detected stall (logged at the moment of the freeze, with the full snapshot):

```
[192.168.1.233] playback frozen 16s at t=142.0s with fresh segments — forcing receiver rebuild —
  readyState=2 netState=2 bufferAhead=0.0s bufferedEnd=142.0s gapAhead=false
  queue=0 appending=false ms=open lastAppend=14.8s lastFetch=14.9s
```

A frozen playhead with a stale feed (pipeline problem, no rebuild):

```
[192.168.1.233] playback frozen 16s at t=142.0s but newest segment is 40s old —
  pipeline stall, not rebuilding receiver — <snapshot>
```

Pull them on the Pi with:

```
journalctl -u twitch-caster --no-pager | grep -E "receiver error|receiver: gap-jump|playback frozen"
```

### Snapshot field reference

| Field | Meaning |
|---|---|
| `readyState` | `video.readyState`: 0=NOTHING 1=METADATA 2=CURRENT 3=FUTURE 4=ENOUGH. < 3 means not enough buffered to play forward. |
| `netState` | `video.networkState`: 0=EMPTY 1=IDLE 2=LOADING 3=NO_SOURCE. |
| `bufferAhead` | Seconds of buffered media ahead of the playhead. ~0 while the feed is alive ⇒ starvation. |
| `bufferedEnd` | End of the last buffered range (total buffer depth). |
| `gapAhead` | `true` when there's a hole ahead of the playhead with buffered media beyond it — even if the playhead is still inside its range (`bufferAhead` small but `bufferedEnd` well beyond it). This is the gap-jump trigger. |
| `queue` | Pending append-queue length. |
| `appending` | Whether an `appendBuffer` is in flight. |
| `ms` | MediaSource state: `open` (healthy), `ended`, `closed`, or `none`. |
| `lastAppend` | Seconds since the last successful segment append (-1 = never). |
| `lastFetch` | Seconds since the last freshly-fetched segment (-1 = never). |
| `err` | `video.error` code, if any (usually absent on silent stalls). |

### Reading the signature — which failure mode hit

The combination of fields identifies the cause. These are mutually
distinguishable:

| Signature | Cause |
|---|---|
| `bufferAhead≈0` + `lastAppend`/`lastFetch` climbing (> 10s) | **Starvation** — the append/fetch loop stopped feeding. Compare `lastFetch` vs `lastAppend` to see whether *fetching* or *appending* died. |
| `gapAhead=true` (or `bufferAhead≈0` while `bufferedEnd` sits seconds beyond it) | **Buffer gap** — a timestamp discontinuity left a hole the player won't cross. **Now auto-handled** by the receiver-side gap jump; look for `receiver: gap-jump …` lines. A `playback frozen` line with this signature means a jump was skipped (gap > 15s) or failed. |
| `bufferAhead>0` + `readyState≥3` but frozen | **Decoder wedge** — data is present and healthy but playback is stuck. The "nothing looks wrong" case. |
| `ms=ended` or `ms=closed` | The MediaSource tore down unexpectedly. |
| `queue>0` + `appending=true` + `lastAppend` high | **Append pipeline hung** on a stuck `appendBuffer`. |

Any genuine thrown error (`QuotaExceededError`, decode `MediaError`,
unsupported codec) also logs separately as `[ip] receiver error: ...` and should
be correlated with the stall line by timestamp/device.

### Collecting data

The first captured stall was a textbook **buffer gap** (`bufferAhead=0.4s` but
`bufferedEnd` 22s beyond it), which is what the gap jump now targets. The open
question is whether *every* stall is that mode. So: let the logs accumulate over
days and watch what shows up.

- `receiver: gap-jump …` lines — expected and healthy; gaps are being skipped
  without a rebuild. Frequency/size is the signal for how often upstream
  discontinuities hit.
- Any remaining `playback frozen … forcing receiver rebuild` lines — these are
  the stalls gap-jumping did *not* resolve. Read their snapshot against the
  signature table: if they're not gaps (decoder wedge, `ms=ended`, hung append),
  that's the next mode to harden. If they *are* gaps (> 15s), consider raising
  `MAX_GAP_JUMP_S`.
