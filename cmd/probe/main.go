// Command probe is a feasibility test, not a VPN.
//
// It answers the single question the whole project depends on:
//
//	Will a given TURN server relay UDP datagrams to an ARBITRARY external
//	peer (your foreign VPS), or does it refuse CreatePermission for peers
//	outside its own network?
//
// How it works:
//  1. Allocate a relayed transport address on the TURN server.
//  2. CreatePermission for the VPS IP, explicitly, and read the server's answer.
//     This separates two failures a later timeout would otherwise merge:
//       - the server forbids the peer (403) -> hard IP whitelist, path dead;
//       - the server grants it -> data is allowed to try, so a later timeout
//         means the egress or return leg is blocked, not the permission.
//  3. Send a probe payload THROUGH the relay toward the VPS echo server.
//  4. Wait for the echo to come back through the relay.
//
// If step 4 succeeds, the core assumption holds and the VPN is worth building.
// If step 2 is rejected, the server refuses external peers outright. If step 2
// passes but step 4 times out, the peer is permitted yet data doesn't traverse
// — the case worth probing with a VPS-initiated ("knock-first") test next.
//
// Run the companion `vpsecho` on your VPS first (see cmd/vpsecho).
//
// Shutdown: SIGINT/SIGTERM (and the round-trip timeout) cancel the wait and
// release the allocation, socket and client in reverse order via defer. SIGKILL
// cannot be caught by any process, so it leaves the server-side allocation to
// expire on its own TTL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pion/logging"
	"github.com/pion/stun/v3"
	"github.com/pion/turn/v4"

	"github.com/pristupaanastasia/turnvpn/internal/dotenv"
)

type config struct {
	turnServer string
	username   string
	password   string
	realm      string
	vpsAddr    string
	timeout    time.Duration
}

func main() {
	if err := run(); err != nil {
		slog.Error("probe failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	// Cancel on SIGINT/SIGTERM or after the round-trip timeout, whichever
	// comes first. One context drives both the wait and resource teardown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	vps, err := net.ResolveUDPAddr("udp4", cfg.vpsAddr)
	if err != nil {
		return fmt.Errorf("resolve -vps %q: %w", cfg.vpsAddr, err)
	}

	// Local UDP socket used to speak STUN/TURN to the server.
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("listen local udp: %w", err)
	}
	defer conn.Close()

	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: cfg.turnServer,
		TURNServerAddr: cfg.turnServer,
		Conn:           conn,
		Username:       cfg.username,
		Password:       cfg.password,
		Realm:          cfg.realm,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	})
	if err != nil {
		return fmt.Errorf("new turn client: %w", err)
	}
	defer client.Close()

	if err := client.Listen(); err != nil {
		return fmt.Errorf("turn client listen: %w", err)
	}

	slog.Info("allocating relay", "server", cfg.turnServer)
	relayConn, err := client.Allocate()
	if err != nil {
		return fmt.Errorf("allocate (creds wrong/expired, or server refused): %w", err)
	}
	defer relayConn.Close()
	slog.Info("allocated", "relay_addr", relayConn.LocalAddr().String())

	// Stage A — explicit CreatePermission. pion otherwise installs the
	// permission lazily on the first WriteTo, so a server that forbids the peer
	// (403) looks identical to one that silently drops data: both just time out.
	// Issuing it ourselves separates "peer forbidden" (hard IP whitelist, dead
	// here) from "permission granted but packets vanish" (egress/return-leg
	// blocked — the case the knock-first test targets).
	slog.Info("creating permission for peer (separates hard whitelist from silent drop)", "peer", vps.String())
	if err := createPermission(ctx, client, vps); err != nil {
		var turnErr *stun.TurnError
		switch {
		case errors.As(err, &turnErr):
			slog.Warn("PERMISSION REJECTED by server",
				"code", int(turnErr.ErrorCodeAttr.Code),
				"reason", string(turnErr.ErrorCodeAttr.Reason))
			slog.Warn("verdict: server forbids this external peer IP at the permission stage — a hard peer whitelist; dead for this server, and knocking from the VPS won't help")
			return fmt.Errorf("create permission rejected: %w", err)
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			slog.Warn("PERMISSION: no response before timeout/cancel", "err", err)
			slog.Warn("verdict: server never answered CreatePermission — likely silently dropping control traffic for external peers")
			return fmt.Errorf("create permission timed out: %w", err)
		default:
			slog.Warn("PERMISSION failed", "err", err)
			return fmt.Errorf("create permission: %w", err)
		}
	}
	slog.Info("PERMISSION ACCEPTED: server allows this external peer IP")

	// Stage B — does data actually traverse the relay both ways?
	return roundTrip(ctx, relayConn, vps, cfg.timeout)
}

// createPermission issues one explicit CreatePermission and surfaces the
// server's own answer instead of letting pion bury it in a later WriteTo. It
// retries once to absorb a stale nonce (the only benign error here — pion
// refreshes the nonce in place), stops immediately on a *stun.TurnError (a real
// server rejection such as 403 Forbidden), and honours ctx so a server that
// silently drops the request cannot hang the probe past its timeout.
func createPermission(ctx context.Context, client *turn.Client, peer net.Addr) error {
	done := make(chan error, 1)
	go func() {
		var err error
		for i := 0; i < 2; i++ {
			if err = client.CreatePermission(peer); err == nil {
				break
			}
			var turnErr *stun.TurnError
			if errors.As(err, &turnErr) {
				break // definitive server rejection — retrying won't change it
			}
			// benign stale-nonce: pion refreshed it in place, a second try lands
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// roundTrip installs a permission for vps, sends a probe payload through the
// relay, and waits for the echo. The reader goroutine exits when relayConn is
// closed by the deferred teardown in run.
func roundTrip(ctx context.Context, relayConn net.PacketConn, vps *net.UDPAddr, timeout time.Duration) error {
	got := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1500)
		for {
			n, from, err := relayConn.ReadFrom(buf)
			if err != nil {
				return // unblocked by the read deadline below, or teardown
			}
			select {
			case got <- fmt.Sprintf("%q from %s", string(buf[:n]), from):
			default:
			}
		}
	}()
	// Stop the reader deterministically before returning: an immediate read
	// deadline unblocks ReadFrom without closing the caller-owned relayConn.
	defer func() {
		_ = relayConn.SetReadDeadline(time.Now())
		wg.Wait()
	}()

	payload := []byte("turnvpn-probe-" + time.Now().Format("150405"))
	// WriteTo on a TURN relay installs the peer permission/channel and forwards
	// the datagram. Send repeatedly: CreatePermission and ChannelBind complete a
	// beat after the first write, so a single early packet can race ahead of them
	// and be dropped. Re-sending every second guarantees packets flow once the
	// binding is up, without waiting on pion's internal timing.
	slog.Info("repeated send toward vps through the relay (data round-trip)", "vps", vps.String())
	send := func() error {
		if _, err := relayConn.WriteTo(payload, vps); err != nil {
			return fmt.Errorf("writeTo relay->vps (permission refused?): %w", err)
		}
		return nil
	}
	if err := send(); err != nil {
		return err
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	slog.Info("waiting for echo through relay", "timeout", timeout.String())
	for {
		select {
		case msg := <-got:
			slog.Info("SUCCESS: relay delivered to external peer and returned", "echo", msg)
			slog.Info("verdict: this TURN server WILL relay to an arbitrary external VPS — build it")
			return nil
		case <-ticker.C:
			if err := send(); err != nil {
				return err
			}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				slog.Warn("TIMEOUT: no echo returned (permission was accepted earlier)")
				slog.Warn("verdict: peer is permitted but data doesn't traverse — egress to the VPS or the return leg is blocked (or vpsecho is down). Next: the VPS-initiated knock-first test")
				return fmt.Errorf("no echo within %s", timeout)
			}
			slog.Info("cancelled by signal")
			return ctx.Err()
		}
	}
}

// parseFlags builds the config from flags, falling back to a .env file and the
// environment so short-lived TURN credentials never have to appear in CLI
// arguments or shell history. Precedence: explicit flag > env var > .env file.
//
// Env var names (see .env.example):
//
//	TURN_SERVER  TURN_USER  TURN_PASS  TURN_REALM  TURN_VPS
func parseFlags() (config, error) {
	// Populate the environment from .env (does not overwrite real env vars).
	if err := dotenv.Load(".env"); err != nil {
		return config{}, fmt.Errorf("load .env: %w", err)
	}

	var cfg config
	flag.StringVar(&cfg.turnServer, "turn", os.Getenv("TURN_SERVER"), "TURN server host:port (e.g. 90.156.234.252:19302)")
	flag.StringVar(&cfg.username, "user", os.Getenv("TURN_USER"), "TURN short-term username")
	flag.StringVar(&cfg.password, "pass", os.Getenv("TURN_PASS"), "TURN short-term password/credential")
	flag.StringVar(&cfg.realm, "realm", os.Getenv("TURN_REALM"), "TURN realm (often empty for short-term creds)")
	flag.StringVar(&cfg.vpsAddr, "vps", os.Getenv("TURN_VPS"), "VPS echo server host:port reachable from the relay")
	flag.DurationVar(&cfg.timeout, "timeout", 10*time.Second, "round-trip timeout")
	flag.Parse()

	for name, v := range map[string]string{"turn": cfg.turnServer, "user": cfg.username, "pass": cfg.password, "vps": cfg.vpsAddr} {
		if v == "" {
			return cfg, fmt.Errorf("missing required -%s (or set it in .env / env)", name)
		}
	}
	return cfg, nil
}
