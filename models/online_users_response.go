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
	} `json:"data"`
}

// MakeOnlineStreamers converts an OnlineUsersResponse into a slice of OnlineStreamers
func (onlineUsersResponse OnlineUsersResponse) MakeOnlineStreamers(streamerIDToThumbnailMap map[string]string) []OnlineStreamer {
	onlineStreamers := make([]OnlineStreamer, 0, len(onlineUsersResponse.Data))
	for _, user := range onlineUsersResponse.Data {
		thumbnailURL := strings.Replace(user.ThumbnailURL, "{width}", "1200", -1)
		thumbnailURL = strings.Replace(thumbnailURL, "{height}", "674", -1)
		gameName := user.GameName
		if gameName == "" {
			gameName = "Unknown"
		}

		onlineStreamer := OnlineStreamer{
			user.UserLogin,
			user.UserName,
			gameName,
			streamerIDToThumbnailMap[user.UserID],
			user.Title,
			thumbnailURL,
			strconv.Itoa(user.ViewerCount),
		}
		onlineStreamers = append(onlineStreamers, onlineStreamer)
	}
	return onlineStreamers
}
