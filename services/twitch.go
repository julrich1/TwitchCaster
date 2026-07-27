package services

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"twitch-caster/auth"
	"twitch-caster/models"
)

const followedStreamsURL = "https://api.twitch.tv/helix/streams/followed"
const streamsURL = "https://api.twitch.tv/helix/streams"
const usersURL = "https://api.twitch.tv/helix/users"
const searchChannelsURL = "https://api.twitch.tv/helix/search/channels"

var endpoints = map[string]endpoint{
	"TWITCH_FOLLOWED_STREAMS": {"GET", followedStreamsURL},
	"TWITCH_STREAMS":          {"GET", streamsURL},
	"TWITCH_USERS":            {"GET", usersURL},
	"TWITCH_SEARCH_CHANNELS":  {"GET", searchChannelsURL},
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

// NewTwitchService creates a new TwitchService object sharing the given auth
// manager, so the OAuth user token is visible to every consumer.
func NewTwitchService(settings models.Settings, authManager *auth.Manager) *TwitchService {
	twitchService := TwitchService{}
	twitchService.settings = settings
	twitchService.authManager = authManager
	return &twitchService
}

// makeAuthedRequest performs the request and, if Twitch rejects our user
// token (401), clears it so the GUI falls back into the OAuth flow instead of
// erroring until restart.
func (t *TwitchService) makeAuthedRequest(request Request, responseObject interface{}) error {
	err := MakeRequest(request, responseObject)
	var reqErr *RequestError
	if errors.As(err, &reqErr) && reqErr.StatusCode == http.StatusUnauthorized {
		t.authManager.ClearUserToken()
	}
	return err
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
	if err := t.makeAuthedRequest(request, &followedStreamsResponse); err != nil {
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
	if err := t.makeAuthedRequest(request, &usersResponse); err != nil {
		return usersResponse, err
	}

	return usersResponse, nil
}

// fetchStreams looks up live streams by login name. Twitch accepts up to 100
// user_login values per call, and silently omits anyone who is offline.
func (t *TwitchService) fetchStreams(logins []string) (models.OnlineUsersResponse, error) {
	var resp models.OnlineUsersResponse
	if len(logins) == 0 {
		return resp, nil
	}
	ep := endpoints["TWITCH_STREAMS"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	if err := t.appendTwitchAuthHeader(headers); err != nil {
		return resp, err
	}

	req := Request{ep.method, ep.url, headers, map[string][]string{
		"user_login": logins,
		"first":      {strconv.Itoa(len(logins))},
	}}
	if err := t.makeAuthedRequest(req, &resp); err != nil {
		return resp, err
	}
	return resp, nil
}

// FetchStreamByLogin returns the current title, game, and viewer count for a
// single stream identified by the streamer's login name. Returns empty strings
// and zero if the stream is offline or the login is not found.
func (t *TwitchService) FetchStreamByLogin(login string) (title, game string, viewerCount int, err error) {
	resp, err := t.fetchStreams([]string{login})
	if err != nil {
		return "", "", 0, err
	}
	if len(resp.Data) == 0 {
		return "", "", 0, nil
	}
	d := resp.Data[0]
	return d.Title, d.GameName, d.ViewerCount, nil
}

// liveStream is what a streams lookup contributes to a search result.
type liveStream struct {
	name        string
	title       string
	game        string
	thumbnail   string
	startedAt   string
	viewerCount int
}

// SearchChannels returns live channels matching a search phrase, ordered by
// Twitch's own relevance ranking.
//
// search/channels reports neither a viewer count nor a frame of the stream, so
// the logins it returns get a second, batched streams lookup to fill the cards
// out. That enrichment is best-effort: if it fails we still show the channels,
// just without a thumbnail or viewer count.
func (t *TwitchService) SearchChannels(query string, limit int) ([]models.OnlineStreamer, error) {
	var searchResponse models.SearchChannelsResponse
	ep := endpoints["TWITCH_SEARCH_CHANNELS"]

	headers := map[string]string{}
	t.appendCommonHeaders(headers)
	if err := t.appendTwitchAuthHeader(headers); err != nil {
		return nil, err
	}

	req := Request{ep.method, ep.url, headers, map[string][]string{
		"query":     {query},
		"live_only": {"true"},
		"first":     {strconv.Itoa(limit)},
	}}
	if err := t.makeAuthedRequest(req, &searchResponse); err != nil {
		return nil, err
	}

	logins := make([]string, 0, len(searchResponse.Data))
	for _, channel := range searchResponse.Data {
		// live_only should have handled this; belt and braces, since an offline
		// channel is not castable.
		if channel.IsLive && channel.BroadcasterLogin != "" {
			logins = append(logins, channel.BroadcasterLogin)
		}
	}

	streamByLogin := make(map[string]liveStream)
	if streams, err := t.fetchStreams(logins); err != nil {
		log.Println("Search enrichment lookup failed, falling back to search data:", err)
	} else {
		for _, s := range streams.Data {
			streamByLogin[strings.ToLower(s.UserLogin)] = liveStream{
				name:        s.UserName,
				title:       s.Title,
				game:        s.GameName,
				thumbnail:   s.ThumbnailURL,
				startedAt:   s.StartedAt,
				viewerCount: s.ViewerCount,
			}
		}
	}

	streamers := make([]models.OnlineStreamer, 0, len(logins))
	for _, channel := range searchResponse.Data {
		if !channel.IsLive || channel.BroadcasterLogin == "" {
			continue
		}
		streamer := models.OnlineStreamer{
			Login:           channel.BroadcasterLogin,
			Name:            channel.DisplayName,
			Game:            models.GameOrUnknown(channel.GameName),
			ProfileImageURL: channel.ThumbnailURL,
			Title:           channel.Title,
			ViewerCount:     "0",
			StartedAt:       channel.StartedAt,
		}
		// The streams endpoint is the fresher of the two, so let it win wherever
		// the two overlap.
		if s, ok := streamByLogin[strings.ToLower(channel.BroadcasterLogin)]; ok {
			if s.name != "" {
				streamer.Name = s.name
			}
			streamer.Title = s.title
			streamer.Game = models.GameOrUnknown(s.game)
			streamer.ThumbnailURL = models.SizeThumbnail(s.thumbnail)
			streamer.ViewerCount = strconv.Itoa(s.viewerCount)
			streamer.StartedAt = s.startedAt
		}
		streamers = append(streamers, streamer)
	}
	return streamers, nil
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
