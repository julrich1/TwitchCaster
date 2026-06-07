package cast

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	defaultAppID = "CC1AD845"

	nsConn      = "urn:x-cast:com.google.cast.tp.connection"
	nsReceiver  = "urn:x-cast:com.google.cast.receiver"
	nsMedia     = "urn:x-cast:com.google.cast.media"
	nsHeartbeat = "urn:x-cast:com.google.cast.tp.heartbeat"

	senderID   = "sender-0"
	receiverID = "receiver-0"
)

type device struct {
	conn *tls.Conn
	mu   sync.Mutex
}

func dialDevice(ipAddress string) (*device, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp",
		fmt.Sprintf("%s:8009", ipAddress),
		&tls.Config{InsecureSkipVerify: true},
	)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", ipAddress, err)
	}
	return &device{conn: conn}, nil
}

func (d *device) send(sourceID, destID, namespace, payload string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return writeMessage(d.conn, castMessage{
		sourceID:  sourceID,
		destID:    destID,
		namespace: namespace,
		payload:   payload,
	})
}

// loadMedia connects to the Chromecast at ipAddress, launches appID, and sends
// a LOAD command for the given URL. The connection is closed once the LOAD is
// acknowledged, which does not interrupt playback on the receiver.
func loadMedia(ipAddress, appID, url, contentType, streamType string) error {
	if appID == "" {
		appID = defaultAppID
	}

	d, err := dialDevice(ipAddress)
	if err != nil {
		return err
	}
	defer d.conn.Close()

	if err := d.send(senderID, receiverID, nsConn, `{"type":"CONNECT"}`); err != nil {
		return fmt.Errorf("CONNECT: %w", err)
	}

	launch, _ := json.Marshal(launchMsg{Type: "LAUNCH", RequestID: 1, AppID: appID})
	if err := d.send(senderID, receiverID, nsReceiver, string(launch)); err != nil {
		return fmt.Errorf("LAUNCH: %w", err)
	}

	transportID, err := d.waitForApp(appID, 60*time.Second)
	if err != nil {
		return fmt.Errorf("waiting for %s to launch: %w", appID, err)
	}

	if err := d.send(senderID, transportID, nsConn, `{"type":"CONNECT"}`); err != nil {
		return fmt.Errorf("CONNECT transport: %w", err)
	}

	load, _ := json.Marshal(loadMsg{
		Type:      "LOAD",
		RequestID: 2,
		Autoplay:  true,
		Media: mediaItem{
			ContentID:   url,
			ContentType: contentType,
			StreamType:  streamType,
		},
	})
	if err := d.send(senderID, transportID, nsMedia, string(load)); err != nil {
		return fmt.Errorf("LOAD: %w", err)
	}

	return nil
}

// waitForApp reads messages until RECEIVER_STATUS shows appID running, then
// returns its transportId. PING messages are answered with PONG inline.
// GET_STATUS is polled every 5s because the Chromecast does not always send
// unsolicited RECEIVER_STATUS updates while a custom receiver is loading.
func (d *device) waitForApp(appID string, timeout time.Duration) (string, error) {
	d.conn.SetReadDeadline(time.Now().Add(timeout))

	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		reqID := 10
		for {
			select {
			case <-ticker.C:
				d.send(senderID, receiverID, nsReceiver,
					fmt.Sprintf(`{"type":"GET_STATUS","requestId":%d}`, reqID))
				reqID++
			case <-done:
				return
			}
		}
	}()

	for {
		msg, err := readMessage(d.conn)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}

		if msg.namespace == nsHeartbeat {
			var t msgType
			if json.Unmarshal([]byte(msg.payload), &t) == nil && t.Type == "PING" {
				// Send PONG without holding the read path.
				go d.send(senderID, receiverID, nsHeartbeat, `{"type":"PONG"}`)
			}
			continue
		}

		if msg.namespace != nsReceiver {
			continue
		}

		var t msgType
		if json.Unmarshal([]byte(msg.payload), &t) == nil && t.Type == "LAUNCH_ERROR" {
			return "", fmt.Errorf("LAUNCH_ERROR from Chromecast: %s", msg.payload)
		}

		var status receiverStatusMsg
		if err := json.Unmarshal([]byte(msg.payload), &status); err != nil || status.Type != "RECEIVER_STATUS" {
			continue
		}

		for _, app := range status.Status.Applications {
			if app.AppID == appID && app.TransportID != "" {
				return app.TransportID, nil
			}
		}
	}
}
