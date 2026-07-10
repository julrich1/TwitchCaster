package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"twitch-caster/models"
)

const configFileName = "configuration.json"
const defaultChannelListURL = "/gui/twitch-channel-list"
const defaultCastURL = "/gui/cast/"
const defaultPort = 3010

// Load is used to load the configuration file from disk
func Load() models.Configuration {
	ex, err := os.Executable()
	if err != nil {
		log.Fatalln("Error fetching the current path: ", err)
	}
	exPath := filepath.Dir(ex)

	data, err := os.ReadFile(exPath + "/" + configFileName)
	if err != nil {
		log.Fatalln("Error reading configuration JSON file: ", err)
	}

	var config models.Configuration
	jsonError := json.Unmarshal(data, &config)
	if jsonError != nil {
		log.Fatalln("Error parsing configuration JSON: ", jsonError)
	}

	validateConfig(&config)
	return config
}

func validateConfig(config *models.Configuration) {
	if config.Settings.UserID == "" ||
		config.Settings.TwitchClientID == "" ||
		config.Settings.TwitchSecret == "" {
		log.Fatalln("Error in " + configFileName + ", missing required settings")
	}

	if config.Settings.BaseURL != "" && config.Settings.AdminPassword == "" {
		log.Fatalln("Error in " + configFileName + ", adminPassword is required when baseURL is set (server is exposed beyond localhost)")
	}

	if config.Settings.Port == 0 {
		config.Settings.Port = defaultPort
	}

	if config.Settings.ChannelListURL == "" {
		config.Settings.ChannelListURL = defaultChannelListURL
	}

	if config.Settings.CastURL == "" {
		config.Settings.CastURL = defaultCastURL
	}

	if len(config.Chromecasts) == 0 {
		log.Fatalln("Error in " + configFileName + ", missing at least one chromecast")
	}

	for i, chromecast := range config.Chromecasts {
		if chromecast.IPAddress == "" ||
			chromecast.Name == "" ||
			chromecast.QualityMax == "" {
			log.Fatalln("Error in " + configFileName + ", Chromecast #" + strconv.Itoa(i) + " missing required settings")
		}
	}
}
