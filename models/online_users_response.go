package models

import (
	"strconv"
	"strings"
)

// OnlineUsersResponse is the object that twitch responds with for streams/followed
type OnlineUsersResponse struct {
	Data []struct {
		UserID       string `json:"user_id"`
		UserLogin    string `json:"user_login"`
		UserName     string `json:"user_name"`
		GameID       string `json:"game_id"`
		GameName     string `json:"game_name"`
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnail_url"`
		ViewerCount  int    `json:"viewer_count"`
		StartedAt    string `json:"started_at"`
	} `json:"data"`
}

// MakeOnlineStreamers converts an OnlineUsersResponse into a slice of OnlineStreamers
func (onlineUsersResponse OnlineUsersResponse) MakeOnlineStreamers(streamerIDToThumbnailMap map[string]string) []OnlineStreamer {
	onlineStreamers := make([]OnlineStreamer, 0, len(onlineUsersResponse.Data))
	for _, user := range onlineUsersResponse.Data {
		// Cards are 300-400px wide, so 640x360 is already oversampled — the old
		// 1200x674 was several times the bytes for no visible gain on a phone.
		thumbnailURL := strings.Replace(user.ThumbnailURL, "{width}", "640", -1)
		thumbnailURL = strings.Replace(thumbnailURL, "{height}", "360", -1)
		gameName := user.GameName
		if gameName == "" {
			gameName = "Unknown"
		}

		onlineStreamer := OnlineStreamer{
			Login:           user.UserLogin,
			Name:            user.UserName,
			Game:            gameName,
			ProfileImageURL: streamerIDToThumbnailMap[user.UserID],
			Title:           user.Title,
			ThumbnailURL:    thumbnailURL,
			ViewerCount:     strconv.Itoa(user.ViewerCount),
			StartedAt:       user.StartedAt,
		}
		onlineStreamers = append(onlineStreamers, onlineStreamer)
	}
	return onlineStreamers
}
