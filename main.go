package main

import (
	"net/http"

	"twitch-caster/endpoints"
)

func main() {
	http.Handle("/gui/static/", http.StripPrefix("/gui/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/gui/twitch-channel-list", endpoints.TwitchChannelList)
	http.HandleFunc("/gui/cast/", endpoints.CastTwitch)
	http.ListenAndServe(":3010", nil)
}
