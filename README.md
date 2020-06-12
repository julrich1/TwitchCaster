# TwitchCaster

A simple front-end server for casting Twitch videos to your Chromecast

## Getting Started

1) Pull down the repository
2) Build the project using Go
3) Populate the configuration.json file with your Twitch User ID, Application Client ID & Secret (Generated here: https://dev.twitch.tv/console), and the static IP address, name, and quality of at least one Chromecast device.
4) Run the executeable and access the server in your browser (http://localhost:3010/gui/twitch-channel-list)

### Prerequisites

This project will require Streamlink (https://streamlink.github.io/) to be installed and in your PATH
