package cast

// msgType is used to peek at the "type" field of any incoming Cast message.
type msgType struct {
	Type string `json:"type"`
}

type launchMsg struct {
	Type      string `json:"type"`
	RequestID int    `json:"requestId"`
	AppID     string `json:"appId"`
}

type loadMsg struct {
	Type        string    `json:"type"`
	RequestID   int       `json:"requestId"`
	Autoplay    bool      `json:"autoplay"`
	CurrentTime int       `json:"currentTime"`
	Media       mediaItem `json:"media"`
}

type mediaItem struct {
	ContentID   string `json:"contentId"`
	ContentType string `json:"contentType"`
	StreamType  string `json:"streamType"`
}

type receiverStatusMsg struct {
	Type   string         `json:"type"`
	Status receiverStatus `json:"status"`
}

type receiverStatus struct {
	Applications []application `json:"applications"`
}

type application struct {
	AppID       string `json:"appId"`
	TransportID string `json:"transportId"`
}
