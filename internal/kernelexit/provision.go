package kernelexit

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

// Peer is one authorized client in the kernel WireGuard interface: its public key
// (turnvpn stores keys as hex; kernel wg wants base64) and the tunnel address it
// is allowed to use.
type Peer struct {
	PubKeyHex string
	AllowedIP string // CIDR, e.g. 10.6.0.2/32
}

// WGClient is the subset of the `wg` CLI the reconciler needs. It is an interface
// so the reconcile logic is testable without a real kernel interface; the real
// implementation (wgExec) shells out to `wg`, which only works on the Linux VPS.
type WGClient interface {
	// Dump returns the set of peer public keys (base64) currently on the
	// interface.
	Dump(ifname string) (map[string]bool, error)
	// Set runs `wg set <ifname> <args...>`.
	Set(ifname string, args ...string) error
}

// hexToWGKey converts a 32-byte hex key (turnvpn/wgkey format) to the base64 the
// `wg` CLI expects.
func hexToWGKey(h string) (string, error) {
	b, err := hex.DecodeString(strings.TrimSpace(h))
	if err != nil {
		return "", fmt.Errorf("bad hex key: %w", err)
	}
	if len(b) != 32 {
		return "", fmt.Errorf("key must be 32 bytes, got %d", len(b))
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Reconcile makes the interface's peer set match desired: it (re)applies every
// desired peer (idempotent, so allowed-ips stay correct) and removes any peer on
// the interface that is no longer desired. Call it at startup and on SIGHUP to add
// or revoke clients without restarting. The interface itself (key, listen port,
// address, nft) is owned by scripts/kernel-exit-setup.sh, not here.
func Reconcile(cli WGClient, ifname string, desired []Peer) error {
	current, err := cli.Dump(ifname)
	if err != nil {
		return fmt.Errorf("kernelexit: read %s peers: %w", ifname, err)
	}

	want := make(map[string]struct{}, len(desired))
	for _, p := range desired {
		b64, err := hexToWGKey(p.PubKeyHex)
		if err != nil {
			return fmt.Errorf("kernelexit: peer %s: %w", p.PubKeyHex, err)
		}
		want[b64] = struct{}{}
		if err := cli.Set(ifname, "peer", b64, "allowed-ips", p.AllowedIP); err != nil {
			return fmt.Errorf("kernelexit: add peer: %w", err)
		}
	}
	for pub := range current {
		if _, ok := want[pub]; !ok {
			if err := cli.Set(ifname, "peer", pub, "remove"); err != nil {
				return fmt.Errorf("kernelexit: remove stale peer: %w", err)
			}
		}
	}
	return nil
}

// wgExec is the real WGClient: it shells out to the `wg` CLI on the VPS.
type wgExec struct{}

// NewWGCLI returns a WGClient backed by the system `wg` command.
func NewWGCLI() WGClient { return wgExec{} }

func (wgExec) Set(ifname string, args ...string) error {
	full := append([]string{"set", ifname}, args...)
	out, err := exec.Command("wg", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("wg %s: %v: %s", strings.Join(full, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func (wgExec) Dump(ifname string) (map[string]bool, error) {
	out, err := exec.Command("wg", "show", ifname, "dump").Output()
	if err != nil {
		return nil, fmt.Errorf("wg show %s dump: %w", ifname, err)
	}
	return parseDump(out), nil
}

// parseDump extracts peer public keys from `wg show <if> dump`. The first line is
// the interface itself (private key, public key, listen port, fwmark); every
// subsequent line is a peer whose first field is its public key (base64).
func parseDump(out []byte) map[string]bool {
	peers := make(map[string]bool)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for first := true; sc.Scan(); first = false {
		if first {
			continue // interface line
		}
		fields := strings.Fields(sc.Text())
		if len(fields) > 0 && fields[0] != "" {
			peers[fields[0]] = true
		}
	}
	return peers
}
