package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"twitch-caster/models"
)

type authResponse struct {
	AccessToken string        `json:"access_token"`
	ExpiresIn   time.Duration `json:"expires_in"`
}

var storedAuthResponse authResponse
var expiresTime time.Time

// Manager handles authentication for Twitch endpoints
type Manager struct {
	settings models.Settings
}

// NewManager creates a new Manager object
func NewManager(settings models.Settings) *Manager {
	manager := Manager{}
	manager.settings = settings
	return &manager
}

// GetToken fetches a new bearer token used to make Twitch API requests
func (a *Manager) GetToken() (string, error) {
	if isSavedTokenValid() {
		return storedAuthResponse.AccessToken, nil
	}

	authURL := "https://id.twitch.tv/oauth2/token?client_id=" + a.settings.TwitchClientID + "&client_secret=" + a.settings.TwitchSecret + "&grant_type=client_credentials"
	req, _ := http.NewRequest("POST", authURL, nil)

	var authResponse authResponse

	client := http.Client{}

	res, error := client.Do(req)
	if error != nil {
		fmt.Println(error)
		return "", error
	}

	defer res.Body.Close()

	body, error := ioutil.ReadAll(res.Body)
	if error != nil {
		return "", errors.New("Error reading auth response")
	}

	err := json.Unmarshal(body, &authResponse)
	if err != nil {
		log.Fatalln(err)
		return "", errors.New("Error parsing auth response JSON")
	}

	storedAuthResponse = authResponse
	expiresTime = time.Now().Add(authResponse.ExpiresIn * time.Second)

	return authResponse.AccessToken, nil
}

func isSavedTokenValid() bool {
	if storedAuthResponse.AccessToken != "" && expiresTime.After(time.Now()) {
		fmt.Println("Valid token")
		return true
	}
	fmt.Println("Invalid token")
	return false
}
