// Package turnvpn is the gomobile-bindable entry point for the Android client.
// Kotlin calls Start() with the VpnService tun file descriptor; this runs a
// tun2socks engine that routes the device's TCP traffic through the turnvpn tunnel
// (TURN + PSK-DTLS + KCP/smux) to the VPS exit.
//
// The tunnel carries TCP only, so UDP flows are refused. Enable Private DNS
// (DNS-over-TLS, which is TCP) on the device so name resolution rides the tunnel;
// QUIC (UDP/443) falls back to TCP in browsers.
package turnvpn

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"
	"sync"

	"github.com/xjasonlyu/tun2socks/v2/engine"
	M "github.com/xjasonlyu/tun2socks/v2/metadata"
	"github.com/xjasonlyu/tun2socks/v2/proxy"

	"github.com/pristupaanastasia/turnvpn/internal/vpntun"
)

// errUDPUnsupported is returned for UDP flows: the tunnel is TCP only.
var errUDPUnsupported = errors.New("turnvpn: UDP not supported; enable Private DNS (DoT) on the device")

var state struct {
	mu      sync.Mutex
	dialer  *vpntun.Dialer
	running bool
}

func init() {
	// Register a proxy scheme the engine resolves to the live turnvpn tunnel.
	proxy.RegisterProtocol("turntun", func(*url.URL) (proxy.Proxy, error) {
		return tunProxy{}, nil
	})
}

// tunProxy routes tun2socks flows through the active turnvpn tunnel.
type tunProxy struct{}

func (tunProxy) DialContext(ctx context.Context, m *M.Metadata) (net.Conn, error) {
	state.mu.Lock()
	d := state.dialer
	state.mu.Unlock()
	if d == nil {
		return nil, errors.New("turnvpn: not started")
	}
	return d.Dial(ctx, "tcp", m.DestinationAddrPort().String())
}

func (tunProxy) DialUDP(*M.Metadata) (net.PacketConn, error) { return nil, errUDPUnsupported }

// Start brings up the VPN: it builds the tunnel (credentials from the VK call token)
// and starts a tun2socks engine on tunFd routing the device's traffic through it.
//
//	tunFd  — the VpnService tun file descriptor
//	mtu    — the tun MTU (e.g. 1500)
//	peer   — the VPS exit "host:port" (running cmd/server)
//	secret — the shared PSK, must match the server
//	vkLink — the token from vk.ru/call/join/<token>
//	token  — the per-call guest token captured from a browser (past the captcha)
//	appKey — the OK application key (public web-client key)
//
// It returns an error if already running or on setup failure.
func Start(tunFd int, mtu int, peer, secret, vkLink, token, appKey string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.running {
		return errors.New("turnvpn: already running")
	}
	d, err := vpntun.New(vpntun.Config{
		PeerAddr: peer,
		Secret:   secret,
		Creds:    vpntun.TokenCreds(appKey, vkLink, token),
	})
	if err != nil {
		return err
	}
	state.dialer = d

	engine.Insert(&engine.Key{
		MTU:      mtu,
		Device:   "fd://" + strconv.Itoa(tunFd),
		Proxy:    "turntun://turnvpn",
		LogLevel: "warning",
	})
	engine.Start()
	state.running = true
	return nil
}

// Stop tears down the engine and the tunnel.
func Stop() {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.running {
		return
	}
	engine.Stop()
	if state.dialer != nil {
		_ = state.dialer.Close()
		state.dialer = nil
	}
	state.running = false
}

// Hello is a trivial sanity call usable from Kotlin to verify the binding.
func Hello() string { return "turnvpn gomobile ok" }
