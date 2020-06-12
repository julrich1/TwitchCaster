package main

import (
	"net/http"

	"twitch-caster/config"
	"twitch-caster/endpoints"
)

func main() {
	config := config.Load()

	twitchEndpoint := endpoints.NewTwitchEndpoint(config)

	http.Handle("/gui/static/", http.StripPrefix("/gui/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/gui/twitch-channel-list", twitchEndpoint.TwitchChannelList)
	http.HandleFunc("/gui/cast/", twitchEndpoint.CastTwitch)
	http.ListenAndServe(":3010", nil)
}
