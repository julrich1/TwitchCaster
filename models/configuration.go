package models

// Configuration object read from JSON
type Configuration struct {
	Settings    Settings     `json:"settings"`
	Chromecasts []Chromecast `json:"chromecasts"`
}

// Settings required to run the application
type Settings struct {
	UserID         string `json:"userId"`
	TwitchClientID string `json:"twitchClientId"`
	TwitchSecret   string `json:"twitchSecret"`
	ChannelListURL string `json:"channelListURL"`
	CastURL        string `json:"castURL"`
}

// Chromecast objects that are cast targets
type Chromecast struct {
	Name       string `json:"name"`
	IPAddress  string `json:"ipAddress"`
	QualityMax string `json:"qualityMax"`
}
