package endpoints

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
)

const sessionCookieName = "tc_session"
const sessionCookieMaxAge = 365 * 24 * 60 * 60 // 1 year — log in once, stay logged in

// SessionAuth gates browser-facing routes behind a long-lived login cookie.
// The cookie value is derived deterministically from the configured password,
// so sessions survive server restarts and changing the password revokes them.
type SessionAuth struct {
	password    string
	cookieValue string
}

// NewSessionAuth creates a SessionAuth for the given admin password.
func NewSessionAuth(password string) *SessionAuth {
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte("twitchcaster-session-v1"))
	return &SessionAuth{
		password:    password,
		cookieValue: hex.EncodeToString(mac.Sum(nil)),
	}
}

// Protect wraps a handler, serving the login page to requests without a valid
// session cookie. With an empty password (no baseURL configured, LAN-only
// setup) it passes requests straight through.
func (s *SessionAuth) Protect(next http.HandlerFunc) http.HandlerFunc {
	if s.password == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if s.isAuthenticated(r) {
			next(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, loginPageHTML, url.QueryEscape(r.URL.RequestURI()))
	}
}

// Login handles the POST from the login form: verify the password, set the
// session cookie, and bounce back to the page the user originally requested.
func (s *SessionAuth) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.password)) != 1 {
		log.Printf("[auth] failed login attempt from %s", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, loginPageHTML, url.QueryEscape(safeNextPath(r.FormValue("next"))))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.cookieValue,
		Path:     "/",
		MaxAge:   sessionCookieMaxAge,
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	log.Printf("[auth] successful login from %s", r.RemoteAddr)
	http.Redirect(w, r, safeNextPath(r.FormValue("next")), http.StatusSeeOther)
}

func (s *SessionAuth) isAuthenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	return err == nil && hmac.Equal([]byte(cookie.Value), []byte(s.cookieValue))
}

// safeNextPath only allows same-site relative redirect targets, preventing
// the login form from being used as an open redirect.
func safeNextPath(next string) string {
	if decoded, err := url.QueryUnescape(next); err == nil {
		next = decoded
	}
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// requestIsHTTPS reports whether the client connection is HTTPS, either
// directly or via a reverse proxy (nginx sets X-Forwarded-Proto). Used to set
// the Secure cookie flag without breaking plain-HTTP LAN access.
func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

const loginPageHTML = `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Login — TwitchCaster</title>
  <style>
    body { font-family: sans-serif; background: #0e0e14; color: #ccc; max-width: 360px; margin: 100px auto; padding: 0 20px; }
    h1 { color: #fff; margin-bottom: 24px; font-size: 1.4em; }
    label { display: block; margin-bottom: 6px; color: #aaa; font-size: .9em; }
    input[type=password] { width: 100%%; padding: 10px; background: #1a1a28; border: 1px solid #333; border-radius: 4px; color: #fff; font-size: .95em; box-sizing: border-box; }
    button { margin-top: 12px; padding: 10px 24px; background: #6441a5; border: none; border-radius: 4px; color: #fff; font-size: 1em; cursor: pointer; width: 100%%; }
    button:hover { background: #7d5bbe; }
  </style>
</head>
<body>
  <h1>TwitchCaster</h1>
  <form method="POST" action="/login">
    <input type="hidden" name="next" value="%s">
    <label for="password">Password</label>
    <input type="password" id="password" name="password" autofocus autocomplete="current-password">
    <button type="submit">Log in</button>
  </form>
</body>
</html>`
