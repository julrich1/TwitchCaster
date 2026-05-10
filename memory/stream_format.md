---
name: Stream Format Handling
description: How TwitchCaster handles CMAF vs standard HLS streams and Chromecast compatibility
type: project
---

Twitch's "Enhanced Broadcasting" beta streams use CMAF/fMP4 HLS (version 6, `.mp4` segments, `#EXT-X-MAP` init segment). Standard streams use MPEG-TS HLS (version 3, `.ts` segments). Older Chromecasts cannot play CMAF HLS natively.

**Detection:** `isCMAFManifest()` in `endpoints/twitch.go` fetches the HLS manifest and checks for `#EXT-X-MAP`.

**Cast strategy in `proxyAndCast()`:**
1. Call `streamlink --stream-url --twitch-supported-codecs h264` to get a direct HLS URL
2. Probe the manifest — if no `#EXT-X-MAP`, it's standard TS: cast the HLS URL directly as `application/x-mpegURL` (Chromecast handles HLS natively)
3. If H264 fails or manifest is CMAF: start streamlink with `--player-external-http --player-external-http-port 50505` as a local proxy; cast proxy URL as `video/mp4`

**Why proxy for CMAF:** streamlink absorbs the CMAF fMP4 segments and re-serves them as a continuous byte stream. The Chromecast receives it as `video/mp4` which it can play.

**Why not proxy for standard streams:** Raw MPEG-TS via proxy served as `video/mp4` causes "loads briefly then ends" on Chromecast. Direct HLS is the correct approach for standard streams.

**go-chromecast quirk:** `app.Load(url, contentType, ...)` — if `contentType` is empty string, the library calls `filepath.Ext()` on the URL for detection. An IP:port URL like `192.168.1.123:50505` extracts `.123:50505` as a bogus extension and fails. Always pass the content type explicitly.
