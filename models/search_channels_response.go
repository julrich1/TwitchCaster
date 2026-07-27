package models

// SearchChannelsResponse is the object Twitch responds with for search/channels.
// Note that ThumbnailURL here is the broadcaster's profile image, not a frame of
// the stream, and the endpoint reports no viewer count — both come from a
// follow-up streams lookup.
type SearchChannelsResponse struct {
	Data []struct {
		BroadcasterLogin string `json:"broadcaster_login"`
		DisplayName      string `json:"display_name"`
		GameName         string `json:"game_name"`
		Title            string `json:"title"`
		ThumbnailURL     string `json:"thumbnail_url"`
		IsLive           bool   `json:"is_live"`
		StartedAt        string `json:"started_at"`
	} `json:"data"`
}
