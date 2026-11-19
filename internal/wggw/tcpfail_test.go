package wggw_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/pristupaanastasia/turnvpn/internal/dgpipe"
	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
)

// TestGatewayTCPDialFail covers handleTCP's failure branch: when the exit cannot
// dial the target it resets the connection, so the client's dial errors.
func TestGatewayTCPDialFail(t *testing.T) {
	serverPriv, serverPub := wgKeypair(t)
	clientPriv, clientPub := wgKeypair(t)
	const clientIP = "10.6.0.2"

	cliC, srvC := dgpipe.Pair()
	gw, err := wggw.New(srvC, wggw.Config{
		ServerPrivKey: serverPriv,
		Peers:         []wggw.Peer{{PubKey: clientPub, AllowedIP: clientIP + "/32"}},
		MTU:           1420,
		Logf:          t.Logf,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("refused")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	defer srvC.Close()

	cliTun, cliNet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(clientIP)}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	cliDev := device.NewDevice(cliTun, wgbind.New(cliC), device.NewLogger(device.LogLevelError, "wg-client "))
	cfg := fmt.Sprintf("private_key=%s\nlisten_port=0\nreplace_peers=true\npublic_key=%s\nendpoint=127.0.0.1:1\nallowed_ip=0.0.0.0/0\npersistent_keepalive_interval=1\n", clientPriv, serverPub)
	if err := cliDev.IpcSet(cfg); err != nil {
		t.Fatal(err)
	}
	if err := cliDev.Up(); err != nil {
		t.Fatal(err)
	}
	defer cliDev.Close()
	defer cliC.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if _, err := cliNet.DialContextTCPAddrPort(ctx, netip.MustParseAddrPort("192.0.2.60:80")); err == nil {
		t.Fatal("expected the dial to fail (gateway refuses the target)")
	}
}
