package services

import (
	apiKeyProvider "twitch-caster/api-keys"
	"twitch-caster/auth"
	"twitch-caster/models"
)

const twitchUserID = "8095777"
const followedStreamersURL = "https://api.twitch.tv/helix/users/follows"
const streamStatusURL = "https://api.twitch.tv/helix/streams"
const gamesURL = "https://api.twitch.tv/helix/games"
const usersURL = "https://api.twitch.tv/helix/users"

var endpoints = map[string]endpoint{
	"TWITCH_FOLLOWERS":        {"GET", followedStreamersURL},
	"TWITCH_STREAMERS_STATUS": {"GET", streamStatusURL},
	"TWITCH_GAMES":            {"GET", gamesURL},
	"TWITCH_USERS":            {"GET", usersURL},
}

type endpoint struct {
	method string
	url    string
}

func FetchTwitchFollows() (models.TwitchFollowsResponse, error) {
	var twitchFollowersData models.TwitchFollowsResponse
	var endpoint = endpoints["TWITCH_FOLLOWERS"]

	headers := map[string]string{}
	appendCommonHeaders(headers)
	err := appendTwitchAuthHeader(headers)
	if err != nil {
		return twitchFollowersData, err
	}

	queryParameters := map[string][]string{"from_id": {twitchUserID}, "first": {"100"}}

	request := Request{endpoint.method, endpoint.url, headers, queryParameters}
	err = MakeRequest(request, &twitchFollowersData)

	return twitchFollowersData, err
}

func FetchTwitchStreamersStatus(twitchFollowsResponse models.TwitchFollowsResponse) (models.OnlineUsersResponse, error) {
	var onlineUsersResponse models.OnlineUsersResponse
	var endpoint = endpoints["TWITCH_STREAMERS_STATUS"]

	headers := map[string]string{}
	appendCommonHeaders(headers)
	err := appendTwitchAuthHeader(headers)
	if err != nil {
		return onlineUsersResponse, err
	}

	queryParameters := map[string][]string{}
	queryParameters["first"] = []string{"100"}
	queryParameters["user_id"] = []string{}
	for _, element := range twitchFollowsResponse.Data {
		queryParameters["user_id"] = append(queryParameters["user_id"], element.ToID)
	}

	request := Request{endpoint.method, endpoint.url, headers, queryParameters}
	err = MakeRequest(request, &onlineUsersResponse)

	return onlineUsersResponse, err
}

func FetchGames(onlineUsers models.OnlineUsersResponse) ([]models.OnlineStreamer, error) {
	var gamesResponse models.GamesResponse
	var endpoint = endpoints["TWITCH_GAMES"]

	headers := map[string]string{}
	appendCommonHeaders(headers)
	err := appendTwitchAuthHeader(headers)
	if err != nil {
		return []models.OnlineStreamer{}, err
	}

	gamesMap := make(map[string]bool)
	for _, user := range onlineUsers.Data {
		gamesMap[user.GameID] = true
	}

	queryParameters := map[string][]string{}
	queryParameters["first"] = []string{"100"}
	queryParameters["id"] = []string{}
	for _, user := range onlineUsers.Data {
		queryParameters["id"] = append(queryParameters["id"], user.GameID)
	}

	request := Request{endpoint.method, endpoint.url, headers, queryParameters}
	err = MakeRequest(request, &gamesResponse)
	if err != nil {
		return []models.OnlineStreamer{}, err
	}

	usersResponse, err := FetchUsers(onlineUsers)
	if err != nil {
		return []models.OnlineStreamer{}, err
	}

	streamerIDToThumbnailMap := make(map[string]string)
	for _, user := range usersResponse.Data {
		streamerIDToThumbnailMap[user.ID] = user.ProfileImageURL
	}

	gameIDToNameMap := make(map[string]string)
	for _, game := range gamesResponse.Data {
		gameIDToNameMap[game.ID] = game.Name
	}

	return onlineUsers.MakeOnlineStreamers(gameIDToNameMap, streamerIDToThumbnailMap), nil
}

func FetchUsers(onlineUsers models.OnlineUsersResponse) (models.UsersResponse, error) {
	var usersResponse models.UsersResponse
	var endpoint = endpoints["TWITCH_USERS"]

	headers := map[string]string{}
	appendCommonHeaders(headers)
	err := appendTwitchAuthHeader(headers)
	if err != nil {
		return usersResponse, err
	}

	queryParameters := map[string][]string{}
	queryParameters["first"] = []string{"100"}
	queryParameters["id"] = []string{}
	for _, user := range onlineUsers.Data {
		queryParameters["id"] = append(queryParameters["id"], user.UserID)
	}

	request := Request{endpoint.method, endpoint.url, headers, queryParameters}
	err = MakeRequest(request, &usersResponse)
	if err != nil {
		return usersResponse, err
	}

	return usersResponse, nil
}

func appendTwitchAuthHeader(headers map[string]string) error {
	token, authError := auth.GetToken()
	if authError == nil {
		headers["Authorization"] = "Bearer " + token
	}
	return authError
}

func appendCommonHeaders(headers map[string]string) {
	headers["Client-ID"] = apiKeyProvider.TwitchClientID()
}
