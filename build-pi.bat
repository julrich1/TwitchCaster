set GOOS=linux
set GOARCH=arm
set GOARM=5
go build
set GOOS=
set GOARCH=
set GOARM=
scp -r twitch-caster configuration.json static pi@raspberrypi:
del twitch-caster