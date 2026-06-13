package cast

import "fmt"

// URL casts a remote URL to a Chromecast device. Pass streamType "LIVE" for
// live HLS streams and "BUFFERED" for progressive/VOD content.
// appID selects the Cast receiver application; empty string uses the Default
// Media Receiver (CC1AD845). meta is optional stream metadata for the seek bar.
func URL(url string, ipAddress string, contentType string, streamType string, appID string, meta *MediaMeta) error {
	if err := loadMedia(ipAddress, appID, url, contentType, streamType, meta); err != nil {
		fmt.Printf("[%s] Cast failed: %v\n", ipAddress, err)
		return err
	}
	return nil
}
