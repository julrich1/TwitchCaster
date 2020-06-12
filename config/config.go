package config

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"twitch-caster/models"
)

const configFileName = "configuration.json"

// Load is used to load the configuration file from disk
func Load() models.Configuration {
	ex, err := os.Executable()
	if err != nil {
		log.Fatalln(err)
	}
	exPath := filepath.Dir(ex)

	data, err := ioutil.ReadFile(exPath + "/" + configFileName)
	if err != nil {
		log.Fatalln(err)
	}

	var config models.Configuration
	jsonError := json.Unmarshal(data, &config)
	if jsonError != nil {
		log.Fatalln(jsonError)
	}
	return config
}
