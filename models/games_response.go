package models

type GamesResponse struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
}
