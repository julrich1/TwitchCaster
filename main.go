package main

import (
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"twitch-caster/auth"
	"twitch-caster/config"
	"twitch-caster/endpoints"
)

func main() {
	config := config.Load()

	authManager := auth.NewManager(config.Settings)
	tokenMonitor := endpoints.NewTokenMonitor()
	tokenMonitor.StartDailyCheck()
	twitchEndpoint := endpoints.NewTwitchEndpoint(config, authManager, tokenMonitor)
	authEndpoint := endpoints.NewAuthEndpoint(config, authManager)
	adminEndpoint := endpoints.NewAdminEndpoint(tokenMonitor)
	pwaEndpoint := endpoints.NewPWAEndpoint(config.Settings.ChannelListURL)
	session := endpoints.NewSessionAuth(config.Settings.AdminPassword)

	// Endpoints the Chromecast receiver fetches stay unauthenticated; the
	// browser-facing GUI/admin routes sit behind the session cookie.
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/receiver", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Receiver page loaded from %s", r.RemoteAddr)
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, "static/receiver.html")
	})
	http.HandleFunc("/manifest.webmanifest", pwaEndpoint.Manifest)
	http.HandleFunc("/current-stream/", twitchEndpoint.CurrentStream)
	http.HandleFunc("/receiver-heartbeat/", twitchEndpoint.ReceiverHeartbeat)
	http.HandleFunc("/receiver-session/", twitchEndpoint.ReceiverSession)
	http.HandleFunc("/login", session.Login)
	http.HandleFunc("/admin/streamlink-token", session.Protect(adminEndpoint.StreamlinkTokenPage))
	http.HandleFunc("/auth/twitch", session.Protect(authEndpoint.OAuthRedirect))
	http.HandleFunc("/oauth", authEndpoint.OAuthCallback)
	http.HandleFunc(config.Settings.ChannelListURL, session.Protect(twitchEndpoint.TwitchChannelList))
	http.HandleFunc("/gui/search", session.Protect(twitchEndpoint.SearchChannels))
	http.HandleFunc(config.Settings.CastURL, session.Protect(twitchEndpoint.CastTwitch))
	http.HandleFunc("/stop-cast/", twitchEndpoint.StopCast)
	http.HandleFunc("/stop-cast", twitchEndpoint.StopCast)
	http.HandleFunc("/stream-info/", twitchEndpoint.StreamInfo)
	http.HandleFunc("/stream/", twitchEndpoint.StreamProxy)

	// Serve ffmpeg-generated HLS files for mpegTS devices (Google TV).
	// Register MIME types so the Cast receiver sees the right Content-Type.
	mime.AddExtensionType(".m3u8", "application/x-mpegURL")
	mime.AddExtensionType(".ts", "video/mp2t")
	hlsRoot := filepath.Join(os.TempDir(), "tc-hls")
	os.RemoveAll(hlsRoot)
	os.MkdirAll(hlsRoot, 0755)
	http.Handle("/hls-files/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/hls-files/"), "/", 2)
		switch filepath.Ext(r.URL.Path) {
		case ".m3u8":
			w.Header().Set("Content-Type", "application/x-mpegURL")
		case ".ts":
			w.Header().Set("Content-Type", "video/mp2t")
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-cache, no-store")
		cw := &captureStatus{ResponseWriter: w}
		http.StripPrefix("/hls-files/", http.FileServer(http.Dir(hlsRoot))).ServeHTTP(cw, r)
		if cw.status == http.StatusNotFound {
			log.Printf("HLS 404: %s", r.URL.Path)
		} else if len(parts) > 0 && parts[0] != "" {
			// Record access so the watchdog knows the receiver is still active.
			// Only count successful responses — 404s come from stale receivers
			// after a stream ends and would keep the watchdog alive for nothing.
			endpoints.RecordHLSAccess(parts[0])
		}
	}))

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", config.Settings.Port),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /stream/ responses are long-lived live streams.
	}

	// Kill streamlink/ffmpeg children on shutdown so they don't outlive us.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received %s, stopping active proxies", sig)
		endpoints.ShutdownProxies()
		os.Exit(0)
	}()

	log.Printf("Listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

type captureStatus struct {
	http.ResponseWriter
	status int
}

func (c *captureStatus) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}
