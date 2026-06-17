package cast

// MediaMeta carries stream metadata to include in the Cast LOAD command.
type MediaMeta struct {
	Title       string
	Game        string
	Login       string // streamer login, passed to receiver for polling
	Resolution  string // e.g. "1920×1080", derived server-side from resolved quality
	FPS         string // e.g. "60fps"
	ViewerCount int
}

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
	ContentID   string           `json:"contentId"`
	ContentType string           `json:"contentType"`
	StreamType  string           `json:"streamType"`
	Metadata    *mediaMetadata   `json:"metadata,omitempty"`
	CustomData  *mediaCustomData `json:"customData,omitempty"`
}

type mediaMetadata struct {
	MetadataType int    `json:"metadataType"` // 0 = GenericMediaMetadata
	Title        string `json:"title"`
	Subtitle     string `json:"subtitle"`
}

type mediaCustomData struct {
	Login       string `json:"login"`
	Resolution  string `json:"resolution,omitempty"`
	FPS         string `json:"fps,omitempty"`
	ViewerCount int    `json:"viewerCount,omitempty"`
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
	SessionID   string `json:"sessionId"`
	TransportID string `json:"transportId"`
}
