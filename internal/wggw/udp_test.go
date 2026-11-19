package wggw_test

import (
	"context"
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

// TestGatewayUDPExit drives a UDP datagram through the WireGuard gateway to a real
// host UDP echo server, exercising the gateway's UDP forwarder (handleUDP/pumpUDP).
func TestGatewayUDPExit(t *testing.T) {
	// Host-side UDP echo server.
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()
	echoAddr := echo.LocalAddr().String()
	const routedTarget = "192.0.2.40:9"

	serverPriv, serverPub := wgKeypair(t)
	clientPriv, clientPub := wgKeypair(t)
	const clientIP = "10.6.0.2"

	cliCarrier, srvCarrier := dgpipe.Pair()
	gw, err := wggw.New(srvCarrier, wggw.Config{
		ServerPrivKey: serverPriv,
		Peers:         []wggw.Peer{{PubKey: clientPub, AllowedIP: clientIP + "/32"}},
		MTU:           1420,
		UDPIdle:       time.Second,
		Logf:          t.Logf,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if network != "udp" || addr != routedTarget {
				return nil, fmt.Errorf("unexpected dial %s %s", network, addr)
			}
			return (&net.Dialer{}).DialContext(ctx, "udp", echoAddr)
		},
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	defer gw.Close()
	defer srvCarrier.Close()

	cliTun, cliNet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(clientIP)}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	cliDev := device.NewDevice(cliTun, wgbind.New(cliCarrier), device.NewLogger(device.LogLevelError, "wg-client "))
	cliCfg := fmt.Sprintf("private_key=%s\nlisten_port=0\nreplace_peers=true\npublic_key=%s\nendpoint=127.0.0.1:1\nallowed_ip=0.0.0.0/0\npersistent_keepalive_interval=1\n", clientPriv, serverPub)
	if err := cliDev.IpcSet(cliCfg); err != nil {
		t.Fatal(err)
	}
	if err := cliDev.Up(); err != nil {
		t.Fatal(err)
	}
	defer cliDev.Close()
	defer cliCarrier.Close()

	uconn, err := cliNet.DialUDPAddrPort(netip.AddrPort{}, netip.MustParseAddrPort(routedTarget))
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer uconn.Close()

	_ = uconn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := uconn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 16)
	n, err := uconn.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if got := string(buf[:n]); got != "ping" {
		t.Fatalf("echo = %q, want ping", got)
	}
}
