package services

import (
	"twitch-caster/auth"
	"twitch-caster/models"
)

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

// TwitchService is a struct that has methods related to making Twitch API requests
type TwitchService struct {
	settings    models.Settings
	authManager *auth.Manager
}

// NewTwitchService creates a new TwitchService object
func NewTwitchService(settings models.Settings) *TwitchService {
	twitchService := TwitchService{}
	twitchService.settings = settings
	twitchService.authManager = auth.NewManager(settings)
	return &twitchService
}

// FetchTwitchFollows fetches the followed streamers for a Twitch user
func (t *TwitchService) FetchTwitchFollows() (models.TwitchFollowsResponse, error) {
	var twitchFollowersData models.TwitchFollowsResponse
	var endpoint = endpoints["TWITCH_FOLLOWERS"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	err := t.appendTwitchAuthHeader(headers)
	if err != nil {
		return twitchFollowersData, err
	}

	queryParameters := map[string][]string{"from_id": {t.settings.UserID}, "first": {"100"}}

	request := Request{endpoint.method, endpoint.url, headers, queryParameters}
	err = MakeRequest(request, &twitchFollowersData)

	return twitchFollowersData, err
}

// FetchTwitchStreamersStatus calls the Twitch API to get additional information about streamers
func (t *TwitchService) FetchTwitchStreamersStatus(twitchFollowsResponse models.TwitchFollowsResponse) (models.OnlineUsersResponse, error) {
	var onlineUsersResponse models.OnlineUsersResponse
	var endpoint = endpoints["TWITCH_STREAMERS_STATUS"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	err := t.appendTwitchAuthHeader(headers)
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

// FetchGames calls the Twitch API to get information on games
func (t *TwitchService) FetchGames(onlineUsers models.OnlineUsersResponse) ([]models.OnlineStreamer, error) {
	var gamesResponse models.GamesResponse
	var endpoint = endpoints["TWITCH_GAMES"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	err := t.appendTwitchAuthHeader(headers)
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

	usersResponse, err := t.FetchUsers(onlineUsers)
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

// FetchUsers calls the Twitch API to get detailed user information
func (t *TwitchService) FetchUsers(onlineUsers models.OnlineUsersResponse) (models.UsersResponse, error) {
	var usersResponse models.UsersResponse
	var endpoint = endpoints["TWITCH_USERS"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	err := t.appendTwitchAuthHeader(headers)
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

func (t *TwitchService) appendTwitchAuthHeader(headers map[string]string) error {
	token, authError := t.authManager.GetToken()
	if authError == nil {
		headers["Authorization"] = "Bearer " + token
	}
	return authError
}

func (t *TwitchService) appendCommonHeaders(headers map[string]string) {
	headers["Client-ID"] = t.settings.TwitchClientID
}
