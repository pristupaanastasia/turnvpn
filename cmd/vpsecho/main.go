// Command vpsecho is a minimal UDP echo server to run on your foreign VPS.
//
// It exists only to validate the relay path in the probe: every datagram it
// receives, it sends straight back to the sender. In the real system this port
// will instead hand packets to WireGuard, but for the feasibility test an echo
// is enough to prove the TURN relay reaches an arbitrary external peer.
//
// Run on the VPS:
//
//	./vpsecho -listen 0.0.0.0:51822
//
// Make sure that UDP port is open in the VPS firewall / security group.
//
// Shutdown: SIGINT (Ctrl+C) and SIGTERM close the socket and drain the serve
// loop within a bounded grace window (see internal/graceful). SIGKILL cannot be
// intercepted by any process — that is an OS guarantee, not an omission.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/pristupaanastasia/turnvpn/internal/graceful"
)

const shutdownGrace = 5 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "0.0.0.0:51822", "UDP address to listen on")
	flag.Parse()

	addr, err := net.ResolveUDPAddr("udp4", *listen)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	slog.Info("listening", "addr", conn.LocalAddr())

	r := graceful.New()
	defer r.Stop()

	// On shutdown, close the socket so the blocked ReadFromUDP in serve
	// returns and the loop can exit cleanly.
	r.Go(func(ctx context.Context) {
		<-ctx.Done()
		slog.Info("shutdown signal received, closing socket")
		conn.Close()
	})

	// The serve loop is a tracked goroutine so Wait blocks until it drains.
	r.Go(func(ctx context.Context) { serve(ctx, conn) })

	if err := r.Wait(shutdownGrace); err != nil {
		return err // ErrShutdownTimeout if the loop didn't drain in time
	}
	slog.Info("stopped")
	return nil
}

// serve echoes every datagram back to its sender until ctx is cancelled (which
// closes the socket and surfaces as a read error).
func serve(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return // expected: our own Close during shutdown
			}
			slog.Warn("read", "err", err)
			continue
		}
		slog.Info("recv", "bytes", n, "from", from.String())
		if _, err := conn.WriteToUDP(buf[:n], from); err != nil {
			slog.Warn("echo write", "to", from.String(), "err", err)
		}
	}
}
