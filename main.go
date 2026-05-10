package main

import (
	"log"
	"net/http"

	"twitch-caster/config"
	"twitch-caster/endpoints"
)

func main() {
	config := config.Load()

	twitchEndpoint := endpoints.NewTwitchEndpoint(config)
	authEndpoint := endpoints.NewAuthEndpoint(config)

	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/auth/twitch", authEndpoint.OAuthRedirect)
	http.HandleFunc("/oauth", authEndpoint.OAuthCallback)
	http.HandleFunc(config.Settings.ChannelListURL, twitchEndpoint.TwitchChannelList)
	http.HandleFunc(config.Settings.CastURL, twitchEndpoint.CastTwitch)
	log.Fatal(http.ListenAndServe(":3010", nil))
}
