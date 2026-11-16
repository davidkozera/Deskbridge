#!/bin/sh
# Builds Deskbridge and packs it into one setup program:
#   dist/Deskbridge-Setup-<version>.exe
# Needs only Go (1.24 or later). Run from any shell, including Git Bash.
set -e
cd "$(dirname "$0")"

VERSION=$(tr -d ' \r\n' < VERSION)
LDFLAGS="-s -w -X deskbridge/internal/proto.Version=$VERSION"
PAYLOAD=cmd/deskbridge-setup/payload
export CGO_ENABLED=0
mkdir -p dist

echo "Deskbridge $VERSION"

# Icon and version details for each program (see tools/winres).
stamp() { go run ./tools/winres -dir "cmd/$1" -version "$VERSION" -desc "$2" -file "$1.exe"; }
stamp deskbridge-server "Deskbridge server"
stamp deskbridge-client "Deskbridge client"
stamp deskbridge-setup  "Deskbridge Setup"

# The server and client, for both processor types, compressed into the
# folder the setup program is built from.
rm -f "$PAYLOAD"/*.gz
for arch in arm64 amd64; do
	GOOS=windows GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "dist/server-$arch.exe" ./cmd/deskbridge-server
	GOOS=windows GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS -H windowsgui" -o "dist/client-$arch.exe" ./cmd/deskbridge-client
	gzip -9 -n -c "dist/server-$arch.exe" > "$PAYLOAD/server-$arch.exe.gz"
	gzip -9 -n -c "dist/client-$arch.exe" > "$PAYLOAD/client-$arch.exe.gz"
	rm "dist/server-$arch.exe" "dist/client-$arch.exe"
done
gzip -9 -n -c README.txt > "$PAYLOAD/README.txt.gz"

# The setup program is built for x64 only: Arm PCs run it under emulation,
# and it installs the native programs.
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS -H windowsgui" \
	-o "dist/Deskbridge-Setup-$VERSION.exe" ./cmd/deskbridge-setup

# The relay (see README: INTERNET) runs on Linux, as a container.
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o dist/deskbridge-relay ./cmd/deskbridge-relay

echo "Built dist/Deskbridge-Setup-$VERSION.exe and dist/deskbridge-relay"
