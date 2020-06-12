package models

// GamesResponse contains Twitch games data
type GamesResponse struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
}
