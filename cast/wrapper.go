package cast

import (
	"fmt"

	"github.com/vishen/go-chromecast/application"
	"github.com/vishen/go-chromecast/cmd"
)

// URL take a URL and IPAddress of a Chromecast device to play video on
func URL(url string, ipAddress string) error {
	app := application.NewApplication()
	entry := cmd.CachedDNSEntry{
		Addr: ipAddress,
		Port: 8009,
	}

	if err := app.Start(entry); err != nil {
		fmt.Println("Unable to start app", err)
		return err
	}

	if err := app.Load(url, "", false, true); err != nil {
		fmt.Printf("unable to load media: %v\n", err)
		return err
	}
	return nil
}
