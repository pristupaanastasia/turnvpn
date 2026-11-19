//go:build linux

package turnvpn

import (
	"golang.zx2c4.com/wireguard/tun"
)

// makeTUN wraps the VpnService file descriptor as a WireGuard TUN device. On
// Android (a Linux GOOS) CreateUnmonitoredTUNFromFD avoids the netlink monitoring
// that VpnService does not provide. The caller passes an independent dup of the fd.
func makeTUN(fd, _ int) (tun.Device, error) {
	dev, _, err := tun.CreateUnmonitoredTUNFromFD(fd)
	return dev, err
}
