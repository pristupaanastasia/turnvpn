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
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/dtls/v3"

	"github.com/pristupaanastasia/turnvpn/internal/authdtls"
	"github.com/pristupaanastasia/turnvpn/internal/kernelexit"
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
	exitMode := flag.String("exit", "userspace", "exit data plane: 'userspace' (gVisor netstack, non-root, SSRF-safe) or 'kernel' (relay to a kernel wg0; needs scripts/kernel-exit-setup.sh)")
	wgPort := flag.Int("wg-port", 51820, "with -exit kernel: the kernel WireGuard listen port on localhost to relay into")
	wgIf := flag.String("wg-if", "wg0", "with -exit kernel: the kernel WireGuard interface to reconcile peers on")
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

	switch *exitMode {
	case "userspace", "kernel":
	default:
		log.Fatalf("server: -exit must be 'userspace' or 'kernel', got %q", *exitMode)
	}

	shapeBytes, err := wgbind.ParseBitrate(*shape)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	// --- WireGuard server key + authorized peers ---
	// In kernel mode the key, peers, routing and NAT all live in the kernel wg0
	// interface (set up by scripts/kernel-exit-setup.sh); the Go server only
	// relays datagrams, so it needs no WireGuard key or peer list of its own.
	var (
		wgPriv string
		peers  []wggw.Peer
	)
	if *exitMode == "userspace" {
		wgPriv, err = loadWGKey(*wgKeyPath)
		if err != nil {
			log.Fatalf("server: wg key: %v", err)
		}
		peers, err = loadWGPeers(*wgPeersPath)
		if err != nil {
			log.Fatalf("server: wg peers: %v", err)
		}
		log.Printf("server: userspace WireGuard exit ready (%d peer(s), mtu %d)", len(peers), *mtu)
	} else {
		if *shape != "" {
			log.Fatal("server: -shape is a userspace-exit feature; shape the kernel exit with tc instead")
		}
		log.Printf("server: kernel WireGuard exit — relaying into 127.0.0.1:%d (ensure wg0 + nft egress filter are up)", *wgPort)
		// Optional: if a peers file is given, the server owns peer membership on
		// wg0 — it reconciles the file into the interface at startup and again on
		// SIGHUP, so clients are added/revoked by editing the file and sending
		// `kill -HUP`, with no restart. Without a peers file, membership is left to
		// whatever scripts/kernel-exit-setup.sh programmed (static).
		if *wgPeersPath != "" || os.Getenv("TURNVPN_WG_PEERS") != "" {
			reconcile := kernelPeerReconciler(kernelexit.NewWGCLI(), *wgIf, *wgPeersPath)
			if err := reconcile(); err != nil {
				log.Fatalf("server: kernel peers: %v", err)
			}
			watchSIGHUP(reconcile, *wgIf)
		}
	}

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
		go serve(conn, exitCfg{
			mode:   *exitMode,
			wgPriv: wgPriv,
			peers:  peers,
			mtu:    *mtu,
			idle:   *idle,
			shape:  shapeBytes,
			wgPort: *wgPort,
		})
	}
}

// exitCfg bundles everything serve needs to stand up the chosen exit for one
// tunnel.
type exitCfg struct {
	mode   string
	wgPriv string
	peers  []wggw.Peer
	mtu    int
	idle   time.Duration
	shape  int
	wgPort int
}

// serve completes the DTLS handshake and runs the chosen WireGuard exit over the
// connection until the client goes idle (no packets for idle), then tears it down.
func serve(conn net.Conn, cfg exitCfg) {
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
	log.Printf("server: tunnel up from %s (exit=%s)", ra, cfg.mode)

	// Wrap the carrier so we can detect an idle (dead) client: WireGuard keepalives
	// keep a live client's reads flowing; silence past idle means it is gone.
	ac := &activityConn{Conn: conn}
	ac.touch()

	closer, err := startExit(ac, ra, cfg)
	if err != nil {
		log.Printf("server: %s exit: %v", ra, err)
		return
	}
	defer func() { _ = closer.Close() }()

	for {
		time.Sleep(cfg.idle / 3)
		if since := time.Since(ac.lastRead()); since > cfg.idle {
			log.Printf("server: tunnel from %s idle for %s, closing", ra, since.Round(time.Second))
			return
		}
	}
}

// startExit stands up the configured exit data plane over the (activity-tracked)
// carrier and returns a Closer that tears it down. Both exits read the carrier via
// ac, so the idle watchdog sees client activity either way.
func startExit(ac net.Conn, ra net.Addr, cfg exitCfg) (io.Closer, error) {
	if cfg.mode == "kernel" {
		r, err := kernelexit.New(ac, cfg.wgPort)
		if err != nil {
			return nil, err
		}
		r.Start()
		return r, nil
	}
	gw, err := wggw.New(ac, wggw.Config{
		ServerPrivKey: cfg.wgPriv,
		Peers:         cfg.peers,
		MTU:           cfg.mtu,
		Shape:         cfg.shape,
		Logf:          log.Printf,
	})
	if err != nil {
		return nil, err
	}
	return gw, nil
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

// kernelPeerReconciler returns a function that reloads the peers file and makes
// the kernel wg interface's peer set match it. Re-reading on each call is what
// makes SIGHUP pick up edits.
func kernelPeerReconciler(cli kernelexit.WGClient, ifname, peersPath string) func() error {
	return func() error {
		wp, err := loadWGPeers(peersPath)
		if err != nil {
			return err
		}
		peers := make([]kernelexit.Peer, len(wp))
		for i, p := range wp {
			peers[i] = kernelexit.Peer{PubKeyHex: p.PubKey, AllowedIP: p.AllowedIP}
		}
		if err := kernelexit.Reconcile(cli, ifname, peers); err != nil {
			return err
		}
		log.Printf("server: reconciled %d peer(s) on %s", len(peers), ifname)
		return nil
	}
}

// watchSIGHUP re-runs reconcile whenever the process receives SIGHUP, logging but
// not dying on a reconcile error (a bad edit shouldn't drop live tunnels).
func watchSIGHUP(reconcile func() error, ifname string) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for range ch {
			log.Printf("server: SIGHUP — reloading peers for %s", ifname)
			if err := reconcile(); err != nil {
				log.Printf("server: reload failed (keeping previous peers): %v", err)
			}
		}
	}()
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
