package endpoints

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"twitch-caster/cast"
	"twitch-caster/models"
	"twitch-caster/services"
)

const proxyBasePort = 50505

type streamProxy struct {
	cmd       *exec.Cmd
	port      int       // 0 for HLS file proxies
	ffmpegCmd *exec.Cmd // non-nil for HLS file proxy mode
	hlsDir    string    // non-empty for HLS file proxy mode
}

var (
	streamProxyMu sync.Mutex
	streamProxies = make(map[string]*streamProxy) // keyed by Chromecast IP

)

// TwitchEndpoint contains the endpoints for handling casting and listing the main GUI
type TwitchEndpoint struct {
	chromecasts   []models.Chromecast
	twitchService *services.TwitchService
	serverPort    int
	serverBaseURL string // external HTTPS base URL, used by custom receivers to avoid CORS
}

// NewTwitchEndpoint creates a new TwitchEndpoint object
func NewTwitchEndpoint(config models.Configuration, serverPort int) *TwitchEndpoint {
	twitchEndpoint := TwitchEndpoint{}
	twitchEndpoint.chromecasts = config.Chromecasts
	twitchEndpoint.twitchService = services.NewTwitchService(config.Settings)
	twitchEndpoint.serverPort = serverPort
	twitchEndpoint.serverBaseURL = config.Settings.BaseURL
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

	var quality, appID, deviceName string
	for _, chromecast := range t.chromecasts {
		if chromecast.IPAddress == ipAddress {
			quality = chromecast.QualityMax
			appID = chromecast.ReceiverAppID
			deviceName = chromecast.Name
		}
	}

	if quality == "" {
		fmt.Printf("Cast request: unknown device %s\n", ipAddress)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	fmt.Printf("Cast request: %s → %s (%s)\n", streamID, deviceName, ipAddress)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	go func() {
		if err := proxyAndCast(streamID, quality, appID, ipAddress, t.serverPort, t.serverBaseURL); err != nil {
			fmt.Printf("Error casting %s to %s: %v\n", streamID, ipAddress, err)
		}
	}()
}

// proxyAndCast casts a Twitch stream to the Chromecast.
//   - Direct H264 HLS (non-CMAF): cast directly, all devices handle this natively.
//   - CMAF, mpegTS device (Google TV): run streamlink | ffmpeg to produce a live
//     TS-based HLS stream on disk; Chromecast fetches it via our file server so
//     ExoPlayer uses HlsMediaSource instead of ProgressiveMediaSource.
//   - CMAF, other devices: streamlink HTTP proxy delivering raw fMP4 as video/mp4.
func proxyAndCast(streamID, quality, appID, ipAddress string, serverPort int, serverBaseURL string) error {
	qualityArg := streamlinkQualityArg(quality)
	customReceiver := appID != "" && appID != "CC1AD845"

	if hlsURL, resolvedQuality, err := resolveStream(streamID, qualityArg, "--twitch-supported-codecs", "h264"); err == nil {
		cmaf, _ := isCMAFManifest(hlsURL)
		// Custom web receivers enforce browser CORS — the raw Twitch CDN URL is
		// cross-origin from the receiver's domain, so always proxy through our
		// server. The Default Media Receiver (ExoPlayer) has no such restriction.
		if !cmaf && !customReceiver {
			killProxy(ipAddress)
			fmt.Printf("[%s] Casting %s via direct HLS [%s]\n", ipAddress, streamID, resolvedQuality)
			return cast.URL(hlsURL, ipAddress, "application/x-mpegURL", "LIVE", appID)
		}
		if err := startDeviceHLSProxy(streamID, qualityArg, ipAddress); err != nil {
			return err
		}
		hlsID := strings.ReplaceAll(ipAddress, ".", "-")
		var castURL string
		if customReceiver && serverBaseURL != "" {
			castURL = fmt.Sprintf("%s/hls-files/%s/index.m3u8", serverBaseURL, hlsID)
		} else {
			localIP, err := getLocalIP()
			if err != nil {
				killProxy(ipAddress)
				return err
			}
			castURL = fmt.Sprintf("http://%s:%d/hls-files/%s/index.m3u8", localIP, serverPort, hlsID)
		}
		fmt.Printf("[%s] Casting %s via HLS file proxy [%s]\n", ipAddress, streamID, resolvedQuality)
		return cast.URL(castURL, ipAddress, "application/x-mpegURL", "LIVE", appID)
	}

	if _, err := startDeviceProxy(streamID, qualityArg, ipAddress); err != nil {
		return err
	}
	localIP, err := getLocalIP()
	if err != nil {
		killProxy(ipAddress)
		return err
	}
	streamURL := fmt.Sprintf("http://%s:%d/stream/%s", localIP, serverPort, ipAddress)
	fmt.Printf("[%s] Casting %s via stream proxy\n", ipAddress, streamID)
	return cast.URL(streamURL, ipAddress, "video/mp4", "BUFFERED", appID)
}

// startDeviceHLSProxy pipes streamlink → ffmpeg to produce a live TS-based HLS
// stream written to /tmp/tc-hls/<device>/. The Chromecast fetches the files
// through our /hls-files/ static server. No CMAF markers in the manifest means
// the Cast receiver accepts it and ExoPlayer uses HlsMediaSource.
func startDeviceHLSProxy(streamID, qualityArg, ipAddress string) error {
	streamProxyMu.Lock()
	killExistingProxy(ipAddress)
	streamProxyMu.Unlock()

	hlsDir := filepath.Join(os.TempDir(), "tc-hls", strings.ReplaceAll(ipAddress, ".", "-"))
	os.RemoveAll(hlsDir)
	if err := os.MkdirAll(hlsDir, 0755); err != nil {
		return fmt.Errorf("create HLS dir: %w", err)
	}
	hlsManifest := filepath.Join(hlsDir, "index.m3u8")

	slCmd := exec.Command("streamlink",
		"--stdout",
		"--hls-segment-stream-data",
		"--hls-live-edge=6",
		"--stream-segment-threads=2",
		"twitch.tv/"+streamID,
		qualityArg,
	)
	ffCmd := exec.Command("ffmpeg",
		"-loglevel", "warning", // global option must precede -i
		"-i", "pipe:0",
		"-c", "copy",
		"-bsf:v", "h264_mp4toannexb", // convert AVCC→Annex B for MPEG-TS container
		"-f", "hls",
		"-hls_segment_type", "mpegts", // force TS segments, no EXT-X-MAP
		"-hls_time", "2",
		"-hls_list_size", "10",
		"-hls_flags", "delete_segments",
		hlsManifest,
	)

	slOut, err := slCmd.StdoutPipe()
	if err != nil {
		os.RemoveAll(hlsDir)
		return err
	}
	ffCmd.Stdin = slOut

	slStderr, err := slCmd.StderrPipe()
	if err != nil {
		os.RemoveAll(hlsDir)
		return err
	}
	ffStderr, err := ffCmd.StderrPipe()
	if err != nil {
		os.RemoveAll(hlsDir)
		return err
	}

	if err := slCmd.Start(); err != nil {
		os.RemoveAll(hlsDir)
		return fmt.Errorf("streamlink start failed: %w", err)
	}
	fmt.Printf("[%s] streamlink started (pid %d): %s\n", ipAddress, slCmd.Process.Pid, strings.Join(slCmd.Args, " "))

	if err := ffCmd.Start(); err != nil {
		slCmd.Process.Kill()
		_ = slCmd.Wait()
		os.RemoveAll(hlsDir)
		return fmt.Errorf("ffmpeg start failed: %w", err)
	}
	fmt.Printf("[%s] ffmpeg started (pid %d): %s\n", ipAddress, ffCmd.Process.Pid, strings.Join(ffCmd.Args, " "))

	go func() {
		scanner := bufio.NewScanner(slStderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Opening stream") || strings.Contains(line, "rror") {
				fmt.Printf("streamlink [%s]: %s\n", ipAddress, line)
			}
		}
	}()
	go func() {
		scanner := bufio.NewScanner(ffStderr)
		for scanner.Scan() {
			fmt.Printf("ffmpeg [%s]: %s\n", ipAddress, scanner.Text())
		}
	}()

	// Wait up to 15s for ffmpeg to write the manifest and at least one segment.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(hlsManifest); statErr == nil {
			if segs, _ := filepath.Glob(filepath.Join(hlsDir, "*.ts")); len(segs) > 0 {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if segs, _ := filepath.Glob(filepath.Join(hlsDir, "*.ts")); len(segs) == 0 {
		slCmd.Process.Kill()
		ffCmd.Process.Kill()
		_ = slCmd.Wait()
		_ = ffCmd.Wait()
		os.RemoveAll(hlsDir)
		return fmt.Errorf("HLS segments not ready within 15s for %s", ipAddress)
	}

	streamProxyMu.Lock()
	streamProxies[ipAddress] = &streamProxy{cmd: slCmd, ffmpegCmd: ffCmd, hlsDir: hlsDir}
	streamProxyMu.Unlock()

	go func() {
		_ = slCmd.Wait()
		ffCmd.Process.Kill()
		_ = ffCmd.Wait()
		streamProxyMu.Lock()
		if p, ok := streamProxies[ipAddress]; ok && p.cmd == slCmd {
			delete(streamProxies, ipAddress)
			os.RemoveAll(hlsDir)
			fmt.Printf("HLS proxy for %s exited\n", ipAddress)
		}
		streamProxyMu.Unlock()
	}()

	return nil
}

// startDeviceProxy kills any existing proxy for the device, allocates a free
// port, and starts a new streamlink proxy. It also launches a goroutine that
// removes the proxy from the map when streamlink exits (i.e. when the
// Chromecast stops pulling data).
func startDeviceProxy(streamID, qualityArg, ipAddress string) (int, error) {
	streamProxyMu.Lock()
	defer streamProxyMu.Unlock()

	killExistingProxy(ipAddress)

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

// StreamProxy forwards the streamlink byte stream to the Chromecast with
// explicit headers. This gives us control over Content-Type, caching, and
// flushing behaviour — important for ExoPlayer-based receivers (Google TV)
// that behave differently from the HTML5-based Cast receiver.
func (t *TwitchEndpoint) StreamProxy(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimRight(r.URL.Path, "/"), "/")
	deviceIP := parts[len(parts)-1]

	fmt.Printf("StreamProxy: %s %s from %s (Range: %q)\n", r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("Range"))

	streamProxyMu.Lock()
	proxy, ok := streamProxies[deviceIP]
	var port int
	if ok {
		port = proxy.port
	}
	streamProxyMu.Unlock()

	if !ok {
		http.Error(w, "no active proxy for device", http.StatusNotFound)
		return
	}

	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		fmt.Sprintf("http://localhost:%d", port), nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	resp, err := http.DefaultClient.Do(upstream)
	if err != nil {
		http.Error(w, "stream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body := resp.Body

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "no-store, no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 65536)
	cumulative := 0
	intervalBytes := 0
	lastLog := time.Now()
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				fmt.Printf("StreamProxy: client %s disconnected after %d MB\n", r.RemoteAddr, cumulative/1024/1024)
				return
			}
			cumulative += n
			intervalBytes += n
			if canFlush {
				flusher.Flush()
			}
		}
		if elapsed := time.Since(lastLog); elapsed >= 5*time.Second {
			fmt.Printf("StreamProxy: %s %.1f KB/s (total %d MB)\n",
				r.RemoteAddr,
				float64(intervalBytes)/elapsed.Seconds()/1024,
				cumulative/1024/1024)
			intervalBytes = 0
			lastLog = time.Now()
		}
		if readErr != nil {
			fmt.Printf("StreamProxy: upstream ended for %s after %d MB: %v\n", r.RemoteAddr, cumulative/1024/1024, readErr)
			return
		}
	}
}

// killExistingProxy kills any running proxy for the device and cleans up.
// Must be called with streamProxyMu held.
func killExistingProxy(ipAddress string) {
	if p, ok := streamProxies[ipAddress]; ok {
		fmt.Printf("[%s] Killing existing proxy (pid %d)\n", ipAddress, p.cmd.Process.Pid)
		p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		if p.ffmpegCmd != nil {
			p.ffmpegCmd.Process.Kill()
			_ = p.ffmpegCmd.Wait()
		}
		if p.hlsDir != "" {
			os.RemoveAll(p.hlsDir)
		}
		delete(streamProxies, ipAddress)
	}
}

func killProxy(ipAddress string) {
	streamProxyMu.Lock()
	defer streamProxyMu.Unlock()
	killExistingProxy(ipAddress)
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
	cmd := exec.Command("streamlink", args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			fmt.Printf("resolveStream stderr: %s\n", strings.TrimSpace(string(exitErr.Stderr)))
		}
		if len(out) > 0 {
			fmt.Printf("resolveStream stdout: %s\n", strings.TrimSpace(string(out)))
		}
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
		"--hls-segment-stream-data", // stream segment bytes as they arrive, no pause at segment boundaries
		"--hls-live-edge=6",         // pre-fetch 6 segments (~12-24s ahead) to absorb any jitter
		"--stream-segment-threads=2",
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
			if strings.Contains(line, "Opening stream") || strings.Contains(line, "rror") {
				fmt.Printf("streamlink [%s]: %s\n", streamID, line)
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
