package models

import (
	"strconv"
	"strings"
)

type OnlineUsersResponse struct {
	Data []struct {
		UserID       string `json:"user_id"`
		UserName     string `json:"user_name"`
		GameID       string `json:"game_id"`
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnail_url"`
		ViewerCount  int    `json:"viewer_count"`
	} `json:"data"`
}

func (onlineUsersResponse OnlineUsersResponse) MakeOnlineStreamers(gameIDToNameMap map[string]string, streamerIDToThumbnailMap map[string]string) []OnlineStreamer {
	onlineStreamers := make([]OnlineStreamer, 0, len(onlineUsersResponse.Data))
	for _, user := range onlineUsersResponse.Data {
		thumbnailURL := strings.Replace(user.ThumbnailURL, "{width}", "1200", -1)
		thumbnailURL = strings.Replace(thumbnailURL, "{height}", "674", -1)
		gameName, ok := gameIDToNameMap[user.GameID]
		if !ok {
			gameName = "Unknown"
		}

		onlineStreamer := OnlineStreamer{
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
