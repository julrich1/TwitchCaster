package models

type TwitchFollowsResponse struct {
	Data []FollowInfo `json:"data"`
}

type FollowInfo struct {
	ToID   string `json:"to_id"`
	ToName string `json:"to_name"`
}