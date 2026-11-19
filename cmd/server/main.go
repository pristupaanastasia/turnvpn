// Command server is the VPS-side exit of the Vector 1 tunnel.
//
// It listens for DTLS over UDP (the packets the VK TURN relay forwards to it) and,
// for every DTLS connection, runs a WireGuard exit entirely in userspace: a
// gVisor netstack gateway terminates the client's WireGuard session carried inside
// the DTLS tunnel and forwards its traffic to the open internet. The DTLS layer
// (mutual PSK) stays as the carrier/disguise; WireGuard replaces the old
// KCP/smux/SOCKS data plane. No TUN device, no kernel WireGuard, no iptables
// MASQUERADE, no root.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/dtls/v3"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/wgbind"
	"github.com/pristupaanastasia/turnvpn/internal/wggw"
	"github.com/pristupaanastasia/turnvpn/internal/wgkey"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:56000", "UDP address to listen on for DTLS")
	secret := flag.String("secret", "", "single shared secret for mutual DTLS auth (or env TURNVPN_SECRET)")
	keyringPath := flag.String("keyring", "", "path to a per-client DTLS keyring file ('identity secret' per line; or env TURNVPN_KEYRING)")
	wgKeyPath := flag.String("wg-key", "", "path to the server WireGuard private key (hex; or env TURNVPN_WG_KEY as the key itself)")
	wgPeersPath := flag.String("wg-peers", "", "path to the WireGuard peers file ('pubkey allowed-ip' per line; or env TURNVPN_WG_PEERS)")
	mtu := flag.Int("mtu", 1280, "inner tunnel MTU (leave room for WireGuard + DTLS inside a TURN datagram)")
	idle := flag.Duration("idle", 150*time.Second, "drop a tunnel after this long with no packets from the client")
	shape := flag.String("shape", "", "footprint-shape outbound bitrate cap, e.g. 2mbit (empty = unlimited)")
	genKey := flag.Bool("genkey", false, "print a fresh WireGuard keypair and exit")
	flag.Parse()

	if *genKey {
		priv, pub, err := wgkey.Generate()
		if err != nil {
			log.Fatalf("server: genkey: %v", err)
		}
		fmt.Printf("private_key (server — keep secret): %s\npublic_key  (give to clients):       %s\n", priv, pub)
		return
	}

	// --- DTLS carrier authentication (unchanged): shared secret or keyring ---
	if *secret == "" {
		*secret = os.Getenv("TURNVPN_SECRET")
	}
	if *keyringPath == "" {
		*keyringPath = os.Getenv("TURNVPN_KEYRING")
	}
	var psk dtls.PSKCallback
	switch {
	case *keyringPath != "":
		kr, err := loadKeyring(*keyringPath)
		if err != nil {
			log.Fatalf("server: keyring %q: %v", *keyringPath, err)
		}
		log.Printf("server: per-client keyring loaded (%d identities)", len(kr))
		psk = kr.PSK()
	case *secret != "":
		psk = authdtls.PSK(*secret)
	default:
		log.Fatal("server: provide -secret/TURNVPN_SECRET or -keyring/TURNVPN_KEYRING — DTLS auth keeps the exit from being an open proxy")
	}

	// --- WireGuard server key + authorized peers ---
	wgPriv, err := loadWGKey(*wgKeyPath)
	if err != nil {
		log.Fatalf("server: wg key: %v", err)
	}
	shapeBytes, err := wgbind.ParseBitrate(*shape)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	peers, err := loadWGPeers(*wgPeersPath)
	if err != nil {
		log.Fatalf("server: wg peers: %v", err)
	}
	log.Printf("server: WireGuard exit ready (%d peer(s), mtu %d)", len(peers), *mtu)

	laddr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		log.Fatalf("server: resolve %q: %v", *listen, err)
	}

	ln, err := dtls.ListenWithOptions("udp", laddr,
		dtls.WithPSK(psk),
		dtls.WithPSKIdentityHint(authdtls.IdentityHint()),
		dtls.WithCipherSuites(authdtls.Cipher),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
	)
	if err != nil {
		log.Fatalf("server: dtls listen on %s: %v", *listen, err)
	}
	defer func() { _ = ln.Close() }()
	log.Printf("server: tunnel exit listening on %s", *listen)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("server: accept: %v", err)
			continue
		}
		go serve(conn, wgPriv, peers, *mtu, *idle, shapeBytes)
	}
}

// serve completes the DTLS handshake and runs a WireGuard exit over the connection
// until the client goes idle (no packets for idle), then tears it down.
func serve(conn net.Conn, wgPriv string, peers []wggw.Peer, mtu int, idle time.Duration, shape int) {
	defer func() { _ = conn.Close() }()
	ra := conn.RemoteAddr()

	if dc, ok := conn.(*dtls.Conn); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := dc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			log.Printf("server: %s handshake: %v", ra, err)
			return
		}
	}
	log.Printf("server: tunnel up from %s", ra)

	// Wrap the carrier so we can detect an idle (dead) client: WireGuard keepalives
	// keep a live client's reads flowing; silence past idle means it is gone.
	ac := &activityConn{Conn: conn}
	ac.touch()

	gw, err := wggw.New(ac, wggw.Config{
		ServerPrivKey: wgPriv,
		Peers:         peers,
		MTU:           mtu,
		Shape:         shape,
		Logf:          log.Printf,
	})
	if err != nil {
		log.Printf("server: %s gateway: %v", ra, err)
		return
	}
	defer func() { _ = gw.Close() }()

	for {
		time.Sleep(idle / 3)
		if since := time.Since(ac.lastRead()); since > idle {
			log.Printf("server: tunnel from %s idle for %s, closing", ra, since.Round(time.Second))
			return
		}
	}
}

// activityConn records the time of the last successful read so an idle tunnel can
// be detected and reclaimed.
type activityConn struct {
	net.Conn
	last atomic.Int64 // UnixNano
}

func (a *activityConn) touch()              { a.last.Store(time.Now().UnixNano()) }
func (a *activityConn) lastRead() time.Time { return time.Unix(0, a.last.Load()) }
func (a *activityConn) Read(p []byte) (int, error) {
	n, err := a.Conn.Read(p)
	if n > 0 {
		a.touch()
	}
	return n, err
}

// loadKeyring parses a DTLS keyring file: one "identity secret" per line, blank
// lines and '#' comments ignored. Revoking a client is removing its line.
func loadKeyring(path string) (authdtls.Keyring, error) {
	secrets, err := readPairs(path, "identity secret")
	if err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return nil, fmt.Errorf("keyring has no identities")
	}
	return authdtls.NewKeyring(secrets), nil
}

// loadWGKey reads the server WireGuard private key from env (TURNVPN_WG_KEY holds
// the hex key directly) or from the given file.
func loadWGKey(path string) (string, error) {
	if path == "" {
		if k := strings.TrimSpace(os.Getenv("TURNVPN_WG_KEY")); k != "" {
			return validWGKey(k)
		}
		return "", fmt.Errorf("provide -wg-key FILE or env TURNVPN_WG_KEY (use -genkey to make one)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return validWGKey(strings.TrimSpace(string(b)))
}

func validWGKey(k string) (string, error) {
	if _, err := wgkey.Public(k); err != nil {
		return "", fmt.Errorf("invalid WireGuard private key: %w", err)
	}
	return k, nil
}

// loadWGPeers parses the WireGuard peers file: one "pubkey allowed-ip" per line,
// blank lines and '#' comments ignored.
func loadWGPeers(path string) ([]wggw.Peer, error) {
	if path == "" {
		path = os.Getenv("TURNVPN_WG_PEERS")
	}
	if path == "" {
		return nil, fmt.Errorf("provide -wg-peers FILE or env TURNVPN_WG_PEERS")
	}
	pairs, err := readPairs(path, "pubkey allowed-ip")
	if err != nil {
		return nil, err
	}
	var peers []wggw.Peer
	for pub, ip := range pairs {
		if _, _, err := net.ParseCIDR(ip); err != nil {
			return nil, fmt.Errorf("peer %s: bad allowed-ip %q: %w", pub, ip, err)
		}
		peers = append(peers, wggw.Peer{PubKey: pub, AllowedIP: ip})
	}
	if len(peers) == 0 {
		return nil, fmt.Errorf("peers file has no peers")
	}
	return peers, nil
}

// readPairs parses a two-field-per-line file into a map, erroring on malformed or
// duplicate first fields. shape is used only in error messages.
func readPairs(path, shape string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: expected '%s'", n, shape)
		}
		if _, dup := out[fields[0]]; dup {
			return nil, fmt.Errorf("line %d: duplicate %q", n, fields[0])
		}
		out[fields[0]] = fields[1]
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
