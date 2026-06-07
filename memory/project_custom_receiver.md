---
name: project-custom-receiver
description: Custom Chromecast receiver implementation details, gotchas discovered on Google TV Streamer
metadata:
  type: project
---

Custom Cast receiver (app ID 7BB8F04F) hosted at https://twitch.shadowline.net/receiver.

**Why:** Default Media Receiver (CC1AD845) accumulates state/memory over time causing buffering and framey playback. Custom receiver starts fresh on each launch.

**Architecture:** Minimal receiver — just `context.start()`, no shaka. CAF's ExoPlayer backend handles HLS natively and works better than shaka for this use case.

**How to apply:** All Chromecasts are configured with `"receiverAppId": "7BB8F04F"` in configuration.json. Go code always routes through HLS file proxy (ffmpeg → TS) when a custom receiver is in use, serving from the external HTTPS URL to avoid CORS.

## Gotchas discovered on Google TV Streamer

- `playerManager.getMediaElement()` does NOT exist on this device's Cast SDK version. Use `document.querySelector('cast-media-player')` and call `.getMediaElement()` on the element instead.
- `async`/`await` in a CAF `setMessageInterceptor` LOAD handler gets silently cancelled by the framework. Use plain `.then().catch()` Promise chains.
- `let` variables in closures caused silent failures (possibly a TDZ bug in the older V8). Use `var` for any variables accessed inside Cast interceptors.
- shaka-player failed due to the above issues and also had MSE/CAF integration problems causing infinite buffering. CAF native (ExoPlayer) handles the proxied TS HLS correctly and is the right choice here.
- Direct Twitch HLS URLs (usher/ttvnw.net) fail with CORS error 1001 in the browser context. Solved by always routing through the HLS file proxy served from twitch.shadowline.net (same origin as receiver).
