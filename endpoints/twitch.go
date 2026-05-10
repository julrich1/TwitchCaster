package endpoints

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"twitch-caster/cast"
	"twitch-caster/models"
	"twitch-caster/services"
)

const proxyBasePort = 50505

type streamProxy struct {
	cmd  *exec.Cmd
	port int
}

var (
	streamProxyMu sync.Mutex
	streamProxies = make(map[string]*streamProxy) // keyed by Chromecast IP
)

// TwitchEndpoint contains the endpoints for handling casting and listing the main GUI
type TwitchEndpoint struct {
	chromecasts   []models.Chromecast
	twitchService *services.TwitchService
}

// NewTwitchEndpoint creates a new TwitchEndpoint object
func NewTwitchEndpoint(config models.Configuration) *TwitchEndpoint {
	twitchEndpoint := TwitchEndpoint{}
	twitchEndpoint.chromecasts = config.Chromecasts
	twitchEndpoint.twitchService = services.NewTwitchService(config.Settings)
	return &twitchEndpoint
}

// CastTwitch is the entry point for a cast twitch HTTP request
func (t *TwitchEndpoint) CastTwitch(w http.ResponseWriter, r *http.Request) {
	var pathParams = strings.Split(r.URL.Path, "/")
	var ipAddress = pathParams[len(pathParams)-1]
	var streamID = pathParams[len(pathParams)-2]

	if streamID == "" {
		fmt.Fprintf(w, "Invalid stream ID")
		return
	}

	var quality string
	for _, chromecast := range t.chromecasts {
		if chromecast.IPAddress == ipAddress {
			quality = chromecast.QualityMax
		}
	}

	if quality == "" {
		fmt.Println("Error: Could not determine quality setting for the selected Chromecast device")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	go func() {
		if err := proxyAndCast(streamID, quality, ipAddress); err != nil {
			fmt.Println("Error casting stream:", err)
		}
	}()
}

// proxyAndCast casts a Twitch stream to the Chromecast. For standard H264 HLS
// streams it casts the HLS URL directly (Chromecast handles it natively). For
// CMAF/fMP4 streams it starts a local streamlink HTTP proxy so the Chromecast
// receives a plain video/mp4 byte stream instead of fragmented HLS segments.
// Each Chromecast device gets its own proxy process and port, so multiple
// devices can stream simultaneously.
func proxyAndCast(streamID, quality, ipAddress string) error {
	qualityArg := streamlinkQualityArg(quality)

	// Try to get a direct H264 HLS URL first.
	if hlsURL, quality, err := resolveStream(streamID, qualityArg, "--twitch-supported-codecs", "h264"); err == nil {
		if cmaf, _ := isCMAFManifest(hlsURL); !cmaf {
			killProxy(ipAddress) // release port if this device had a proxy before
			fmt.Printf("Casting %s to %s via direct HLS [%s]\n", streamID, ipAddress, quality)
			return cast.URL(hlsURL, ipAddress, "application/x-mpegURL")
		}
	}

	// No H264 stream or the manifest is CMAF — run a local proxy for this device.
	fmt.Printf("No direct H264 HLS for %s, starting proxy for %s\n", streamID, ipAddress)
	port, err := startDeviceProxy(streamID, qualityArg, ipAddress)
	if err != nil {
		return err
	}

	localIP, err := getLocalIP()
	if err != nil {
		killProxy(ipAddress)
		return err
	}

	streamURL := fmt.Sprintf("http://%s:%d", localIP, port)
	fmt.Printf("Casting %s to %s via proxy %s\n", streamID, ipAddress, streamURL)
	return cast.URL(streamURL, ipAddress, "video/mp4")
}

// startDeviceProxy kills any existing proxy for the device, allocates a free
// port, and starts a new streamlink proxy. It also launches a goroutine that
// removes the proxy from the map when streamlink exits (i.e. when the
// Chromecast stops pulling data).
func startDeviceProxy(streamID, qualityArg, ipAddress string) (int, error) {
	streamProxyMu.Lock()
	defer streamProxyMu.Unlock()

	if existing, ok := streamProxies[ipAddress]; ok {
		existing.cmd.Process.Kill()
		_ = existing.cmd.Wait()
		delete(streamProxies, ipAddress)
	}

	port := allocateProxyPort()
	cmd, err := startProxy(streamID, qualityArg, port)
	if err != nil {
		return 0, err
	}

	streamProxies[ipAddress] = &streamProxy{cmd: cmd, port: port}

	go monitorAndKillProxy(cmd, port, ipAddress)
	go func() {
		cmd.Wait()
		streamProxyMu.Lock()
		if p, ok := streamProxies[ipAddress]; ok && p.cmd == cmd {
			delete(streamProxies, ipAddress)
			fmt.Printf("Proxy for %s exited, port %d released\n", ipAddress, port)
		}
		streamProxyMu.Unlock()
	}()

	return port, nil
}

// monitorAndKillProxy waits for the Chromecast to connect to the proxy port,
// then kills the streamlink process once the connection drops. Streamlink does
// not exit on client disconnect for live streams, so we have to do this ourselves.
func monitorAndKillProxy(cmd *exec.Cmd, port int, ipAddress string) {
	// Wait up to 30s for the Chromecast to establish a connection.
	connected := false
	for i := 0; i < 30; i++ {
		if hasActiveConnection(port) {
			connected = true
			break
		}
		time.Sleep(time.Second)
	}
	if !connected {
		fmt.Printf("No connection on port %d after 30s, killing proxy for %s\n", port, ipAddress)
		cmd.Process.Kill()
		return
	}

	// Poll until the connection drops, with a grace period to ignore brief blips.
	for {
		time.Sleep(5 * time.Second)
		if !hasActiveConnection(port) {
			time.Sleep(10 * time.Second)
			if !hasActiveConnection(port) {
				fmt.Printf("Chromecast disconnected from port %d, killing proxy for %s\n", port, ipAddress)
				cmd.Process.Kill()
				return
			}
		}
	}
}

func hasActiveConnection(port int) bool {
	out, err := exec.Command("ss", "-tn").Output()
	if err != nil {
		return false
	}
	portStr := fmt.Sprintf(":%d", port)
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, portStr) && strings.Contains(line, "ESTAB") {
			return true
		}
	}
	return false
}

func killProxy(ipAddress string) {
	streamProxyMu.Lock()
	defer streamProxyMu.Unlock()
	if proxy, ok := streamProxies[ipAddress]; ok {
		proxy.cmd.Process.Kill()
		_ = proxy.cmd.Wait()
		delete(streamProxies, ipAddress)
	}
}

// allocateProxyPort returns the lowest port in the proxyBasePort range not
// currently in use by an active proxy. Must be called with streamProxyMu held.
func allocateProxyPort() int {
	used := make(map[int]bool)
	for _, p := range streamProxies {
		used[p.port] = true
	}
	for port := proxyBasePort; ; port++ {
		if !used[port] {
			return port
		}
	}
}

// resolveStream uses streamlink --json to get the HLS URL and actual quality
// name (e.g. "1080p60") for the first available entry in the qualityArg
// fallback chain (e.g. "best,worst" or "720p60,720p30,best,worst").
func resolveStream(streamID, qualityArg string, extraArgs ...string) (url, quality string, err error) {
	args := append(append([]string{"--json"}, extraArgs...), "twitch.tv/"+streamID)
	out, err := exec.Command("streamlink", args...).Output()
	if err != nil {
		return "", "", err
	}

	var result struct {
		Streams map[string]struct {
			URL string `json:"url"`
		} `json:"streams"`
	}
	if err = json.Unmarshal(out, &result); err != nil {
		return "", "", err
	}

	aliases := map[string]bool{"best": true, "worst": true, "audio_only": true}

	for _, q := range strings.Split(qualityArg, ",") {
		q = strings.TrimSpace(q)
		s, ok := result.Streams[q]
		if !ok || s.URL == "" {
			continue
		}
		// If q is an alias (best/worst), find the real resolution name.
		canonical := q
		if aliases[q] {
			for name, other := range result.Streams {
				if !aliases[name] && other.URL == s.URL {
					canonical = name
					break
				}
			}
		}
		return s.URL, canonical, nil
	}

	return "", "", fmt.Errorf("no stream available for %s at quality %s", streamID, qualityArg)
}

func parseStreamQuality(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if idx := strings.Index(line, "Opening stream: "); idx >= 0 {
			rest := line[idx+len("Opening stream: "):]
			if fields := strings.Fields(rest); len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return "unknown"
}

func isCMAFManifest(hlsURL string) (bool, error) {
	resp, err := http.Get(hlsURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.Contains(string(body), "#EXT-X-MAP"), nil
}

func startProxy(streamID, qualityArg string, port int) (*exec.Cmd, error) {
	args := []string{
		"--player-external-http",
		fmt.Sprintf("--player-external-http-port=%d", port),
		"twitch.tv/" + streamID,
		qualityArg,
	}
	cmd := exec.Command("streamlink", args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Opening stream: ") {
				fmt.Printf("Proxy for %s selected quality: %s\n", streamID, parseStreamQuality(line))
			}
		}
		io.Copy(io.Discard, stderr)
	}()
	if err := waitForPort(port, 15*time.Second); err != nil {
		cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return cmd, nil
}

// streamlinkQualityArg converts a Chromecast quality preference into a
// streamlink quality string with fallbacks.
func streamlinkQualityArg(quality string) string {
	switch quality {
	case "best":
		return "best,worst"
	case "high":
		return "720p60,720p30,best,worst"
	default:
		return quality + ",best,worst"
	}
}

func waitForPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("streamlink proxy not ready on port %d within %v", port, timeout)
}

func getLocalIP() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String(), nil
			}
		}
	}
	return "", errors.New("no local IP address found")
}

// TwitchChannelList is the entry point for an HTTP channel list request
func (t *TwitchEndpoint) TwitchChannelList(w http.ResponseWriter, r *http.Request) {
	if !t.twitchService.HasUserToken() {
		http.Redirect(w, r, "/auth/twitch", http.StatusFound)
		return
	}

	onlineStreamers, error := t.twitchService.FetchFollowedStreams()
	if error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(error)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "%s", "<html><head><link rel=\"stylesheet\" type=\"text/css\" href=\"/static/style.css\"><link rel=\"icon\" type=\"image/x-icon\" href=\"/static/favicon.ico\"/></head><body>")
	fmt.Fprintf(w, "%s",
		`<script>
		  function manualCast(element) {
				const streamer = document.getElementsByName("sname")[0].value
				castStreamer(streamer, element)
			}
			function castStreamer(streamer, element) {
				const http = new XMLHttpRequest()
				const dropDownElement = document.getElementById("device_selection")
				const ip = dropDownElement.options[dropDownElement.selectedIndex].value
				const url='/gui/cast/' + streamer + '/' + ip
				http.open("GET", url)

				http.onreadystatechange = (e) => {
					if (http.readyState === 4 && http.status === 200) {
					}
				}
				http.send();
			}
		</script>`)
	fmt.Fprintf(w, "%s", "<div class=\"logoContainer\"><img class=\"logo\" src=\"/static/twitch-logo.png\"></div>")

	fmt.Fprintf(w, "%s", "<select id=\"device_selection\">")
	for _, chromecast := range t.chromecasts {
		fmt.Fprintf(w, "<option value=\""+chromecast.IPAddress+"\">"+chromecast.Name+"</option>")
	}
	fmt.Fprintf(w, "</select><br>")

	fmt.Fprintf(w, "%s", "<div class=\"manualContainer\"><input type=\"text\" name=\"sname\"><button onclick=\"manualCast(this);\">Manual Cast</button></div>")
	fmt.Fprintf(w, "%s", "<div class='container'>")
	for _, user := range onlineStreamers {
		fmt.Fprintf(w, "%s",
			"<div class='streamContainer'>"+
				"<div onclick=\"castStreamer('"+user.Login+"', this);\" class='thumbnailContainer'>"+
				"<img src=\""+user.ThumbnailURL+"\" class='thumbnailImage'>"+
				"<div class='viewerCountContainer'><div class='viewerCount'><script>document.write(parseInt("+user.ViewerCount+").toLocaleString()+' viewers')</script></div></div>"+
				"</div>"+
				"<div class='streamDetailsContainer'>"+
				"<div class='profileImageContainer'>"+
				"<img src=\""+user.ProfileImageURL+"\" class='profileImage'>"+
				"</div>"+
				"<div class='textContainer'>"+
				"<h3>"+user.Title+"</h3>"+
				"<h4>"+user.Name+"</h4>"+
				"<h4>"+user.Game+"</h4>"+
				"</div>"+
				"</div>"+
				"</div>")
	}
	fmt.Fprintf(w, "%s", "</div>")
	fmt.Fprintf(w, "%s", "</body></html>")
}
