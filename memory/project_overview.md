---
name: Project Overview
description: TwitchCaster architecture, key files, Twitch API endpoints, OAuth flow, and casting pipeline
type: project
---

Personal Go web app that lets the user cast Twitch streams to Chromecast devices.

**Why:** Personal home setup — casts to Living Room, Jacob's Office, Master Bedroom, Kitchen Display, Gym chromecasts.

**Stack:**
- Go HTTP server on port 3010, reverse-proxied via `https://twitch.shadowline.net`
- `streamlink` CLI (must be installed on host) for stream resolution and proxying
- `github.com/vishen/go-chromecast v0.2.0` for casting

**Key files:**
- `main.go` — routes, config loading
- `endpoints/twitch.go` — cast logic (`proxyAndCast`), channel list HTML
- `endpoints/auth.go` — Twitch OAuth flow (`/auth/twitch` redirect, `/oauth` callback)
- `cast/wrapper.go` — thin wrapper around go-chromecast `app.Load()`
- `services/twitch.go` — Twitch Helix API calls (`GET /helix/streams/followed`, `GET /helix/users`)
- `auth/authManager.go` — token management (OAuth user token preferred, falls back to client credentials)
- `configuration.json` — chromecasts list, Twitch app credentials, baseURL

**Twitch API:** Uses `GET /helix/streams/followed` (requires OAuth user token with `user:read:follows` scope). The old `users/follows` endpoint was removed Sept 2023.

**OAuth:** Full Authorization Code flow. Token stored in memory only (resets on restart). Redirect URI must use `baseURL` from config (not `r.Host` — app is behind a reverse proxy).
