package config

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"twitch-caster/models"
)

const configFileName = "configuration.json"

// Load is used to load the configuration file from disk
func Load() models.Configuration {
	ex, err := os.Executable()
	if err != nil {
		log.Fatalln("Error fetching the current path: ", err)
	}
	exPath := filepath.Dir(ex)

	data, err := ioutil.ReadFile(exPath + "/" + configFileName)
	if err != nil {
		log.Fatalln("Error reading configuration JSON file: ", err)
	}

	var config models.Configuration
	jsonError := json.Unmarshal(data, &config)
	if jsonError != nil {
		log.Fatalln("Error parsing configuration JSON: ", jsonError)
	}

	validateConfig(config)
	return config
}

func validateConfig(config models.Configuration) {
	if config.Settings.UserID == "" ||
		config.Settings.TwitchClientID == "" ||
		config.Settings.TwitchSecret == "" {
		log.Fatalln("Error in " + configFileName + ", missing required settings")
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
