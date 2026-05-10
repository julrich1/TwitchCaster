package endpoints

import (
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

const proxyPort = 50505

var (
	streamProcessMu sync.Mutex
	streamProcess   *exec.Cmd
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
func proxyAndCast(streamID, quality, ipAddress string) error {
	// Kill any previous proxy so the port is free for the new stream.
	streamProcessMu.Lock()
	if streamProcess != nil {
		streamProcess.Process.Kill()
		_ = streamProcess.Wait()
		streamProcess = nil
	}
	streamProcessMu.Unlock()

	qualityArg := streamlinkQualityArg(quality)

	// Try to get a direct H264 HLS URL first.
	if hlsURL, err := getStreamURL(streamID, qualityArg, "--twitch-supported-codecs", "h264"); err == nil {
		if cmaf, _ := isCMAFManifest(hlsURL); !cmaf {
			fmt.Printf("Casting %s via direct HLS\n", streamID)
			return cast.URL(hlsURL, ipAddress, "application/x-mpegURL")
		}
	}

	// No H264 stream or the manifest is CMAF — run a local proxy.
	fmt.Printf("No direct H264 HLS for %s, using local proxy\n", streamID)
	cmd, err := startProxy(streamID, qualityArg, nil)
	if err != nil {
		return err
	}

	streamProcessMu.Lock()
	streamProcess = cmd
	streamProcessMu.Unlock()

	localIP, err := getLocalIP()
	if err != nil {
		cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}

	streamURL := fmt.Sprintf("http://%s:%d", localIP, proxyPort)
	fmt.Printf("Casting %s via local proxy %s\n", streamID, streamURL)
	return cast.URL(streamURL, ipAddress, "video/mp4")
}

func getStreamURL(streamID, qualityArg string, extraArgs ...string) (string, error) {
	args := append(append([]string{"--stream-url"}, extraArgs...), "twitch.tv/"+streamID, qualityArg)
	out, err := exec.Command("streamlink", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

func startProxy(streamID, qualityArg string, extraArgs []string) (*exec.Cmd, error) {
	args := append(
		append([]string{}, extraArgs...),
		"--player-external-http",
		fmt.Sprintf("--player-external-http-port=%d", proxyPort),
		"twitch.tv/"+streamID,
		qualityArg,
	)
	cmd := exec.Command("streamlink", args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := waitForPort(proxyPort, 15*time.Second); err != nil {
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
