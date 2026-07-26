package models

// OnlineStreamer is the model used to represent online streamers
type OnlineStreamer struct {
	Login           string
	Name            string
	Game            string
	ProfileImageURL string
	Title           string
	ThumbnailURL    string
	ViewerCount     string
	StartedAt       string // RFC3339 from Twitch; the GUI renders uptime client-side
}
