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

	hlsAccessMu   sync.Mutex
	hlsLastAccess = make(map[string]time.Time) // keyed by Chromecast IP

	castGenMu sync.Mutex
	castGens  = make(map[string]uint64) // keyed by Chromecast IP

	currentStreamMu sync.Mutex
	currentStreams   = make(map[string]*streamState) // keyed by hlsID

	sessionMu  sync.Mutex
	sessionMap = make(map[string]string) // Cast sessionID → hlsID
)

type streamState struct {
	Seq         uint64 `json:"seq"`
	URL         string `json:"url"`
	Login       string `json:"login"`
	Resolution  string `json:"resolution,omitempty"`
	FPS         string `json:"fps,omitempty"`
	ViewerCount int    `json:"viewerCount,omitempty"`
	Codec       string `json:"codec,omitempty"` // "h264", "av1", "hevc"
}

func bumpCastGen(ip string) uint64 {
	castGenMu.Lock()
	defer castGenMu.Unlock()
	castGens[ip]++
	return castGens[ip]
}

func isCastGenCurrent(ip string, gen uint64) bool {
	castGenMu.Lock()
	defer castGenMu.Unlock()
	return castGens[ip] == gen
}

// RecordHLSAccess notes that the given hlsID (e.g. "192-168-1-233") was just served.
func RecordHLSAccess(hlsID string) {
	ip := strings.ReplaceAll(hlsID, "-", ".")
	hlsAccessMu.Lock()
	hlsLastAccess[ip] = time.Now()
	hlsAccessMu.Unlock()
}

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

	meta := &cast.MediaMeta{Login: streamID}
	if title, game, viewerCount, err := t.twitchService.FetchStreamByLogin(streamID); err == nil && title != "" {
		meta.Title = title
		meta.Game = game
		meta.ViewerCount = viewerCount
	}

	gen := bumpCastGen(ipAddress)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	go func() {
		if err := proxyAndCast(streamID, quality, appID, ipAddress, t.serverPort, t.serverBaseURL, meta, gen); err != nil {
			fmt.Printf("Error casting %s to %s: %v\n", streamID, ipAddress, err)
		}
	}()
}

// proxyAndCast casts a Twitch stream to the Chromecast.
//   - Custom receiver: always HLS file proxy (CORS blocks direct CDN access);
//     proxy startup and Chromecast app launch run concurrently.
//   - Default receiver, direct H264 HLS (non-CMAF): cast directly.
//   - Default receiver, CMAF: HLS file proxy (mpegTS repack via ffmpeg).
//   - Fallback: streamlink HTTP proxy delivering raw fMP4 as video/mp4.
func proxyAndCast(streamID, quality, appID, ipAddress string, serverPort int, serverBaseURL string, meta *cast.MediaMeta, gen uint64) error {
	qualityArg := streamlinkQualityArg(quality)
	customReceiver := appID != "" && appID != "CC1AD845"

	if customReceiver {
		// Pre-compute the cast URL — derived from device IP, no network calls needed.
		hlsID := strings.ReplaceAll(ipAddress, ".", "-")
		var castURL string
		if serverBaseURL != "" {
			castURL = fmt.Sprintf("%s/hls-files/%s/index.m3u8", serverBaseURL, hlsID)
		} else {
			localIP, err := getLocalIP()
			if err != nil {
				return err
			}
			castURL = fmt.Sprintf("http://%s:%d/hls-files/%s/index.m3u8", localIP, serverPort, hlsID)
		}

		// Start proxy and Chromecast app launch concurrently.
		var proxyErr, launchErr error
		var session *cast.Session
		var proxyQuality string
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			proxyQuality, proxyErr = startDeviceHLSProxy(streamID, qualityArg, ipAddress, gen)
		}()
		go func() {
			defer wg.Done()
			session, launchErr = cast.LaunchApp(ipAddress, appID)
		}()
		wg.Wait()

		if proxyErr != nil {
			if session != nil {
				session.Close()
			}
			return proxyErr
		}
		if launchErr != nil {
			killProxy(ipAddress)
			return launchErr
		}
		// Abort if a newer cast request has already taken over this device.
		if !isCastGenCurrent(ipAddress, gen) {
			session.Close()
			return nil
		}
		if meta != nil && proxyQuality != "" {
			meta.Resolution, meta.FPS = parseResolution(proxyQuality)
		}
		hlsDir := filepath.Join(os.TempDir(), "tc-hls", hlsID)
		detectedCodec := detectVideoCodec(hlsDir)
		fmt.Printf("[%s] Detected codec: %q\n", ipAddress, detectedCodec)
		state := &streamState{Seq: gen, URL: castURL, Codec: detectedCodec}
		if meta != nil {
			state.Login = meta.Login
			state.Resolution = meta.Resolution
			state.FPS = meta.FPS
			state.ViewerCount = meta.ViewerCount
		}
		currentStreamMu.Lock()
		currentStreams[hlsID] = state
		currentStreamMu.Unlock()
		if session.SessionID != "" {
			sessionMu.Lock()
			sessionMap[session.SessionID] = hlsID
			sessionMu.Unlock()
		}
		fmt.Printf("[%s] Casting %s via HLS file proxy (custom receiver, session=%s)\n", ipAddress, streamID, session.SessionID)
		session.Close()
		return nil
	}

	if hlsURL, resolvedQuality, err := resolveStream(streamID, qualityArg, "--twitch-supported-codecs", "h264"); err == nil {
		if meta != nil && resolvedQuality != "" {
			meta.Resolution, meta.FPS = parseResolution(resolvedQuality)
		}
		cmaf, _ := isCMAFManifest(hlsURL)
		if !cmaf {
			killProxy(ipAddress)
			fmt.Printf("[%s] Casting %s via direct HLS [%s]\n", ipAddress, streamID, resolvedQuality)
			return cast.URL(hlsURL, ipAddress, "application/x-mpegURL", "LIVE", appID, meta)
		}
		if _, err := startDeviceHLSProxy(streamID, qualityArg, ipAddress, gen); err != nil {
			return err
		}
		localIP, err := getLocalIP()
		if err != nil {
			killProxy(ipAddress)
			return err
		}
		castURL := fmt.Sprintf("http://%s:%d/hls-files/%s/index.m3u8", localIP, serverPort, strings.ReplaceAll(ipAddress, ".", "-"))
		fmt.Printf("[%s] Casting %s via HLS file proxy [%s]\n", ipAddress, streamID, resolvedQuality)
		return cast.URL(castURL, ipAddress, "application/x-mpegURL", "LIVE", appID, meta)
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
	return cast.URL(streamURL, ipAddress, "video/mp4", "BUFFERED", appID, meta)
}

// startDeviceHLSProxy pipes streamlink → ffmpeg to produce a live TS-based HLS
// stream written to /tmp/tc-hls/<device>/. The Chromecast fetches the files
// through our /hls-files/ static server. No CMAF markers in the manifest means
// the Cast receiver accepts it and ExoPlayer uses HlsMediaSource.
func startDeviceHLSProxy(streamID, qualityArg, ipAddress string, gen uint64) (resolvedQuality string, err error) {
	streamProxyMu.Lock()
	killExistingProxy(ipAddress)
	streamProxyMu.Unlock()

	hlsDir := filepath.Join(os.TempDir(), "tc-hls", strings.ReplaceAll(ipAddress, ".", "-"))
	os.RemoveAll(hlsDir)
	if err = os.MkdirAll(hlsDir, 0755); err != nil {
		return "", fmt.Errorf("create HLS dir: %w", err)
	}
	hlsManifest := filepath.Join(hlsDir, "index.m3u8")

	slArgs := []string{
		"--stdout",
		"--hls-segment-stream-data",
		"--hls-live-edge=3",
		"--stream-segment-threads=2",
		"--twitch-supported-codecs", "av1,h265,h264",
		"--twitch-access-token-param", "playerType=site",
		"--webbrowser-headless=true", // needed when auth triggers CI token via Chromium
	}
	slArgs = append(slArgs, "twitch.tv/"+streamID, qualityArg)
	slCmd := exec.Command("streamlink", slArgs...)
	ffCmd := exec.Command("ffmpeg",
		"-loglevel", "error", // global option must precede -i
		"-i", "pipe:0",
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc", // convert ADTS→MPEG-4 ASC for MP4 container
		"-f", "hls",
		"-hls_segment_type", "fmp4",
		"-hls_time", "2",
		"-hls_list_size", "60",
		"-hls_flags", "delete_segments",
		hlsManifest,
	)

	slOut, slOutErr := slCmd.StdoutPipe()
	if slOutErr != nil {
		os.RemoveAll(hlsDir)
		return "", slOutErr
	}
	ffCmd.Stdin = slOut

	slStderr, slStderrErr := slCmd.StderrPipe()
	if slStderrErr != nil {
		os.RemoveAll(hlsDir)
		return "", slStderrErr
	}
	ffStderr, ffStderrErr := ffCmd.StderrPipe()
	if ffStderrErr != nil {
		os.RemoveAll(hlsDir)
		return "", ffStderrErr
	}

	if startErr := slCmd.Start(); startErr != nil {
		os.RemoveAll(hlsDir)
		return "", fmt.Errorf("streamlink start failed: %w", startErr)
	}
	fmt.Printf("[%s] streamlink started (pid %d): %s\n", ipAddress, slCmd.Process.Pid, strings.Join(slCmd.Args, " "))

	if startErr := ffCmd.Start(); startErr != nil {
		slCmd.Process.Kill()
		_ = slCmd.Wait()
		os.RemoveAll(hlsDir)
		return "", fmt.Errorf("ffmpeg start failed: %w", startErr)
	}
	fmt.Printf("[%s] ffmpeg started (pid %d): %s\n", ipAddress, ffCmd.Process.Pid, strings.Join(ffCmd.Args, " "))

	// Capture the resolved quality from the "Opening stream: 1080p60 (HLS)" log line.
	qualityCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(slStderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Opening stream") || strings.Contains(line, "rror") {
				fmt.Printf("streamlink [%s]: %s\n", ipAddress, line)
			}
			if q := parseStreamQuality(line); q != "unknown" {
				select {
				case qualityCh <- q:
				default:
				}
			}
		}
	}()
	go func() {
		scanner := bufio.NewScanner(ffStderr)
		for scanner.Scan() {
			fmt.Printf("ffmpeg [%s]: %s\n", ipAddress, scanner.Text())
		}
	}()

	// Wait up to 15s for ffmpeg to produce at least 5 segments (~10s of buffer).
	// The initial streamlink burst (3 pre-fetched Twitch segments) typically
	// delivers these in 2–3s, so startup only increases by ~2s over the 1-segment
	// threshold while eliminating the buffer-starvation stutter at playback start.
	const minSegments = 5
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !isCastGenCurrent(ipAddress, gen) {
			// A newer cast request has taken over; leave the directory alone.
			slCmd.Process.Kill()
			ffCmd.Process.Kill()
			_ = slCmd.Wait()
			_ = ffCmd.Wait()
			return "", fmt.Errorf("cast superseded for %s", ipAddress)
		}
		if _, statErr := os.Stat(hlsManifest); statErr == nil {
			if segs, _ := filepath.Glob(filepath.Join(hlsDir, "*.m4s")); len(segs) >= minSegments {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if segs, _ := filepath.Glob(filepath.Join(hlsDir, "*.m4s")); len(segs) == 0 {
		slCmd.Process.Kill()
		ffCmd.Process.Kill()
		_ = slCmd.Wait()
		_ = ffCmd.Wait()
		// Only remove the directory if we still own it — a newer proxy may have
		// already claimed this path.
		if isCastGenCurrent(ipAddress, gen) {
			os.RemoveAll(hlsDir)
		}
		return "", fmt.Errorf("HLS segments not ready within 15s for %s", ipAddress)
	}

	// Read whatever quality the streamlink stderr goroutine captured (non-blocking).
	select {
	case resolvedQuality = <-qualityCh:
	default:
	}

	streamProxyMu.Lock()
	streamProxies[ipAddress] = &streamProxy{cmd: slCmd, ffmpegCmd: ffCmd, hlsDir: hlsDir}
	streamProxyMu.Unlock()

	// Seed the access time so the watchdog counts from first-segment-ready, not from
	// process start, giving the receiver time to make its first request.
	hlsAccessMu.Lock()
	hlsLastAccess[ipAddress] = time.Now()
	hlsAccessMu.Unlock()

	go func() {
		for {
			time.Sleep(30 * time.Second)
			streamProxyMu.Lock()
			p, ok := streamProxies[ipAddress]
			streamProxyMu.Unlock()
			if !ok || p.cmd != slCmd {
				return // proxy was replaced or killed
			}
			hlsAccessMu.Lock()
			last := hlsLastAccess[ipAddress]
			hlsAccessMu.Unlock()
			if time.Since(last) > 2*time.Minute {
				fmt.Printf("[%s] HLS proxy idle for 2 minutes, stopping\n", ipAddress)
				killProxy(ipAddress)
				return
			}
			segs, _ := filepath.Glob(filepath.Join(hlsDir, "*.m4s"))
			if len(segs) > 0 {
				var newest time.Time
				for _, seg := range segs {
					if info, statErr := os.Stat(seg); statErr == nil && info.ModTime().After(newest) {
						newest = info.ModTime()
					}
				}
				if age := time.Since(newest); age > 15*time.Second {
					fmt.Printf("[%s] WARNING: newest HLS segment is %s old — possible pipeline stall\n", ipAddress, age.Round(time.Second))
				}
			}
		}
	}()

	// ffmpeg watcher: detects ffmpeg dying before streamlink (pipeline stall scenario).
	go func() {
		err := ffCmd.Wait()
		streamProxyMu.Lock()
		p, ok := streamProxies[ipAddress]
		current := ok && p.cmd == slCmd
		streamProxyMu.Unlock()
		if current {
			fmt.Printf("[%s] ffmpeg exited unexpectedly: %v\n", ipAddress, err)
			slCmd.Process.Kill()
		}
	}()

	go func() {
		err := slCmd.Wait()
		ffCmd.Process.Kill()
		// ffmpeg goroutine owns ffCmd.Wait(); don't call it here.
		streamProxyMu.Lock()
		if p, ok := streamProxies[ipAddress]; ok && p.cmd == slCmd {
			delete(streamProxies, ipAddress)
			os.RemoveAll(hlsDir)
			fmt.Printf("[%s] streamlink exited: %v\n", ipAddress, err)
			hlsID := strings.ReplaceAll(ipAddress, ".", "-")
			currentStreamMu.Lock()
			delete(currentStreams, hlsID)
			currentStreamMu.Unlock()
		}
		streamProxyMu.Unlock()
	}()

	return resolvedQuality, nil
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
		// Clear the current stream state so the receiver gets a 204 and stops
		// its HLS polling loop. Without this the receiver keeps requesting
		// index.m3u8 forever against a directory that no longer exists.
		hlsID := strings.ReplaceAll(ipAddress, ".", "-")
		currentStreamMu.Lock()
		delete(currentStreams, hlsID)
		currentStreamMu.Unlock()
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

// parseResolution converts a streamlink quality name (e.g. "1080p60") into
// display-ready resolution and fps strings for the receiver overlay.
func parseResolution(q string) (resolution, fps string) {
	parts := strings.SplitN(q, "p", 2)
	if len(parts) != 2 {
		return "", ""
	}
	heightStr, fpsStr := parts[0], parts[1]
	widths := map[string]string{"2160": "3840", "1440": "2560", "1080": "1920", "720": "1280", "480": "854", "360": "640", "160": "284"}
	w, ok := widths[heightStr]
	if !ok {
		return "", ""
	}
	if fpsStr == "" {
		fpsStr = "30"
	}
	return w + "×" + heightStr, fpsStr + "fps"
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
		"--hls-segment-stream-data",
		"--hls-live-edge=3",
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

// StopCast kills the HLS proxy for a device. The receiver calls
// /stop-cast/{hlsID} (e.g. /stop-cast/192-168-1-233) so the device is
// identified by path rather than request IP, which nginx would mask.
func (t *TwitchEndpoint) StopCast(w http.ResponseWriter, r *http.Request) {
	hlsID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/stop-cast"), "/")
	if hlsID == "" {
		fmt.Printf("Stop cast request (no device ID)\n")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ip := strings.ReplaceAll(hlsID, "-", ".")
	fmt.Printf("Stop cast request for %s\n", ip)
	killProxy(ip)
	w.WriteHeader(http.StatusNoContent)
}

// CurrentStream returns the active stream state for a device as JSON, or 204
// if nothing is currently casting. The native receiver polls this every 2s to
// know when to start (or switch) playback.
func (t *TwitchEndpoint) CurrentStream(w http.ResponseWriter, r *http.Request) {
	hlsID := strings.TrimPrefix(r.URL.Path, "/current-stream/")
	if hlsID == "" {
		http.Error(w, "missing hlsID", http.StatusBadRequest)
		return
	}
	currentStreamMu.Lock()
	state, ok := currentStreams[hlsID]
	currentStreamMu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

// ReceiverSession maps a Cast session ID to the hlsID of its device. The
// native receiver calls this on startup (using the session ID from CAF) to
// discover which device it is without relying on request IP.
func (t *TwitchEndpoint) ReceiverSession(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/receiver-session/")
	if sessionID == "" {
		http.Error(w, "missing sessionID", http.StatusBadRequest)
		return
	}
	sessionMu.Lock()
	hlsID, ok := sessionMap[sessionID]
	sessionMu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	fmt.Printf("[receiver-session] %s → %s\n", sessionID, hlsID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"hlsID": hlsID})
}

// detectVideoCodec runs ffprobe on the fMP4 init segment to return the codec
// name ("h264", "av1", "hevc", …). Returns "" on any error.
func detectVideoCodec(hlsDir string) string {
	inits, _ := filepath.Glob(filepath.Join(hlsDir, "init*.mp4"))
	if len(inits) == 0 {
		return ""
	}
	// codec_tag_string gives us "hev1"/"hvc1"/"avc1"/"av01" — the exact fMP4 box
	// type needed to construct the MSE addSourceBuffer codec string.
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_tag_string",
		"-of", "default=noprint_wrappers=1:nokey=1",
		inits[0],
	).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// StreamInfo returns the current title and game for a streamer as JSON.
// The receiver calls this every 60 seconds to keep the seek-bar metadata fresh.
func (t *TwitchEndpoint) StreamInfo(w http.ResponseWriter, r *http.Request) {
	login := strings.TrimPrefix(r.URL.Path, "/stream-info/")
	if login == "" {
		http.Error(w, "missing login", http.StatusBadRequest)
		return
	}
	fmt.Printf("[stream-info] polling %s\n", login)
	title, game, viewerCount, err := t.twitchService.FetchStreamByLogin(login)
	if err != nil {
		http.Error(w, "upstream error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"title": title, "game": game, "viewerCount": viewerCount})
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
