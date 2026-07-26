package endpoints

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"twitch-caster/auth"
	"twitch-caster/cast"
	"twitch-caster/models"
	"twitch-caster/services"
)

const proxyBasePort = 50505

const (
	// Segments required before publishing the stream to the receiver. The
	// custom receiver applies its own buffer gate before starting playback,
	// so it only needs enough for codec detection and the first appends; the
	// default receiver starts playing immediately, so it keeps a bigger head start.
	customReceiverMinSegments  = 2
	defaultReceiverMinSegments = 5

	// Auto-recovery tuning for unexpected streamlink exits (custom receiver).
	maxRecoveryAttempts = 3
	recoveryRetryDelay  = 2 * time.Second
	healthyRunReset     = 5 * time.Minute // a proxy that ran this long resets the attempt counter

	// Playback-stall detection via the receiver heartbeat. The receiver POSTs
	// its currentTime every few seconds; if the playhead stops advancing while
	// the video is playing (not paused) for longer than playbackStallThreshold
	// AND segments are still fresh, the receiver's MSE player has stalled and we
	// force it to rebuild. A stale segment feed instead means a pipeline problem,
	// which the exit/idle watchdogs already handle — so we leave those alone.
	playbackStallThreshold = 15 * time.Second
	segmentFreshThreshold  = 12 * time.Second
)

// hlsProxyOpts controls startDeviceHLSProxy behavior per caller.
type hlsProxyOpts struct {
	minSegments int
	attempt     int // recovery attempt that started this proxy (0 = fresh cast)
	// onUnexpectedExit is invoked (in a new goroutine) when streamlink dies
	// without being deliberately killed. nil disables recovery.
	onUnexpectedExit func(nextAttempt int)
}

type streamProxy struct {
	cmd       *exec.Cmd
	done      chan struct{} // closed once cmd has been reaped by its Wait owner
	port      int           // 0 for HLS file proxies
	ffmpegCmd *exec.Cmd     // non-nil for HLS file proxy mode
	ffDone    chan struct{} // closed once ffmpegCmd has been reaped
	hlsDir    string        // non-empty for HLS file proxy mode
}

var (
	streamProxyMu sync.Mutex
	streamProxies = make(map[string]*streamProxy) // keyed by Chromecast IP

	hlsAccessMu   sync.Mutex
	hlsLastAccess = make(map[string]time.Time) // keyed by Chromecast IP

	castGenMu sync.Mutex
	castGens  = make(map[string]uint64) // keyed by Chromecast IP

	currentStreamMu sync.Mutex
	currentStreams  = make(map[string]*streamState) // keyed by hlsID

	sessionMu  sync.Mutex
	sessionMap = make(map[string]string) // Cast sessionID → hlsID

	heartbeatMu sync.Mutex
	heartbeats  = make(map[string]*hbState) // keyed by hlsID
)

// hbState tracks receiver playback progress between heartbeats so we can detect
// a frozen playhead. Guarded by heartbeatMu.
type hbState struct {
	seq        uint64    // stream Seq this tracker is following
	lastTime   float64   // last reported video.currentTime
	advancedAt time.Time // last moment currentTime moved forward
}

type streamState struct {
	Seq         uint64 `json:"seq"`
	URL         string `json:"url"`
	Login       string `json:"login"`
	Resolution  string `json:"resolution,omitempty"`
	FPS         string `json:"fps,omitempty"`
	ViewerCount int    `json:"viewerCount,omitempty"`
	Codec       string `json:"codec,omitempty"` // "h264", "av1", "hevc"
}

// streamSeq feeds streamState.Seq. The receiver treats any change as "new
// stream, rebuild the player", which is how recovery forces an MSE reset
// after ffmpeg restarts segment numbering.
var streamSeq atomic.Uint64

func nextStreamSeq() uint64 {
	return streamSeq.Add(1)
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
	castURL       string // configured cast route prefix, handed to the GUI so it isn't hard-coded there
	tokenMonitor  *TokenMonitor
}

// NewTwitchEndpoint creates a new TwitchEndpoint object
func NewTwitchEndpoint(config models.Configuration, authManager *auth.Manager, tokenMonitor *TokenMonitor) *TwitchEndpoint {
	twitchEndpoint := TwitchEndpoint{}
	twitchEndpoint.chromecasts = config.Chromecasts
	twitchEndpoint.twitchService = services.NewTwitchService(config.Settings, authManager)
	twitchEndpoint.serverPort = config.Settings.Port
	twitchEndpoint.serverBaseURL = config.Settings.BaseURL
	twitchEndpoint.castURL = config.Settings.CastURL
	twitchEndpoint.tokenMonitor = tokenMonitor
	return &twitchEndpoint
}

// CastTwitch is the entry point for a cast twitch HTTP request
func (t *TwitchEndpoint) CastTwitch(w http.ResponseWriter, r *http.Request) {
	var pathParams = strings.Split(r.URL.Path, "/")
	if len(pathParams) < 2 {
		http.Error(w, "expected /gui/cast/{streamer}/{deviceIP}", http.StatusBadRequest)
		return
	}
	var ipAddress = pathParams[len(pathParams)-1]
	var streamID = pathParams[len(pathParams)-2]

	if streamID == "" || ipAddress == "" {
		http.Error(w, "expected /gui/cast/{streamer}/{deviceIP}", http.StatusBadRequest)
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
		log.Printf("Cast request: unknown device %s", ipAddress)
		http.Error(w, "unknown device "+ipAddress, http.StatusNotFound)
		return
	}

	log.Printf("Cast request: %s → %s (%s)\n", streamID, deviceName, ipAddress)

	gen := bumpCastGen(ipAddress)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	go func() {
		// Metadata is fetched inside proxyAndCast, concurrently with the
		// proxy/app startup, so it stays off the casting critical path.
		meta := &cast.MediaMeta{Login: streamID}
		if err := t.proxyAndCast(streamID, quality, appID, ipAddress, meta, gen); err != nil {
			log.Printf("Error casting %s to %s: %v\n", streamID, ipAddress, err)
		}
	}()
}

// fetchMetaInto fills title/game/viewer count for the receiver overlay.
// Best-effort: on error the cast proceeds with just the login.
func (t *TwitchEndpoint) fetchMetaInto(meta *cast.MediaMeta, streamID string) {
	if title, game, viewerCount, err := t.twitchService.FetchStreamByLogin(streamID); err == nil && title != "" {
		meta.Title = title
		meta.Game = game
		meta.ViewerCount = viewerCount
	}
}

// proxyAndCast casts a Twitch stream to the Chromecast.
//   - Custom receiver: always HLS file proxy (CORS blocks direct CDN access);
//     proxy startup, Chromecast app launch, and metadata fetch run concurrently.
//   - Default receiver, direct H264 HLS (non-CMAF): cast directly.
//   - Default receiver, CMAF: HLS file proxy (mpegTS repack via ffmpeg).
//   - Fallback: streamlink HTTP proxy delivering raw fMP4 as video/mp4.
func (t *TwitchEndpoint) proxyAndCast(streamID, quality, appID, ipAddress string, meta *cast.MediaMeta, gen uint64) error {
	qualityArg := streamlinkQualityArg(quality)
	customReceiver := appID != "" && appID != "CC1AD845"

	if customReceiver {
		// Pre-compute the cast URL — derived from device IP, no network calls needed.
		hlsID := strings.ReplaceAll(ipAddress, ".", "-")
		var castURL string
		if t.serverBaseURL != "" {
			castURL = fmt.Sprintf("%s/hls-files/%s/index.m3u8", t.serverBaseURL, hlsID)
		} else {
			localIP, err := getLocalIP()
			if err != nil {
				return err
			}
			castURL = fmt.Sprintf("http://%s:%d/hls-files/%s/index.m3u8", localIP, t.serverPort, hlsID)
		}

		// recoverStream restarts the pipeline after an unexpected streamlink
		// exit and republishes the stream with a new Seq so the receiver
		// rebuilds its player. Attempts are consecutive: a proxy that stays up
		// ≥healthyRunReset resets the counter (see the exit watcher).
		var recoverStream func(attempt int)
		recoverStream = func(attempt int) {
			if !isCastGenCurrent(ipAddress, gen) {
				return
			}
			if attempt > maxRecoveryAttempts {
				log.Printf("[%s] giving up on %s after %d recovery attempts", ipAddress, streamID, maxRecoveryAttempts)
				clearStreamState(hlsID)
				return
			}
			log.Printf("[%s] recovering stream %s (attempt %d/%d)", ipAddress, streamID, attempt, maxRecoveryAttempts)
			proxyQuality, err := startDeviceHLSProxy(streamID, qualityArg, ipAddress, gen, hlsProxyOpts{
				minSegments:      customReceiverMinSegments,
				attempt:          attempt,
				onUnexpectedExit: recoverStream,
			})
			if err != nil {
				log.Printf("[%s] recovery attempt %d for %s failed: %v", ipAddress, attempt, streamID, err)
				if !isCastGenCurrent(ipAddress, gen) {
					return
				}
				time.Sleep(recoveryRetryDelay)
				recoverStream(attempt + 1)
				return
			}
			publishStreamState(hlsID, castURL, ipAddress, proxyQuality, meta)
			log.Printf("[%s] stream %s recovered", ipAddress, streamID)
		}

		// Start proxy, Chromecast app launch, and metadata fetch concurrently.
		var proxyErr, launchErr error
		var session *cast.Session
		var proxyQuality string
		var wg sync.WaitGroup
		wg.Add(3)
		go func() {
			defer wg.Done()
			// attempt 0 = fresh cast, so the first recovery is attempt 1/3.
			proxyQuality, proxyErr = startDeviceHLSProxy(streamID, qualityArg, ipAddress, gen, hlsProxyOpts{
				minSegments:      customReceiverMinSegments,
				attempt:          0,
				onUnexpectedExit: recoverStream,
			})
		}()
		go func() {
			defer wg.Done()
			session, launchErr = cast.LaunchApp(ipAddress, appID)
		}()
		go func() {
			defer wg.Done()
			t.fetchMetaInto(meta, streamID)
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
		publishStreamState(hlsID, castURL, ipAddress, proxyQuality, meta)
		if session.SessionID != "" {
			sessionMu.Lock()
			sessionMap[session.SessionID] = hlsID
			sessionMu.Unlock()
		}
		log.Printf("[%s] Casting %s via HLS file proxy (custom receiver, session=%s)\n", ipAddress, streamID, session.SessionID)
		session.Close()
		return nil
	}

	t.fetchMetaInto(meta, streamID)

	if hlsURL, resolvedQuality, err := resolveStream(streamID, qualityArg, "--twitch-supported-codecs", "h264"); err == nil {
		if meta != nil && resolvedQuality != "" {
			meta.Resolution, meta.FPS = parseResolution(resolvedQuality)
		}
		cmaf, _ := isCMAFManifest(hlsURL)
		if !cmaf {
			killProxy(ipAddress)
			log.Printf("[%s] Casting %s via direct HLS [%s]\n", ipAddress, streamID, resolvedQuality)
			return cast.URL(hlsURL, ipAddress, "application/x-mpegURL", "LIVE", appID, meta)
		}
		if _, err := startDeviceHLSProxy(streamID, qualityArg, ipAddress, gen, hlsProxyOpts{minSegments: defaultReceiverMinSegments}); err != nil {
			return err
		}
		localIP, err := getLocalIP()
		if err != nil {
			killProxy(ipAddress)
			return err
		}
		castURL := fmt.Sprintf("http://%s:%d/hls-files/%s/index.m3u8", localIP, t.serverPort, strings.ReplaceAll(ipAddress, ".", "-"))
		log.Printf("[%s] Casting %s via HLS file proxy [%s]\n", ipAddress, streamID, resolvedQuality)
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
	streamURL := fmt.Sprintf("http://%s:%d/stream/%s", localIP, t.serverPort, ipAddress)
	log.Printf("[%s] Casting %s via stream proxy\n", ipAddress, streamID)
	return cast.URL(streamURL, ipAddress, "video/mp4", "BUFFERED", appID, meta)
}

// publishStreamState detects the codec, builds the stream state, and makes it
// visible to the receiver's /current-stream/ poll. Each call gets a fresh Seq,
// which tells the receiver to (re)build its player. Used by the initial cast
// and by recovery.
func publishStreamState(hlsID, castURL, ipAddress, proxyQuality string, meta *cast.MediaMeta) {
	hlsDir := filepath.Join(os.TempDir(), "tc-hls", hlsID)
	detectedCodec := detectVideoCodec(hlsDir)
	log.Printf("[%s] Detected codec: %q", ipAddress, detectedCodec)
	state := &streamState{Seq: nextStreamSeq(), URL: castURL, Codec: detectedCodec}
	if meta != nil {
		if proxyQuality != "" {
			meta.Resolution, meta.FPS = parseResolution(proxyQuality)
		}
		state.Login = meta.Login
		state.Resolution = meta.Resolution
		state.FPS = meta.FPS
		state.ViewerCount = meta.ViewerCount
	}
	currentStreamMu.Lock()
	currentStreams[hlsID] = state
	currentStreamMu.Unlock()
}

// forcePlayerRebuild bumps the published Seq for a device without touching the
// proxy, so the receiver's /current-stream poll sees a "new" stream and rebuilds
// its MSE player against the still-healthy segment feed. Used to recover from a
// receiver-side playback stall detected via the heartbeat.
func forcePlayerRebuild(hlsID string) {
	currentStreamMu.Lock()
	if state, ok := currentStreams[hlsID]; ok {
		state.Seq = nextStreamSeq()
	}
	currentStreamMu.Unlock()
}

// newestSegmentAge returns how long ago the most recent HLS segment for a device
// was written, or a large duration if no segments exist yet. Used to tell a
// receiver-side stall (fresh segments) apart from a pipeline stall (stale ones).
func newestSegmentAge(hlsID string) time.Duration {
	hlsDir := filepath.Join(os.TempDir(), "tc-hls", hlsID)
	segs, _ := filepath.Glob(filepath.Join(hlsDir, "*.m4s"))
	var newest time.Time
	for _, seg := range segs {
		if info, err := os.Stat(seg); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return time.Hour
	}
	return time.Since(newest)
}

// startDeviceHLSProxy pipes streamlink → ffmpeg to produce a live TS-based HLS
// stream written to /tmp/tc-hls/<device>/. The Chromecast fetches the files
// through our /hls-files/ static server. No CMAF markers in the manifest means
// the Cast receiver accepts it and ExoPlayer uses HlsMediaSource.
func startDeviceHLSProxy(streamID, qualityArg, ipAddress string, gen uint64, opts hlsProxyOpts) (resolvedQuality string, err error) {
	proxyStart := time.Now()
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
	log.Printf("[%s] streamlink started (pid %d): %s", ipAddress, slCmd.Process.Pid, strings.Join(slCmd.Args, " "))

	// Each command has exactly one Wait owner; everything else that needs the
	// process reaped (kill paths, exit watchers) blocks on the done channel.
	slDone := make(chan struct{})
	var slExitErr error
	go func() {
		slExitErr = slCmd.Wait()
		close(slDone)
	}()

	if startErr := ffCmd.Start(); startErr != nil {
		slCmd.Process.Kill()
		<-slDone
		os.RemoveAll(hlsDir)
		return "", fmt.Errorf("ffmpeg start failed: %w", startErr)
	}
	log.Printf("[%s] ffmpeg started (pid %d): %s", ipAddress, ffCmd.Process.Pid, strings.Join(ffCmd.Args, " "))

	ffDone := make(chan struct{})
	var ffExitErr error
	go func() {
		ffExitErr = ffCmd.Wait()
		close(ffDone)
	}()

	killBoth := func() {
		slCmd.Process.Kill()
		ffCmd.Process.Kill()
		<-slDone
		<-ffDone
	}

	// Capture the resolved quality from the "Opening stream: 1080p60 (HLS)" log line.
	qualityCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(slStderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Opening stream") || strings.Contains(line, "rror") {
				log.Printf("streamlink [%s]: %s\n", ipAddress, line)
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
			log.Printf("ffmpeg [%s]: %s\n", ipAddress, scanner.Text())
		}
	}()

	// Wait up to 15s for ffmpeg to produce opts.minSegments segments plus the
	// fMP4 init segment (needed for codec detection). The initial streamlink
	// burst (3 pre-fetched Twitch segments) typically delivers these within a
	// few seconds. The custom receiver applies its own buffer gate before
	// starting playback, so publication doesn't need a big head start here.
	deadline := time.Now().Add(15 * time.Second)
	slExited := false
	for time.Now().Before(deadline) {
		select {
		case <-slDone:
			// streamlink died during startup (stream offline, auth failure…).
			// Fail fast instead of burning the rest of the 15s.
			slExited = true
		default:
		}
		if slExited {
			break
		}
		if !isCastGenCurrent(ipAddress, gen) {
			// A newer cast request has taken over; leave the directory alone.
			killBoth()
			return "", fmt.Errorf("cast superseded for %s", ipAddress)
		}
		if _, statErr := os.Stat(hlsManifest); statErr == nil {
			segs, _ := filepath.Glob(filepath.Join(hlsDir, "*.m4s"))
			inits, _ := filepath.Glob(filepath.Join(hlsDir, "init*.mp4"))
			if len(segs) >= opts.minSegments && len(inits) > 0 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if slExited {
		ffCmd.Process.Kill()
		<-ffDone
		if isCastGenCurrent(ipAddress, gen) {
			os.RemoveAll(hlsDir)
		}
		return "", fmt.Errorf("streamlink exited during startup for %s: %v", ipAddress, slExitErr)
	}
	if segs, _ := filepath.Glob(filepath.Join(hlsDir, "*.m4s")); len(segs) == 0 {
		killBoth()
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
	streamProxies[ipAddress] = &streamProxy{cmd: slCmd, done: slDone, ffmpegCmd: ffCmd, ffDone: ffDone, hlsDir: hlsDir}
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
				log.Printf("[%s] HLS proxy idle for 2 minutes, stopping\n", ipAddress)
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
					log.Printf("[%s] WARNING: newest HLS segment is %s old — possible pipeline stall\n", ipAddress, age.Round(time.Second))
				}
			}
		}
	}()

	// ffmpeg watcher: detects ffmpeg dying before streamlink (pipeline stall scenario).
	go func() {
		<-ffDone
		streamProxyMu.Lock()
		p, ok := streamProxies[ipAddress]
		current := ok && p.cmd == slCmd
		streamProxyMu.Unlock()
		if current {
			log.Printf("[%s] ffmpeg exited unexpectedly: %v", ipAddress, ffExitErr)
			slCmd.Process.Kill()
		}
	}()

	go func() {
		<-slDone
		ffCmd.Process.Kill()
		streamProxyMu.Lock()
		p, ok := streamProxies[ipAddress]
		owned := ok && p.cmd == slCmd
		if owned {
			delete(streamProxies, ipAddress)
			os.RemoveAll(hlsDir)
		}
		streamProxyMu.Unlock()
		if !owned {
			// Deliberate kill: killExistingProxy deregistered us and handles
			// all cleanup — no recovery.
			return
		}
		log.Printf("[%s] streamlink exited unexpectedly: %v", ipAddress, slExitErr)
		if opts.onUnexpectedExit != nil && isCastGenCurrent(ipAddress, gen) {
			// Keep the published stream state: the receiver retries the
			// (currently 404ing) manifest while we restart the pipeline.
			nextAttempt := opts.attempt + 1
			if time.Since(proxyStart) >= healthyRunReset {
				nextAttempt = 1
			}
			go opts.onUnexpectedExit(nextAttempt)
			return
		}
		clearStreamState(strings.ReplaceAll(ipAddress, ".", "-"))
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
	cmd, done, err := startProxy(streamID, qualityArg, port)
	if err != nil {
		return 0, err
	}

	streamProxies[ipAddress] = &streamProxy{cmd: cmd, done: done, port: port}

	go monitorAndKillProxy(cmd, port, ipAddress)
	go func() {
		<-done
		streamProxyMu.Lock()
		if p, ok := streamProxies[ipAddress]; ok && p.cmd == cmd {
			delete(streamProxies, ipAddress)
			log.Printf("Proxy for %s exited, port %d released", ipAddress, port)
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
		log.Printf("No connection on port %d after 30s, killing proxy for %s\n", port, ipAddress)
		cmd.Process.Kill()
		return
	}

	// Poll until the connection drops, with a grace period to ignore brief blips.
	for {
		time.Sleep(5 * time.Second)
		if !hasActiveConnection(port) {
			time.Sleep(10 * time.Second)
			if !hasActiveConnection(port) {
				log.Printf("Chromecast disconnected from port %d, killing proxy for %s\n", port, ipAddress)
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

	log.Printf("StreamProxy: %s %s from %s (Range: %q)\n", r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("Range"))

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
				log.Printf("StreamProxy: client %s disconnected after %d MB\n", r.RemoteAddr, cumulative/1024/1024)
				return
			}
			cumulative += n
			intervalBytes += n
			if canFlush {
				flusher.Flush()
			}
		}
		if elapsed := time.Since(lastLog); elapsed >= 5*time.Second {
			log.Printf("StreamProxy: %s %.1f KB/s (total %d MB)\n",
				r.RemoteAddr,
				float64(intervalBytes)/elapsed.Seconds()/1024,
				cumulative/1024/1024)
			intervalBytes = 0
			lastLog = time.Now()
		}
		if readErr != nil {
			log.Printf("StreamProxy: upstream ended for %s after %d MB: %v\n", r.RemoteAddr, cumulative/1024/1024, readErr)
			return
		}
	}
}

// killExistingProxy kills any running proxy for the device and cleans up.
// Must be called with streamProxyMu held. The Wait owner goroutines close the
// done channels without taking streamProxyMu, so blocking on them here is safe.
func killExistingProxy(ipAddress string) {
	if p, ok := streamProxies[ipAddress]; ok {
		log.Printf("[%s] Killing existing proxy (pid %d)", ipAddress, p.cmd.Process.Pid)
		p.cmd.Process.Kill()
		if p.ffmpegCmd != nil {
			p.ffmpegCmd.Process.Kill()
		}
		<-p.done
		if p.ffDone != nil {
			<-p.ffDone
		}
		if p.hlsDir != "" {
			os.RemoveAll(p.hlsDir)
		}
		delete(streamProxies, ipAddress)
		// Clear the current stream state so the receiver gets a 204 and stops
		// its HLS polling loop. Without this the receiver keeps requesting
		// index.m3u8 forever against a directory that no longer exists.
		clearStreamState(strings.ReplaceAll(ipAddress, ".", "-"))
	}
}

// clearStreamState removes the receiver-facing state for a device so its
// /current-stream/ polls return 204, and prunes session-ID mappings pointing
// at it so sessionMap stays bounded by active streams.
func clearStreamState(hlsID string) {
	currentStreamMu.Lock()
	delete(currentStreams, hlsID)
	currentStreamMu.Unlock()
	sessionMu.Lock()
	for sid, id := range sessionMap {
		if id == hlsID {
			delete(sessionMap, sid)
		}
	}
	sessionMu.Unlock()
	heartbeatMu.Lock()
	delete(heartbeats, hlsID)
	heartbeatMu.Unlock()
}

// ShutdownProxies kills every active proxy so no streamlink/ffmpeg children
// outlive the server process. Called from the signal handler on shutdown.
func ShutdownProxies() {
	streamProxyMu.Lock()
	ips := make([]string, 0, len(streamProxies))
	for ip := range streamProxies {
		ips = append(ips, ip)
	}
	streamProxyMu.Unlock()
	for _, ip := range ips {
		killProxy(ip)
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
			log.Printf("resolveStream stderr: %s\n", strings.TrimSpace(string(exitErr.Stderr)))
		}
		if len(out) > 0 {
			log.Printf("resolveStream stdout: %s\n", strings.TrimSpace(string(out)))
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

var manifestClient = &http.Client{Timeout: 10 * time.Second}

func isCMAFManifest(hlsURL string) (bool, error) {
	resp, err := manifestClient.Get(hlsURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.Contains(string(body), "#EXT-X-MAP"), nil
}

func startProxy(streamID, qualityArg string, port int) (*exec.Cmd, chan struct{}, error) {
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
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "Opening stream") || strings.Contains(line, "rror") {
				log.Printf("streamlink [%s]: %s", streamID, line)
			}
		}
		io.Copy(io.Discard, stderr)
	}()
	if err := waitForPort(port, 15*time.Second); err != nil {
		cmd.Process.Kill()
		<-done
		return nil, nil, err
	}
	return cmd, done, nil
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
		log.Printf("Stop cast request (no device ID)\n")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ip := strings.ReplaceAll(hlsID, "-", ".")
	log.Printf("Stop cast request for %s\n", ip)
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

// heartbeatPayload is the playback telemetry the custom receiver POSTs every
// few seconds while a stream is active. The fields below CurrentTime are a
// diagnostic snapshot of the MSE player, logged when a stall is detected to
// explain *why* playback froze — since silent stalls throw no error.
type heartbeatPayload struct {
	Seq         uint64   `json:"seq"`
	CurrentTime float64  `json:"currentTime"`
	Paused      bool     `json:"paused"`
	Errors      []string `json:"errors,omitempty"`
	// GapJumps are diagnostic notes for buffer-gap seeks the receiver performed
	// to skip a timeline hole (not errors — playback recovered on its own).
	GapJumps []string `json:"gapJumps,omitempty"`

	// video.readyState (0=NOTHING 1=METADATA 2=CURRENT 3=FUTURE 4=ENOUGH) and
	// video.networkState (0=EMPTY 1=IDLE 2=LOADING 3=NO_SOURCE).
	ReadyState   int `json:"readyState"`
	NetworkState int `json:"networkState"`
	// BufferAhead is seconds buffered past the playhead; ~0 with the feed alive
	// means starvation. GapAhead means the playhead sits before a hole in the
	// buffered ranges (a dropped/misordered segment). BufferedEnd is the end of
	// the last buffered range (buffer depth).
	BufferAhead float64 `json:"bufferAhead"`
	BufferedEnd float64 `json:"bufferedEnd"`
	GapAhead    bool    `json:"gapAhead"`
	// Append-pipeline health: pending queue length, whether an append is in
	// flight, the MediaSource state ("open"/"ended"/"closed"), and seconds since
	// the last successful append / last freshly-fetched segment (-1 = never).
	QueueLen      int     `json:"queueLen"`
	Appending     bool    `json:"appending"`
	MSReadyState  string  `json:"msReadyState"`
	LastAppendAgo float64 `json:"lastAppendAgo"`
	LastFetchAgo  float64 `json:"lastFetchAgo"`
	VideoError    string  `json:"videoError,omitempty"`
}

// snapshot renders the diagnostic fields as a compact log fragment.
func (hb heartbeatPayload) snapshot() string {
	s := fmt.Sprintf("readyState=%d netState=%d bufferAhead=%.1fs bufferedEnd=%.1fs gapAhead=%v queue=%d appending=%v ms=%s lastAppend=%.1fs lastFetch=%.1fs",
		hb.ReadyState, hb.NetworkState, hb.BufferAhead, hb.BufferedEnd, hb.GapAhead,
		hb.QueueLen, hb.Appending, hb.MSReadyState, hb.LastAppendAgo, hb.LastFetchAgo)
	if hb.VideoError != "" {
		s += " err=" + hb.VideoError
	}
	return s
}

// ReceiverHeartbeat receives periodic playback telemetry from the custom
// receiver. Healthy heartbeats are silent by design; only reported errors are
// logged. A frozen playhead (currentTime not advancing while playing, with the
// segment feed still fresh) means the receiver's MSE player has stalled, so we
// force it to rebuild. Stays unauthenticated — the receiver calls it directly.
func (t *TwitchEndpoint) ReceiverHeartbeat(w http.ResponseWriter, r *http.Request) {
	hlsID := strings.TrimPrefix(r.URL.Path, "/receiver-heartbeat/")
	if hlsID == "" {
		http.Error(w, "missing hlsID", http.StatusBadRequest)
		return
	}
	var hb heartbeatPayload
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)

	ip := strings.ReplaceAll(hlsID, "-", ".")
	for _, e := range hb.Errors {
		if e = strings.TrimSpace(e); e != "" {
			log.Printf("[%s] receiver error: %s", ip, e)
		}
	}
	// Gap jumps are recoveries, not errors — the receiver skipped a hole and kept
	// playing. Logged so we can see how often/large these are without a rebuild.
	for _, g := range hb.GapJumps {
		if g = strings.TrimSpace(g); g != "" {
			log.Printf("[%s] receiver: %s", ip, g)
		}
	}

	checkPlaybackStall(hlsID, ip, hb)
}

// checkPlaybackStall updates the per-device progress tracker from a heartbeat
// and forces a player rebuild when the playhead has been frozen too long while
// segments are still fresh.
func checkPlaybackStall(hlsID, ip string, hb heartbeatPayload) {
	currentStreamMu.Lock()
	state, ok := currentStreams[hlsID]
	currentStreamMu.Unlock()
	if !ok {
		// Nothing casting to this device; drop any stale tracker.
		heartbeatMu.Lock()
		delete(heartbeats, hlsID)
		heartbeatMu.Unlock()
		return
	}

	now := time.Now()
	heartbeatMu.Lock()
	hs := heartbeats[hlsID]
	// (Re)seed on a fresh stream, a rebuild we just triggered (Seq bumped), or
	// while paused — then wait for the next heartbeat before judging progress.
	if hs == nil || hs.seq != hb.Seq || hb.Seq != state.Seq || hb.Paused {
		heartbeats[hlsID] = &hbState{seq: state.Seq, lastTime: hb.CurrentTime, advancedAt: now}
		heartbeatMu.Unlock()
		return
	}
	if hb.CurrentTime > hs.lastTime+0.25 {
		hs.lastTime = hb.CurrentTime
		hs.advancedAt = now
		heartbeatMu.Unlock()
		return
	}
	stalledFor := now.Sub(hs.advancedAt)
	heartbeatMu.Unlock()

	if stalledFor < playbackStallThreshold {
		return
	}
	// A stale segment feed is a pipeline problem, not a receiver-side MSE stall —
	// rebuilding the player wouldn't help, so leave it to the other watchdogs.
	if age := newestSegmentAge(hlsID); age > segmentFreshThreshold {
		log.Printf("[%s] playback frozen %s at t=%.1fs but newest segment is %s old — pipeline stall, not rebuilding receiver — %s",
			ip, stalledFor.Round(time.Second), hb.CurrentTime, age.Round(time.Second), hb.snapshot())
		return
	}
	log.Printf("[%s] playback frozen %s at t=%.1fs with fresh segments — forcing receiver rebuild — %s",
		ip, stalledFor.Round(time.Second), hb.CurrentTime, hb.snapshot())
	forcePlayerRebuild(hlsID)
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
	log.Printf("[receiver-session] %s → %s\n", sessionID, hlsID)
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
	log.Printf("[stream-info] polling %s\n", login)
	title, game, viewerCount, err := t.twitchService.FetchStreamByLogin(login)
	if err != nil {
		http.Error(w, "upstream error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"title": title, "game": game, "viewerCount": viewerCount})
}

//go:embed templates/channel_list.html
var channelListHTML string

var channelListTemplate = template.Must(template.New("channel_list").Parse(channelListHTML))

type channelListStreamer struct {
	models.OnlineStreamer
	ViewerCountDisplay string
}

type channelListData struct {
	Chromecasts []models.Chromecast
	Streamers   []channelListStreamer
	TokenAlert  string // non-empty → warning banner at the top of the page
	CastURL     string // cast route prefix, so static/app.js doesn't hard-code it
}

// TwitchChannelList is the entry point for an HTTP channel list request
func (t *TwitchEndpoint) TwitchChannelList(w http.ResponseWriter, r *http.Request) {
	if !t.twitchService.HasUserToken() {
		http.Redirect(w, r, "/auth/twitch", http.StatusFound)
		return
	}

	onlineStreamers, err := t.twitchService.FetchFollowedStreams()
	if err != nil {
		log.Println("Error fetching followed streams:", err)
		// The 401 handler may have cleared a rejected user token; bounce back
		// into the OAuth flow instead of showing an error.
		if !t.twitchService.HasUserToken() {
			http.Redirect(w, r, "/auth/twitch", http.StatusFound)
			return
		}
		http.Error(w, "failed to fetch followed streams", http.StatusInternalServerError)
		return
	}

	data := channelListData{Chromecasts: t.chromecasts, CastURL: t.castURL}
	if t.tokenMonitor != nil {
		switch t.tokenMonitor.Status().State {
		case TokenInvalid:
			data.TokenAlert = "Streamlink token is invalid — casts will get ads and lose enhanced streams. Update it."
		case TokenMissing:
			data.TokenAlert = "No streamlink token configured — casts will get ads and lose enhanced streams. Set one up."
		}
	}
	// Twitch caches live thumbnails behind a stable URL, so without a
	// cache-buster a refreshed list shows the same stale frames.
	cacheBust := strconv.FormatInt(time.Now().Unix(), 10)
	for _, user := range onlineStreamers {
		if user.ThumbnailURL != "" {
			sep := "?"
			if strings.Contains(user.ThumbnailURL, "?") {
				sep = "&"
			}
			user.ThumbnailURL += sep + "t=" + cacheBust
		}
		data.Streamers = append(data.Streamers, channelListStreamer{
			OnlineStreamer:     user,
			ViewerCountDisplay: formatCount(user.ViewerCount),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := channelListTemplate.Execute(w, data); err != nil {
		log.Println("Error rendering channel list:", err)
	}
}

// formatCount adds thousands separators to a numeric string ("12345" → "12,345").
func formatCount(s string) string {
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead == 0 {
		lead = 3
	}
	b.WriteString(s[:lead])
	for i := lead; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
