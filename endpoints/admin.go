package endpoints

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func streamlinkConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "streamlink", "config.twitch"), nil
}

func StreamlinkTokenPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		updateStreamlinkToken(w, r)
		return
	}

	configPath, err := streamlinkConfigPath()
	configured := err == nil
	if configured {
		_, err = os.Stat(configPath)
		configured = err == nil
	}

	updated := r.URL.Query().Get("updated") == "1"

	status := `<span style="color:#e44">Not configured</span>`
	if configured {
		status = `<span style="color:#4e4">Token configured</span>`
	}
	banner := ""
	if updated {
		banner = `<p style="color:#4e4;margin-bottom:1em">Token updated successfully. Next cast will use the new token.</p>`
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
</html>`, status, banner)
}

func updateStreamlinkToken(w http.ResponseWriter, r *http.Request) {
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

	fmt.Printf("[admin] streamlink token updated\n")
	http.Redirect(w, r, "/admin/streamlink-token?updated=1", http.StatusSeeOther)
}
