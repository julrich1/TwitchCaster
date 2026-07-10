package endpoints

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// AdminEndpoint serves the streamlink-token admin page.
type AdminEndpoint struct {
	monitor *TokenMonitor
}

// NewAdminEndpoint creates the admin endpoint backed by the shared token monitor.
func NewAdminEndpoint(monitor *TokenMonitor) *AdminEndpoint {
	return &AdminEndpoint{monitor: monitor}
}

func streamlinkConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "streamlink", "config.twitch"), nil
}

func (a *AdminEndpoint) StreamlinkTokenPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.updateStreamlinkToken(w, r)
		return
	}

	status := a.monitor.Status()
	statusHTML := `<span style="color:#e44">Not configured</span>`
	switch status.State {
	case TokenValid:
		statusHTML = fmt.Sprintf(`<span style="color:#4e4">Valid — logged in as %s</span> <span style="color:#666">(checked %s)</span>`,
			status.Login, status.CheckedAt.Format("Jan 2 15:04"))
	case TokenInvalid:
		statusHTML = fmt.Sprintf(`<span style="color:#e44">Invalid — Twitch rejected the token</span> <span style="color:#666">(checked %s)</span>`,
			status.CheckedAt.Format("Jan 2 15:04"))
	case TokenUnknown:
		statusHTML = `<span style="color:#aa4">Not checked yet</span>`
	}

	banner := ""
	switch {
	case r.URL.Query().Get("updated") != "1":
		// no banner
	case r.URL.Query().Get("valid") == "1":
		banner = fmt.Sprintf(`<p style="color:#4e4;margin-bottom:1em">Token verified — logged in as %s. Next cast will use it.</p>`, status.Login)
	case r.URL.Query().Get("valid") == "0":
		banner = `<p style="color:#e44;margin-bottom:1em">Token saved, but Twitch rejected it — double-check you copied the full auth-token cookie value.</p>`
	default:
		banner = `<p style="color:#aa4;margin-bottom:1em">Token saved, but the verification check could not reach Twitch. It will be re-checked at midnight.</p>`
	}

	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <meta charset="utf-8">
  <title>Streamlink Token — TwitchCaster</title>
  <style>
    body { font-family: sans-serif; background: #0e0e14; color: #ccc; max-width: 560px; margin: 60px auto; padding: 0 20px; }
    h1 { color: #fff; margin-bottom: 4px; }
    .status { margin-bottom: 24px; font-size: .9em; }
    label { display: block; margin-bottom: 6px; color: #aaa; font-size: .9em; }
    input[type=text] { width: 100%%; padding: 10px; background: #1a1a28; border: 1px solid #333; border-radius: 4px; color: #fff; font-family: monospace; font-size: .95em; box-sizing: border-box; }
    button { margin-top: 12px; padding: 10px 24px; background: #6441a5; border: none; border-radius: 4px; color: #fff; font-size: 1em; cursor: pointer; }
    button:hover { background: #7d5bbe; }
    details { margin-top: 28px; font-size: .85em; color: #888; }
    summary { cursor: pointer; color: #aaa; }
    ol { margin-top: 8px; padding-left: 20px; line-height: 1.8; }
    code { background: #1a1a28; padding: 1px 5px; border-radius: 3px; color: #bbb; }
  </style>
</head>
<body>
  <h1>Streamlink Auth Token</h1>
  <div class="status">Status: %s</div>
  %s
  <form method="POST">
    <label for="token">Twitch auth-token cookie value</label>
    <input type="text" id="token" name="token" placeholder="paste token here" autocomplete="off" spellcheck="false">
    <button type="submit">Save token</button>
  </form>
  <details>
    <summary>How to get the auth-token</summary>
    <ol>
      <li>Open <code>twitch.tv</code> in your browser while logged in</li>
      <li>Open DevTools → Application → Cookies → <code>https://www.twitch.tv</code></li>
      <li>Find the cookie named <code>auth-token</code> and copy its value</li>
      <li>Paste it above and click Save</li>
    </ol>
  </details>
</body>
</html>`, statusHTML, banner)
}

func (a *AdminEndpoint) updateStreamlinkToken(w http.ResponseWriter, r *http.Request) {
	// Session cookies are SameSite=Lax, but reject cross-site POSTs explicitly
	// in case an older browser ignores that attribute.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site request rejected", http.StatusForbidden)
		return
	}

	token := strings.TrimSpace(r.FormValue("token"))
	token = strings.TrimPrefix(token, "OAuth ")
	token = strings.TrimPrefix(token, "oauth:")
	token = strings.TrimSpace(token)

	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	configPath, err := streamlinkConfigPath()
	if err != nil {
		http.Error(w, "could not determine config path: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		http.Error(w, "could not create config directory: "+err.Error(), http.StatusInternalServerError)
		return
	}

	content := "twitch-api-header=Authorization=OAuth " + token + "\n"
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		http.Error(w, "could not write config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("[admin] streamlink token updated")

	// Verify the new token right away so a bad paste is caught on the spot.
	valid := "" // unknown (check couldn't reach Twitch)
	switch a.monitor.CheckNow().State {
	case TokenValid:
		valid = "1"
	case TokenInvalid:
		valid = "0"
	}
	http.Redirect(w, r, "/admin/streamlink-token?updated=1&valid="+valid, http.StatusSeeOther)
}
