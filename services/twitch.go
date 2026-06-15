package services

import (
	"twitch-caster/auth"
	"twitch-caster/models"
)

const followedStreamsURL = "https://api.twitch.tv/helix/streams/followed"
const streamsURL = "https://api.twitch.tv/helix/streams"
const usersURL = "https://api.twitch.tv/helix/users"

var endpoints = map[string]endpoint{
	"TWITCH_FOLLOWED_STREAMS": {"GET", followedStreamsURL},
	"TWITCH_STREAMS":          {"GET", streamsURL},
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

// HasUserToken reports whether a user-scoped access token is available.
func (t *TwitchService) HasUserToken() bool {
	return t.authManager.HasUserToken()
}

// FetchFollowedStreams returns all live streams from channels the user follows.
// Requires a user access token with user:read:follows scope in configuration.
func (t *TwitchService) FetchFollowedStreams() ([]models.OnlineStreamer, error) {
	var followedStreamsResponse models.OnlineUsersResponse
	endpoint := endpoints["TWITCH_FOLLOWED_STREAMS"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	if err := t.appendTwitchAuthHeader(headers); err != nil {
		return nil, err
	}

	queryParameters := map[string][]string{
		"user_id": {t.settings.UserID},
		"first":   {"100"},
	}

	request := Request{endpoint.method, endpoint.url, headers, queryParameters}
	if err := MakeRequest(request, &followedStreamsResponse); err != nil {
		return nil, err
	}

	usersResponse, err := t.fetchUsers(followedStreamsResponse)
	if err != nil {
		return nil, err
	}

	streamerIDToThumbnailMap := make(map[string]string)
	for _, user := range usersResponse.Data {
		streamerIDToThumbnailMap[user.ID] = user.ProfileImageURL
	}

	return followedStreamsResponse.MakeOnlineStreamers(streamerIDToThumbnailMap), nil
}

func (t *TwitchService) fetchUsers(onlineUsers models.OnlineUsersResponse) (models.UsersResponse, error) {
	var usersResponse models.UsersResponse
	endpoint := endpoints["TWITCH_USERS"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	if err := t.appendTwitchAuthHeader(headers); err != nil {
		return usersResponse, err
	}

	queryParameters := map[string][]string{
		"first": {"100"},
		"id":    {},
	}
	for _, user := range onlineUsers.Data {
		queryParameters["id"] = append(queryParameters["id"], user.UserID)
	}

	request := Request{endpoint.method, endpoint.url, headers, queryParameters}
	if err := MakeRequest(request, &usersResponse); err != nil {
		return usersResponse, err
	}

	return usersResponse, nil
}

// FetchStreamByLogin returns the current title, game, and viewer count for a
// single stream identified by the streamer's login name. Returns empty strings
// and zero if the stream is offline or the login is not found.
func (t *TwitchService) FetchStreamByLogin(login string) (title, game string, viewerCount int, err error) {
	var resp models.OnlineUsersResponse
	ep := endpoints["TWITCH_STREAMS"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	if err = t.appendTwitchAuthHeader(headers); err != nil {
		return "", "", 0, err
	}

	req := Request{ep.method, ep.url, headers, map[string][]string{
		"user_login": {login},
		"first":      {"1"},
	}}
	if err = MakeRequest(req, &resp); err != nil {
		return "", "", 0, err
	}
	if len(resp.Data) == 0 {
		return "", "", 0, nil
	}
	d := resp.Data[0]
	return d.Title, d.GameName, d.ViewerCount, nil
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
