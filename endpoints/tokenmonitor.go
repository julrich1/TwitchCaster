package endpoints

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const twitchValidateURL = "https://id.twitch.tv/oauth2/validate"

// TokenState describes the last known health of the streamlink auth token.
type TokenState string

const (
	TokenUnknown TokenState = "unknown" // no check has completed yet
	TokenValid   TokenState = "valid"
	TokenInvalid TokenState = "invalid" // Twitch rejected it (401)
	TokenMissing TokenState = "missing" // no config file / no token line
)

var validateClient = &http.Client{Timeout: 10 * time.Second}

// TokenMonitor tracks whether the streamlink auth-token (the twitch.tv
// browser session token in ~/.config/streamlink/config.twitch) is still
// accepted by Twitch. A dead token doesn't break streamlink — playback
// silently loses Turbo ad-free and enhanced streams — so this is the only
// signal the user gets.
type TokenMonitor struct {
	mu        sync.Mutex
	state     TokenState
	login     string // Twitch login the token belongs to, when valid
	checkedAt time.Time
	lastError string // network/transport problem from the most recent check, if any
}

// TokenStatus is a point-in-time snapshot for handlers and templates.
type TokenStatus struct {
	State     TokenState
	Login     string
	CheckedAt time.Time
	LastError string
}

// NewTokenMonitor creates a monitor in the unknown state; call CheckNow or
// StartDailyCheck to populate it.
func NewTokenMonitor() *TokenMonitor {
	return &TokenMonitor{state: TokenUnknown}
}

// Status returns a snapshot of the current token health.
func (m *TokenMonitor) Status() TokenStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return TokenStatus{State: m.state, Login: m.login, CheckedAt: m.checkedAt, LastError: m.lastError}
}

// CheckNow validates the configured token against Twitch's official
// /oauth2/validate endpoint and updates the status. Network failures keep the
// previous valid/invalid verdict (no false-alarm banners) and only record the
// error.
func (m *TokenMonitor) CheckNow() TokenStatus {
	token, err := readStreamlinkToken()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.checkedAt = time.Now()
	m.lastError = ""

	if err != nil || token == "" {
		m.state = TokenMissing
		m.login = ""
		log.Printf("[token-monitor] no streamlink token configured")
		return m.snapshotLocked()
	}

	req, err := http.NewRequest(http.MethodGet, twitchValidateURL, nil)
	if err != nil {
		m.lastError = err.Error()
		return m.snapshotLocked()
	}
	req.Header.Set("Authorization", "OAuth "+token)

	res, err := validateClient.Do(req)
	if err != nil {
		// Can't reach Twitch — keep the previous verdict rather than alarm.
		m.lastError = err.Error()
		log.Printf("[token-monitor] validation check failed (keeping previous state %q): %v", m.state, err)
		return m.snapshotLocked()
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode == http.StatusOK:
		var body struct {
			Login string `json:"login"`
		}
		json.NewDecoder(res.Body).Decode(&body)
		m.state = TokenValid
		m.login = body.Login
		log.Printf("[token-monitor] token valid (logged in as %s)", body.Login)
	case res.StatusCode == http.StatusUnauthorized:
		m.state = TokenInvalid
		m.login = ""
		log.Printf("[token-monitor] token INVALID — Twitch rejected it; update it at /admin/streamlink-token")
	default:
		m.lastError = res.Status
		log.Printf("[token-monitor] unexpected validation response %s (keeping previous state %q)", res.Status, m.state)
	}
	return m.snapshotLocked()
}

// snapshotLocked returns a status copy; m.mu must be held.
func (m *TokenMonitor) snapshotLocked() TokenStatus {
	return TokenStatus{State: m.state, Login: m.login, CheckedAt: m.checkedAt, LastError: m.lastError}
}

// StartDailyCheck runs an immediate check, then one every midnight (local
// time). Recomputing the next midnight each cycle keeps it correct across DST
// changes.
func (m *TokenMonitor) StartDailyCheck() {
	go func() {
		m.CheckNow()
		for {
			now := time.Now()
			nextMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
			log.Printf("[token-monitor] next check at %s", nextMidnight.Format("2006-01-02 15:04"))
			time.Sleep(time.Until(nextMidnight))
			m.CheckNow()
		}
	}()
}

// readStreamlinkToken extracts the token from the streamlink config written
// by the admin page (twitch-api-header=Authorization=OAuth <token>).
func readStreamlinkToken() (string, error) {
	configPath, err := streamlinkConfigPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	const prefix = "twitch-api-header=Authorization=OAuth "
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)), nil
		}
	}
	return "", nil
}
