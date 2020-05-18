package endpoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	"twitch-caster/services"
)

const livingRoomChromecastIP = "192.168.86.92"
const kitchenChromecastIP = "192.168.86.57"

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

func CastTwitch(w http.ResponseWriter, r *http.Request) {
	var pathParams = strings.Split(r.URL.Path, "/")
	var ipAddress = pathParams[len(pathParams)-1]
	var streamID = pathParams[len(pathParams)-2]

	if streamID == "" {
		fmt.Fprintf(w, "Invalid stream ID")
		return
	}

	bestQuality := false
	if ipAddress == livingRoomChromecastIP {
		bestQuality = true
	}

	streamURL, err := fetchQuality(streamID, bestQuality)
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

func fetchQuality(streamID string, bestQuality bool) (string, error) {
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

	if bestQuality {
		stream, ok := streamLinkResponse.Streams["best"]
		if !ok {
			return "", errors.New("Error fetching best quality stream")
		}
		fmt.Println("Sending best quality stream")
		return stream.URL, nil
	}

	// Not using best quality so find the next level down
	stream, ok := streamLinkResponse.Streams["720p"]
	if !ok {
		stream, ok := streamLinkResponse.Streams["480p"]
		if !ok {
			return "", errors.New("Could not find a lower quality stream")
		}
		fmt.Println("Sending 480 stream")
		return stream.URL, nil
	}
	fmt.Println("Sending 720 stream")
	return stream.URL, nil
}

func TwitchChannelList(w http.ResponseWriter, r *http.Request) {
	twitchFollowsResponse, error := services.FetchTwitchFollows()
	if error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(error)
		return
	}

	onlineUsersResponse, error := services.FetchTwitchStreamersStatus(twitchFollowsResponse)
	if error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Println(error)
		return
	}

	onlineStreamers, error := services.FetchGames(onlineUsersResponse)
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
	fmt.Fprintf(w, "%s", "<h1>Online Users</h1>")
	fmt.Fprintf(w, "%s", "<select id=\"device_selection\"><option value=\""+livingRoomChromecastIP+"\">Living Room</option><option value=\""+kitchenChromecastIP+"\">Kitchen</option></select><br>")
	fmt.Fprintf(w, "%s", "<input type=\"text\" name=\"sname\"><button onclick=\"manualCast(this);\">Manual Cast</button>")
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
				// "<button onclick=\"castStreamer('"+user.Name+"', this);\">"+user.Name+"</button>"+
				"<h3>"+user.Title+"</h3>"+
				"<h4>"+user.Name+"</h4>"+
				"<h4>"+user.Game+"</h4>"+
				"</div>"+
				// "<p style='margin-bottom: 0px; margin-top: 0px'>"+user.Game+" - Viewers: <script>document.write(parseInt("+user.ViewerCount+").toLocaleString())</script></p>"+
				// "<p style='margin-top: 0px'>"+user.Title+"</p>"+
				"</div>"+
				"</div>")
	}
	fmt.Fprintf(w, "%s", "</div>")
	fmt.Fprintf(w, "%s", "</body></html>")
}
