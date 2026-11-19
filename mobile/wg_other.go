//go:build !linux

package turnvpn

import (
	"errors"

	"golang.zx2c4.com/wireguard/tun"
)

// makeTUN is only implemented on Android/Linux; this stub keeps the package
// buildable on dev hosts (so `go test ./...` compiles) but Start cannot run here.
func makeTUN(_, _ int) (tun.Device, error) {
	return nil, errors.New("turnvpn: Start is only supported on Android")
}
