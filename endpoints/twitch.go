package endpoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	"twitch-caster/models"
	"twitch-caster/services"
)

func createKeyValuePairs(m map[string]streamLinkStreamInfo) string {
	b := new(bytes.Buffer)
	for key, value := range m {
		fmt.Fprintf(b, "%s=\"%s\"\n", key, value)
	}
	return b.String()
}

// Response object when quality is not specified
type streamLinkFullResponse struct {
	Streams map[string]streamLinkStreamInfo `json:"streams"`
	Plugin  string                          `json:"plugin"`
}

type streamLinkStreamInfo struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Headers struct {
		UserAgent      string `json:"User-Agent"`
		AcceptEncoding string `json:"Accept-Encoding"`
		Accept         string `json:"Accept"`
		Connection     string `json:"Connection"`
		ClientID       string `json:"Client-ID"`
	} `json:"headers"`
}

type castJSONResponse struct {
	Success bool `json:"success"`
}

// TwitchEndpoint contains the endpoints for handling casting and listing the main GUI
type TwitchEndpoint struct {
	chromecasts   []models.Chromecast
	twitchService *services.TwitchService
}

func NewTwitchEndpoint(config models.Configuration) *TwitchEndpoint {
	twitchEndpoint := TwitchEndpoint{}
	twitchEndpoint.chromecasts = config.Chromecasts
	twitchEndpoint.twitchService = services.NewTwitchService(config.Settings)
	return &twitchEndpoint
}

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
		fmt.Println("Error: Chromecast IP Address not found")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	streamURL, err := t.fetchQuality(streamID, quality)
	if err != nil {
		fmt.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	castCmd := exec.Command("cast", "--host", ipAddress, "media", "play", streamURL)
	_, castCommandError := castCmd.Output()

	if castCommandError != nil {
		fmt.Println(castCommandError)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	castJSONResponse := castJSONResponse{true}
	jsonResponse, err := json.Marshal(castJSONResponse)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonResponse)
}

func (t *TwitchEndpoint) fetchQuality(streamID string, quality string) (string, error) {
	streamLinkCmd := exec.Command("streamlink", "twitch.tv/"+streamID, "--http-header=Client-ID=jzkbprff40iqj646a697cyrvl0zt2m6", "--player-passthrough=http,hls,rtmp", "-j")
	output, streamLinkError := streamLinkCmd.Output()

	if streamLinkError != nil {
		return "", streamLinkError
	}

	var streamLinkResponse streamLinkFullResponse
	jsonError := json.Unmarshal(output, &streamLinkResponse)
	if jsonError != nil {
		return "", jsonError
	}

	var stream streamLinkStreamInfo
	var ok bool
	stream, ok = streamLinkResponse.Streams[quality]
	if !ok {
		// Couldn't find the requested quality - falling back
		stream, ok = streamLinkResponse.Streams["480p"]
		if !ok {
			return "", errors.New("Could not find a lower quality stream")
		}
	}
	return stream.URL, nil
}

func (t *TwitchEndpoint) TwitchChannelList(w http.ResponseWriter, r *http.Request) {
	twitchFollowsResponse, error := t.twitchService.FetchTwitchFollows()
	if error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(error)
		return
	}

	onlineUsersResponse, error := t.twitchService.FetchTwitchStreamersStatus(twitchFollowsResponse)
	if error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(error)
		return
	}

	onlineStreamers, error := t.twitchService.FetchGames(onlineUsersResponse)
	if error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(error)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "%s", "<html><head><link rel=\"stylesheet\" type=\"text/css\" href=\"static/style.css\"><link rel=\"icon\" type=\"image/x-icon\" href=\"/gui/static/favicon.ico\"/></head><body>")
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

				element.classList.remove("loadFailure")
				element.classList.remove("loadSuccess")

				http.onreadystatechange = (e) => {
					if (http.readyState === 4 && http.status === 200) {
						if (JSON.parse(http.responseText).success === true) {
							element.classList.add("loadSuccess")
						}
						else {
							element.classList.add("loadFailure")
						}
					}
				}											
				http.send();
			}
		</script>`)
	fmt.Fprintf(w, "%s", "<div class=\"logoContainer\"><img class=\"logo\" src=\"/gui/static/twitch-logo.png\"></div>")

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
				"<div onclick=\"castStreamer('"+user.Name+"', this);\" class='thumbnailContainer'>"+
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
