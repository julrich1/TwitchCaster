GOOS=linux GOARCH=arm GOARM=5 go build
scp -r twitch-caster static pi@raspberrypi:
rm twitch-caster