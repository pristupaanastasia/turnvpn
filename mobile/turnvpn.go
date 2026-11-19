// Package turnvpn is the gomobile-bindable entry point for the Android client.
// Kotlin calls Start() with the VpnService tun file descriptor; this runs a
// WireGuard device directly on that tun, carried over the turnvpn transport
// (TURN + PSK-DTLS) to the VPS exit, which terminates WireGuard and egresses to
// the open internet. WireGuard routes the whole device (TCP, UDP and QUIC), so no
// tun2socks and no DNS-over-TCP shim are needed; DNS resolves at the exit.
package turnvpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/device"

	"github.com/pristupaanastasia/turnvpn/internal/carrier"
	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
)

var state struct {
	mu      sync.Mutex
	dev     *device.Device
	closers []io.Closer
	running bool
}

func closeAll(cs []io.Closer) {
	for i := len(cs) - 1; i >= 0; i-- {
		_ = cs[i].Close()
	}
}

// Start brings up the VPN: it establishes the DTLS-over-TURN carrier (credentials
// from the VK call token) and runs a WireGuard device on tunFd over it.
//
//	tunFd       — the VpnService tun file descriptor (an independent dup; see Kotlin)
//	mtu         — the tun MTU
//	peer        — the VPS exit "host:port" (running cmd/server)
//	secret      — the shared DTLS PSK, must match the server
//	vkLink      — the token from vk.ru/call/join/<token>
//	token       — the per-call guest token captured from a browser (past the captcha)
//	appKey      — the OK application key (public web-client key)
//	wgKey       — this device's WireGuard private key (hex; kept in the Keystore)
//	wgServerPub — the server's WireGuard public key (hex)
//	wgAddr      — this device's WireGuard tunnel address (e.g. "10.6.0.2")
//
// It returns an error if already running or on setup failure.
func Start(tunFd, mtu int, peer, secret, vkLink, token, appKey, wgKey, wgServerPub, wgAddr string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.running {
		return errors.New("turnvpn: already running")
	}

	peerAddr, err := net.ResolveUDPAddr("udp4", peer)
	if err != nil {
		return fmt.Errorf("turnvpn: resolve peer %q: %w", peer, err)
	}

	// Establish the carrier (bounded; surfaces credential/TURN/DTLS failures here so
	// the UI only shows "connected" once it succeeds).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	carrierConn, closers, err := carrier.Dial(ctx, carrier.Config{
		Peer:   peerAddr,
		Secret: secret,
		Creds:  carrier.TokenCreds(appKey, vkLink, token),
		Logf:   GoLog,
	})
	if err != nil {
		return err
	}

	tunDev, err := makeTUN(tunFd, mtu)
	if err != nil {
		closeAll(closers)
		return err
	}

	dev := device.NewDevice(tunDev, wgbind.New(carrierConn), device.NewLogger(device.LogLevelError, "turnvpn "))
	uapi := fmt.Sprintf(""+
		"private_key=%s\n"+
		"listen_port=0\n"+
		"replace_peers=true\n"+
		"public_key=%s\n"+
		"endpoint=127.0.0.1:1\n"+ // dummy; wgbind ignores it (one carrier = one peer)
		"allowed_ip=0.0.0.0/0\n"+
		"allowed_ip=::/0\n"+
		"persistent_keepalive_interval=25\n",
		wgKey, wgServerPub)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		closeAll(closers)
		return fmt.Errorf("turnvpn: wireguard config: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		closeAll(closers)
		return fmt.Errorf("turnvpn: wireguard up: %w", err)
	}

	state.dev = dev
	state.closers = closers
	state.running = true
	return nil
}

// Stop tears down the WireGuard device and the carrier.
func Stop() {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.running {
		return
	}
	if state.dev != nil {
		state.dev.Close()
		state.dev = nil
	}
	closeAll(state.closers)
	state.closers = nil
	state.running = false
}

// Hello is a trivial sanity call usable from Kotlin to verify the binding.
func Hello() string { return "turnvpn gomobile ok" }

// GoLog is a no-op by default; Kotlin may override logging by other means. It
// matches the carrier Logf shape.
func GoLog(string, ...any) {}
