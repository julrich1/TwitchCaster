set GOOS=linux
set GOARCH=arm
set GOARM=5
go build
set GOOS=
set GOARCH=
set GOARM=
scp -r twitch-caster static pi@raspberrypi: