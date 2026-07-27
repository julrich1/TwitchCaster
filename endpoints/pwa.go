package endpoints

import (
	"encoding/json"
	"log"
	"net/http"
)

// PWAEndpoint serves the web app manifest that makes the channel list
// installable as a home-screen app (Chrome on Android builds a WebAPK from it).
// Chrome dropped the service-worker requirement for installability in 108, so a
// manifest plus 192/512 icons over HTTPS is all this needs — and since every
// screen here is live cast state, there is nothing worth caching offline.
//
// The manifest stays outside session auth on purpose: browsers fetch it with
// credentials omitted, so a protected manifest would 401 and silently break
// installation. It carries no secrets.
type PWAEndpoint struct {
	manifest []byte
}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

// NewPWAEndpoint builds the manifest once at startup. startURL is the
// configured channel-list path, so the installed icon opens the list directly
// rather than the site root.
func NewPWAEndpoint(startURL string) *PWAEndpoint {
	manifest := map[string]any{
		// Identity is pinned separately from start_url: changing where the app
		// opens later must not orphan an already-installed copy.
		"id":               "/twitchcaster",
		"name":             "TwitchCaster",
		"short_name":       "TwitchCaster",
		"description":      "Cast the Twitch streams you follow to any Chromecast in the house.",
		"start_url":        startURL,
		"scope":            "/",
		"display":          "standalone",
		"background_color": "#070710",
		"theme_color":      "#070710",
		"categories":       []string{"entertainment"},
		"icons": []manifestIcon{
			{Src: "/static/icon-192.png", Sizes: "192x192", Type: "image/png", Purpose: "any"},
			{Src: "/static/icon-512.png", Sizes: "512x512", Type: "image/png", Purpose: "any"},
			{Src: "/static/icon-maskable-512.png", Sizes: "512x512", Type: "image/png", Purpose: "maskable"},
		},
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		log.Fatalln("Error building the web app manifest: ", err)
	}
	return &PWAEndpoint{manifest: data}
}

// Manifest serves /manifest.webmanifest.
func (p *PWAEndpoint) Manifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(p.manifest)
}
