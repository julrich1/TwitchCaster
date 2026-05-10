GOOS=linux GOARCH=arm GOARM=5 go build
scp -r twitch-caster configuration.json static pi@streambox.local:
rm twitch-caster