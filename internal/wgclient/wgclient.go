// Package wgclient is the client-side WireGuard endpoint for turnvpn.
//
// It runs a wireguard-go device over a carrier net.Conn (the DTLS-over-TURN
// tunnel) on a userspace gVisor netstack, and exposes DialContext so a local
// SOCKS5 front-end can open connections that egress through the WireGuard exit on
// the VPS. Running on netstack means the client needs no TUN device and no root;
// hostnames are resolved by DNS over the tunnel (remote DNS), so names resolve at
// the exit, not on the possibly-cut-off local network.
//
//	SOCKS5 app → DialContext → netstack → wireguard-go → wgbind → carrier(DTLS/TURN)
package wgclient

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
)

// Config configures a Client.
type Config struct {
	// PrivKey (client) and ServerPub are hex-encoded X25519 keys (UAPI format).
	PrivKey   string
	ServerPub string

	// Address is the client's tunnel IP, e.g. "10.6.0.2".
	Address string

	// DNS are the resolvers reachable through the tunnel; names are looked up here
	// (remote DNS). Default 1.1.1.1 and 8.8.8.8.
	DNS []netip.Addr

	// MTU of the inner tunnel. Default 1280.
	MTU int

	// Keepalive is the WireGuard persistent-keepalive interval in seconds; it keeps
	// the carrier/relay mapping and the server's idle watchdog alive. Default 25.
	Keepalive int

	// Shape paces outbound WireGuard emission to at most this many bytes/sec
	// (footprint shaping). 0 = unlimited.
	Shape int

	// Logf logs device-level errors; default no-op.
	Logf func(string, ...any)
}

// Client is a running WireGuard endpoint over one carrier conn.
type Client struct {
	dev *device.Device
	net *netstack.Net
}

// New brings a wireguard-go device up over carrier and returns a Client ready to
// DialContext. The carrier is owned by the caller and closed separately.
func New(carrier net.Conn, cfg Config) (*Client, error) {
	if cfg.MTU == 0 {
		cfg.MTU = 1280
	}
	if cfg.Keepalive == 0 {
		cfg.Keepalive = 25
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if len(cfg.DNS) == 0 {
		cfg.DNS = []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}
	}
	addr, err := netip.ParseAddr(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("wgclient: address %q: %w", cfg.Address, err)
	}

	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, cfg.DNS, cfg.MTU)
	if err != nil {
		return nil, fmt.Errorf("wgclient: tun: %w", err)
	}
	dev := device.NewDevice(tunDev, wgbind.NewWithRate(carrier, cfg.Shape), device.NewLogger(device.LogLevelError, "wgclient "))
	uapi := fmt.Sprintf(""+
		"private_key=%s\n"+
		"listen_port=0\n"+
		"replace_peers=true\n"+
		"public_key=%s\n"+
		"endpoint=127.0.0.1:1\n"+ // dummy; wgbind ignores it (one carrier = one peer)
		"allowed_ip=0.0.0.0/0\n"+
		"allowed_ip=::/0\n"+
		"persistent_keepalive_interval=%d\n",
		cfg.PrivKey, cfg.ServerPub, cfg.Keepalive)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgclient: ipc: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgclient: up: %w", err)
	}
	return &Client{dev: dev, net: tnet}, nil
}

// DialContext opens a connection to address ("host:port") through the tunnel. A
// hostname is resolved by DNS over the tunnel (remote DNS), so resolution happens
// at the exit. network is "tcp"/"tcp4"/"tcp6" or "udp"/"udp4"/"udp6".
func (c *Client) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("wgclient: bad address %q: %w", address, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("wgclient: bad port in %q: %w", address, err)
	}

	ip, err := netip.ParseAddr(host)
	if err != nil {
		addrs, lerr := c.net.LookupHost(host)
		if lerr != nil {
			return nil, fmt.Errorf("wgclient: resolve %s: %w", host, lerr)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("wgclient: resolve %s: no addresses", host)
		}
		if ip, err = netip.ParseAddr(addrs[0]); err != nil {
			return nil, fmt.Errorf("wgclient: bad resolved address %q: %w", addrs[0], err)
		}
	}
	dst := netip.AddrPortFrom(ip, uint16(port))

	switch network {
	case "tcp", "tcp4", "tcp6", "":
		return c.net.DialContextTCPAddrPort(ctx, dst)
	case "udp", "udp4", "udp6":
		return c.net.DialUDPAddrPort(netip.AddrPort{}, dst)
	default:
		return nil, fmt.Errorf("wgclient: unsupported network %q", network)
	}
}

// Close shuts the device down. The carrier conn is not closed.
func (c *Client) Close() error {
	c.dev.Close()
	return nil
}
