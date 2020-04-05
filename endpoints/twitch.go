package endpoints

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"

	"twitch-caster/services"
)

const LR_CHROMECAST_IP = "192.168.86.92"
const KITCHEN_CHROMECAST_IP = "192.168.86.57"

type streamLinkResponse struct {
	URL string `json:"url"`
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
	streamLinkCmd := exec.Command("streamlink", "twitch.tv/"+streamID, "best", "--http-header=Client-ID=jzkbprff40iqj646a697cyrvl0zt2m6", "--player-passthrough=http,hls,rtmp", "-j")
	output, streamLinkError := streamLinkCmd.Output()

	if streamLinkError != nil {
		fmt.Println(streamLinkError)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var streamLinkResponse streamLinkResponse
	jsonError := json.Unmarshal(output, &streamLinkResponse)

	if jsonError != nil {
		fmt.Println(jsonError)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	castCmd := exec.Command("cast", "--host", ipAddress, "media", "play", streamLinkResponse.URL)
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

	onlineStreamers, gamesError := services.FetchGames(onlineUsersResponse)
	if gamesError != nil {
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
	fmt.Fprintf(w, "%s", "<select id=\"device_selection\"><option value=\""+LR_CHROMECAST_IP+"\">Living Room</option><option value=\""+KITCHEN_CHROMECAST_IP+"\">Kitchen</option></select><br>")
	fmt.Fprintf(w, "%s", "<input type=\"text\" name=\"sname\"><button onclick=\"manualCast(this);\">Manual Cast</button>")
	fmt.Fprintf(w, "%s", "<ul>")
	for _, user := range onlineStreamers {
		fmt.Fprintf(w, "%s", "<li style='margin-bottom: 5px; font-size: large'><img src=\""+user.ThumbnailURL+"\"><br><button onclick=\"castStreamer('"+user.Name+"', this);\">"+user.Name+"</button><p style='margin-bottom: 0px; margin-top: 0px'>"+user.Game+" - Viewers: <script>document.write(parseInt("+user.ViewerCount+").toLocaleString())</script></p><p style='margin-top: 0px'>"+user.Title+"</p></li>")
	}
	fmt.Fprintf(w, "%s", "</ul>")
	fmt.Fprintf(w, "%s", "</body></html>")
}