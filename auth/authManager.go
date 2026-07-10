package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"twitch-caster/models"
)

const tokenURL = "https://id.twitch.tv/oauth2/token"

// expirySafetyMargin is subtracted from the app token lifetime so we refresh
// slightly before Twitch actually rejects it.
const expirySafetyMargin = 60 * time.Second

var tokenClient = &http.Client{Timeout: 10 * time.Second}

type authResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"` // seconds
	Message     string `json:"message"`
}

// Manager handles authentication for Twitch endpoints. A single instance
// should be shared by everything that makes Twitch API calls so the user
// token obtained via the OAuth flow is visible to all of them.
type Manager struct {
	settings models.Settings

	mu             sync.Mutex
	userToken      string // OAuth user token; in memory only, reset on restart
	appToken       string // client-credentials app token
	appTokenExpiry time.Time
}

// NewManager creates a new Manager object
func NewManager(settings models.Settings) *Manager {
	return &Manager{settings: settings}
}

// StoreUserToken saves a user access token obtained via the OAuth flow.
func (a *Manager) StoreUserToken(token string) {
	a.mu.Lock()
	a.userToken = token
	a.mu.Unlock()
}

// ClearUserToken drops the stored user token. Called when Twitch rejects it
// (expired/revoked) so the GUI redirects back into the OAuth flow instead of
// failing until restart.
func (a *Manager) ClearUserToken() {
	a.mu.Lock()
	cleared := a.userToken != ""
	a.userToken = ""
	a.mu.Unlock()
	if cleared {
		log.Println("User OAuth token rejected by Twitch, cleared — next GUI visit will re-authenticate")
	}
}

// HasUserToken reports whether a user access token has been obtained via the OAuth flow.
func (a *Manager) HasUserToken() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.userToken != ""
}

// GetToken returns a bearer token for Twitch API requests. It prefers a
// dynamically obtained OAuth token and falls back to a client credentials app
// token, fetching a new one when the cached token is missing or expired.
func (a *Manager) GetToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.userToken != "" {
		return a.userToken, nil
	}

	if a.appToken != "" && time.Now().Before(a.appTokenExpiry) {
		return a.appToken, nil
	}

	token, expiresIn, err := a.fetchAppToken()
	if err != nil {
		return "", err
	}

	a.appToken = token
	a.appTokenExpiry = time.Now().Add(time.Duration(expiresIn)*time.Second - expirySafetyMargin)
	return token, nil
}

func (a *Manager) fetchAppToken() (token string, expiresIn int64, err error) {
	authURL := tokenURL + "?client_id=" + a.settings.TwitchClientID +
		"&client_secret=" + a.settings.TwitchSecret + "&grant_type=client_credentials"

	res, err := tokenClient.Post(authURL, "", nil)
	if err != nil {
		return "", 0, fmt.Errorf("requesting app token: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", 0, errors.New("error reading auth response")
	}

	var resp authResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", 0, fmt.Errorf("parsing auth response JSON: %w", err)
	}
	if resp.AccessToken == "" {
		return "", 0, fmt.Errorf("no access token in auth response (status %d): %s", res.StatusCode, resp.Message)
	}

	return resp.AccessToken, resp.ExpiresIn, nil
}
