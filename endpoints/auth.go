package endpoints

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"twitch-caster/auth"
	"twitch-caster/models"
)

const twitchAuthorizeURL = "https://id.twitch.tv/oauth2/authorize"
const twitchTokenURL = "https://id.twitch.tv/oauth2/token"
const oauthCallbackPath = "/oauth"
const oauthScope = "user:read:follows"
const oauthStateCookie = "tc_oauth_state"

var oauthClient = &http.Client{Timeout: 10 * time.Second}

// AuthEndpoint handles the Twitch OAuth flow.
type AuthEndpoint struct {
	settings       models.Settings
	authManager    *auth.Manager
	channelListURL string
	callbackURL    string
}

// NewAuthEndpoint creates a new AuthEndpoint using the shared auth manager.
func NewAuthEndpoint(config models.Configuration, authManager *auth.Manager) *AuthEndpoint {
	return &AuthEndpoint{
		settings:       config.Settings,
		authManager:    authManager,
		channelListURL: config.Settings.ChannelListURL,
		callbackURL:    config.Settings.BaseURL + oauthCallbackPath,
	}
}

// OAuthRedirect sends the browser to Twitch's authorization page. The CSRF
// state token travels in a short-lived cookie so concurrent logins don't
// clobber each other.
func (a *AuthEndpoint) OAuthRedirect(w http.ResponseWriter, r *http.Request) {
	state, err := generateState()
	if err != nil {
		http.Error(w, "Failed to generate OAuth state", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     oauthCallbackPath,
		MaxAge:   600,
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})

	params := url.Values{}
	params.Set("client_id", a.settings.TwitchClientID)
	params.Set("redirect_uri", a.callbackURL)
	params.Set("response_type", "code")
	params.Set("scope", oauthScope)
	params.Set("state", state)

	http.Redirect(w, r, twitchAuthorizeURL+"?"+params.Encode(), http.StatusFound)
}

// OAuthCallback handles the redirect back from Twitch, exchanges the code for
// a token, stores it in memory, then sends the browser to the channel list.
func (a *AuthEndpoint) OAuthCallback(w http.ResponseWriter, r *http.Request) {
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		http.Error(w, "Twitch authorization denied: "+errParam, http.StatusUnauthorized)
		return
	}

	state := r.URL.Query().Get("state")
	stateCookie, cookieErr := r.Cookie(oauthStateCookie)
	if state == "" || cookieErr != nil || state != stateCookie.Value {
		http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Path: oauthCallbackPath, MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}

	token, err := a.exchangeCode(code, a.callbackURL)
	if err != nil {
		log.Println("OAuth token exchange error:", err)
		http.Error(w, "Token exchange failed", http.StatusInternalServerError)
		return
	}

	a.authManager.StoreUserToken(token)
	http.Redirect(w, r, a.channelListURL, http.StatusFound)
}

func (a *AuthEndpoint) exchangeCode(code, callbackURI string) (string, error) {
	params := url.Values{}
	params.Set("client_id", a.settings.TwitchClientID)
	params.Set("client_secret", a.settings.TwitchSecret)
	params.Set("code", code)
	params.Set("grant_type", "authorization_code")
	params.Set("redirect_uri", callbackURI)

	resp, err := oauthClient.Post(twitchTokenURL, "application/x-www-form-urlencoded", strings.NewReader(params.Encode()))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", err
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("no access token in response: %s", tokenResp.Message)
	}

	return tokenResp.AccessToken, nil
}

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
