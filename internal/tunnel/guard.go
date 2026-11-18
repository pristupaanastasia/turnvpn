package tunnel

import (
	"fmt"
	"net"
)

// TargetAllowed reports whether the server may relay to ip. The default policy
// rejects loopback, private (RFC1918 / ULA), link-local (incl. the cloud-metadata
// address 169.254.169.254), multicast and unspecified addresses — this closes
// SSRF from a client into the VPS host and its internal / cloud-metadata network.
// It is a package var so tests (which relay to loopback origins) can relax it.
var TargetAllowed = func(ip net.IP) bool { return !isBlockedIP(ip) }

func isBlockedIP(ip net.IP) bool {
	return ip == nil ||
		ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

// resolveSafe resolves target ("host:port") to a concrete "ip:port" that passes
// TargetAllowed, or errors if the host cannot be resolved or every resolved
// address is blocked. Returning the concrete IP for the caller to dial also
// closes the DNS-rebinding TOCTOU between this check and the dial.
func resolveSafe(target string) (string, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", fmt.Errorf("bad target %q: %w", target, err)
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if ips, err = net.LookupIP(host); err != nil {
		return "", fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if TargetAllowed(ip) {
			return net.JoinHostPort(ip.String(), port), nil
		}
	}
	return "", fmt.Errorf("target %q not permitted (blocked address range)", target)
}
