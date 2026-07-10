# TwitchCaster

A simple front-end server for casting Twitch videos to your Chromecast

## Getting Started

1) Pull down the repository
2) Build the project using Go
3) Copy `configuration.example.json` to `configuration.json` (next to the executable) and populate it with your Twitch User ID, Application Client ID & Secret (Generated here: https://dev.twitch.tv/console), an admin password for the web GUI, and the static IP address, name, and quality of at least one Chromecast device. `configuration.json` is gitignored so your credentials stay out of version control.
4) Run the executable and access the server in your browser (http://localhost:3010/gui/twitch-channel-list)

### Prerequisites

This project will require Streamlink (https://streamlink.github.io/) to be installed and in your PATH
