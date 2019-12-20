package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os/exec"
	"strings"

	apikeys "twitch-caster/api-keys"
)

const CHROMECAST_IP = "192.168.86.92"

const twitchUserID = "8095777"
const followedStreamersURL = "https://api.twitch.tv/helix/users/follows?from_id=" + twitchUserID + "&first=100"
const streamStatusURL = "https://api.twitch.tv/helix/streams"
const gamesURL = "https://api.twitch.tv/helix/games"

type TwitchFollowsResponse struct {
	Data []FollowInfo `json:"data"`
}

type FollowInfo struct {
	ToID   string `json:"to_id"`
	ToName string `json:"to_name"`
}

type OnlineUsersResponse struct {
	Data []struct {
		UserName string `json:"user_name"`
		GameID   string `json:"game_id"`
		Title    string `json:"title"`
	} `json:"data"`
}

type StreamLinkResponse struct {
	URL string `json:"url"`
}

type GamesResponse struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
}

type OnlineStreamer struct {
	Name  string
	Game  string
	Title string
}

type CastJSONResponse struct {
	Success bool `json:"success"`
}

func main() {
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/gui/twitch-channel-list", twitchChannelList)
	http.HandleFunc("/gui/cast/", castTwitch)
	http.ListenAndServe(":3010", nil)
}

func castTwitch(w http.ResponseWriter, r *http.Request) {
	var pathParams = strings.Split(r.URL.Path, "/")
	var streamID = pathParams[len(pathParams)-1]

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

	var streamLinkResponse StreamLinkResponse
	jsonError := json.Unmarshal(output, &streamLinkResponse)

	if jsonError != nil {
		fmt.Println(jsonError)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	castCmd := exec.Command("cast", "--host", CHROMECAST_IP, "media", "play", streamLinkResponse.URL)
	_, castCommandError := castCmd.Output()

	if castCommandError != nil {
		fmt.Println(castCommandError)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	castJSONResponse := CastJSONResponse{true}
	jsonResponse, err := json.Marshal(castJSONResponse)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(jsonResponse)
}

func twitchChannelList(w http.ResponseWriter, r *http.Request) {
	client := http.Client{}

	twitchFollowsResponse, error := fetchTwitchFollows(client)
	if error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	onlineUsersResponse, error := fetchTwitchStreamersStatus(client, twitchFollowsResponse)
	if error != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	onlineStreamers, gamesError := fetchGames(client, onlineUsersResponse)
	if gamesError != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, "<html><head><link rel=\"stylesheet\" type=\"text/css\" href=\"/static/style.css\"></head><body>")
	fmt.Fprintf(w, `<script>
										function castStreamer(streamer, element) {
											const Http = new XMLHttpRequest();
											const url='/gui/cast/' + streamer;
											Http.open("GET", url);

											element.classList.remove("loadFailure");
											element.classList.remove("loadSuccess");

											Http.onreadystatechange = (e) => {
												if (Http.readyState === 4 && Http.status === 200) {
													if (JSON.parse(Http.responseText).success === true) {
														element.classList.add("loadSuccess");
													}
													else {
														element.classList.add("loadFailure");
													}
												}
											}											
											Http.send();
										}
									</script>`)
	fmt.Fprintf(w, "<h1>Online Users</h1>")
	fmt.Fprintf(w, "<ul>")
	for _, user := range onlineStreamers {
		fmt.Fprintf(w, "<li style='margin-bottom: 5px; font-size: large'><div onclick=\"castStreamer('"+user.Name+"', this);\">"+user.Name+"</div><p style='margin-bottom: 0px; margin-top: 0px'>"+user.Game+"</p><p style='margin-top: 0px'>"+user.Title+"</p></li>")
	}
	fmt.Fprintf(w, "</ul>")
	fmt.Fprintf(w, "</body></html>")
}

func fetchTwitchFollows(client http.Client) (TwitchFollowsResponse, error) {
	req, _ := http.NewRequest("GET", followedStreamersURL, nil)
	req.Header.Set("Client-ID", apikeys.TwitchAPIKey)

	var twitchFollowersData TwitchFollowsResponse

	res, error := client.Do(req)
	if error != nil {
		fmt.Println(error)
		return twitchFollowersData, error
	}

	defer res.Body.Close()

	body, error := ioutil.ReadAll(res.Body)
	if error != nil {
		return twitchFollowersData, errors.New("Error reading response")
	}

	err := json.Unmarshal(body, &twitchFollowersData)
	if err != nil {
		log.Fatalln(err)
		return twitchFollowersData, errors.New("Error parsing JSON")
	}
	return twitchFollowersData, nil
}

func fetchTwitchStreamersStatus(client http.Client, twitchFollowsResponse TwitchFollowsResponse) (OnlineUsersResponse, error) {
	streamStatusRequest, error := http.NewRequest("GET", streamStatusURL, nil)
	streamStatusRequest.Header.Set("Client-ID", apikeys.TwitchAPIKey)

	var onlineUsersResponse OnlineUsersResponse

	q := streamStatusRequest.URL.Query()
	q.Add("first", "100")
	for _, element := range twitchFollowsResponse.Data {
		q.Add("user_id", element.ToID)
	}

	streamStatusRequest.URL.RawQuery = q.Encode()

	streamStatusResponse, error := client.Do(streamStatusRequest)
	if error != nil {
		log.Fatalln(error)
	}

	defer streamStatusResponse.Body.Close()

	statusResponseBody, error := ioutil.ReadAll(streamStatusResponse.Body)
	if error != nil {
		return onlineUsersResponse, error
	}

	err := json.Unmarshal(statusResponseBody, &onlineUsersResponse)
	if err != nil {
		log.Fatalln(err)
		return onlineUsersResponse, errors.New("Error parsing JSON")
	}
	return onlineUsersResponse, nil
}

func fetchGames(client http.Client, onlineUsers OnlineUsersResponse) ([]OnlineStreamer, error) {
	gamesMap := make(map[string]bool)

	for _, user := range onlineUsers.Data {
		gamesMap[user.GameID] = true
	}

	gamesRequest, error := http.NewRequest("GET", gamesURL, nil)
	gamesRequest.Header.Set("Client-ID", apikeys.TwitchAPIKey)

	var onlineStreamers []OnlineStreamer
	var gamesResponse GamesResponse

	q := gamesRequest.URL.Query()
	q.Add("first", "100")
	for key := range gamesMap {
		q.Add("id", key)
	}

	gamesRequest.URL.RawQuery = q.Encode()

	gamesRes, error := client.Do(gamesRequest)
	if error != nil {
		log.Fatalln(error)
	}

	defer gamesRes.Body.Close()

	statusResponseBody, error := ioutil.ReadAll(gamesRes.Body)
	if error != nil {
		return onlineStreamers, error
	}

	err := json.Unmarshal(statusResponseBody, &gamesResponse)
	if err != nil {
		log.Fatalln(err)
		return onlineStreamers, errors.New("Error parsing JSON")
	}

	gameIDToNameMap := make(map[string]string)
	for _, game := range gamesResponse.Data {
		gameIDToNameMap[game.ID] = game.Name
	}

	for _, user := range onlineUsers.Data {
		onlineStreamer := OnlineStreamer{
			user.UserName,
			gameIDToNameMap[user.GameID],
			user.Title,
		}
		onlineStreamers = append(onlineStreamers, onlineStreamer)
	}
	return onlineStreamers, nil
}
