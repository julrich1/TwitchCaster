set GOOS=linux
set GOARCH=arm
set GOARM=5
go build
set GOOS=
set GOARCH=
set GOARM=
scp twitch-caster pi@raspberrypi: