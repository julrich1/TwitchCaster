package models

// TwitchFollowsResponse contains the response payload for a Twitch followers request
type TwitchFollowsResponse struct {
	Data []FollowInfo `json:"data"`
}

// FollowInfo is the response data from Twitch
type FollowInfo struct {
	ToID   string `json:"to_id"`
	ToName string `json:"to_name"`
}
