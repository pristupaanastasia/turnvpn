#!/usr/bin/env sh
# Cross-compile turnvpn binaries into dist/.
#
#   ./scripts/build.sh           # all targets (VPS server + clients incl. Android)
#   ./scripts/build.sh android   # just the Android/Termux clients (arm64 + arm)
#
# Termux on Android runs ordinary Linux binaries, so the "Android" clients are just
# statically linked linux/arm64 and linux/arm builds. Everything is pure Go
# (CGO_ENABLED=0), so no NDK/toolchain is needed.
set -eu

mkdir -p dist
LDFLAGS="-s -w"

build() { # os arch goarm out pkg
	echo "building dist/$4 ($1/$2${3:+ v$3})"
	GOOS="$1" GOARCH="$2" GOARM="${3:-}" CGO_ENABLED=0 \
		go build -ldflags "$LDFLAGS" -o "dist/$4" "$5"
}

case "${1:-all}" in
android)
	build linux arm64 "" turnvpn-client-android-arm64 ./cmd/client
	build linux arm 7 turnvpn-client-android-arm ./cmd/client
	;;
all)
	build linux amd64 "" turnvpn-server-linux-amd64 ./cmd/server
	build linux amd64 "" turnvpn-client-linux-amd64 ./cmd/client
	build linux arm64 "" turnvpn-client-android-arm64 ./cmd/client
	build linux arm 7 turnvpn-client-android-arm ./cmd/client
	;;
*)
	echo "usage: $0 [all|android]" >&2
	exit 2
	;;
esac

echo "done -> dist/"
