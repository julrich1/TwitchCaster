GOOS=linux GOARCH=arm GOARM=5 go build
scp -r twitch-caster configuration.json static pi@raspberrypi:
rm twitch-caster