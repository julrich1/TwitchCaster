package models

import (
	"strings"
	"strconv"
)

type OnlineUsersResponse struct {
	Data []struct {
		UserName     string `json:"user_name"`
		GameID       string `json:"game_id"`
		Title        string `json:"title"`
		ThumbnailURL string `json:"thumbnail_url"`
		ViewerCount  int    `json:"viewer_count"`
	} `json:"data"`
}

func (onlineUsersResponse OnlineUsersResponse) MakeOnlineStreamers(gameIDToNameMap map[string]string) []OnlineStreamer {
	onlineStreamers := make([]OnlineStreamer, 0, len(onlineUsersResponse.Data))
	for _, user := range onlineUsersResponse.Data {
		thumbnailURL := strings.Replace(user.ThumbnailURL, "{width}", "320", -1)
		thumbnailURL = strings.Replace(thumbnailURL, "{height}", "180", -1)
		gameName, ok := gameIDToNameMap[user.GameID]
		if !ok {
			gameName = "Unknown"
		}

		onlineStreamer := OnlineStreamer{
			user.UserName,
			gameName,
			user.Title,
			thumbnailURL,
			strconv.Itoa(user.ViewerCount),
		}
		onlineStreamers = append(onlineStreamers, onlineStreamer)
	}
	return onlineStreamers
}
