package main

import (
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"twitch-caster/config"
	"twitch-caster/endpoints"
)

func main() {
	config := config.Load()

	twitchEndpoint := endpoints.NewTwitchEndpoint(config, 3010)
	authEndpoint := endpoints.NewAuthEndpoint(config)

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/receiver", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, "static/receiver.html")
	})
	http.HandleFunc("/auth/twitch", authEndpoint.OAuthRedirect)
	http.HandleFunc("/oauth", authEndpoint.OAuthCallback)
	http.HandleFunc(config.Settings.ChannelListURL, twitchEndpoint.TwitchChannelList)
	http.HandleFunc(config.Settings.CastURL, twitchEndpoint.CastTwitch)
	http.HandleFunc("/stop-cast/", twitchEndpoint.StopCast)
	http.HandleFunc("/stop-cast", twitchEndpoint.StopCast)
	http.HandleFunc("/stream/", twitchEndpoint.StreamProxy)

	// Serve ffmpeg-generated HLS files for mpegTS devices (Google TV).
	// Register MIME types so the Cast receiver sees the right Content-Type.
	mime.AddExtensionType(".m3u8", "application/x-mpegURL")
	mime.AddExtensionType(".ts", "video/mp2t")
	hlsRoot := filepath.Join(os.TempDir(), "tc-hls")
	os.RemoveAll(hlsRoot)
	os.MkdirAll(hlsRoot, 0755)
	http.Handle("/hls-files/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record access so the proxy watchdog knows the receiver is still active.
		// Path is /hls-files/{hlsID}/..., so parts[2] is the hlsID.
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/hls-files/"), "/", 2)
		if len(parts) > 0 && parts[0] != "" {
			endpoints.RecordHLSAccess(parts[0])
		}
		switch filepath.Ext(r.URL.Path) {
		case ".m3u8":
			w.Header().Set("Content-Type", "application/x-mpegURL")
		case ".ts":
			w.Header().Set("Content-Type", "video/mp2t")
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		http.StripPrefix("/hls-files/", http.FileServer(http.Dir(hlsRoot))).ServeHTTP(w, r)
	}))

	log.Fatal(http.ListenAndServe(":3010", nil))
}
