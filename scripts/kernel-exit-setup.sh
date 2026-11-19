#!/usr/bin/env bash
# kernel-exit-setup.sh — stand up the variant-B "kernel exit" on a Linux VPS.
#
# It brings up a kernel WireGuard interface (wg0) with the server key and the
# authorized peers, enables forwarding, and installs an nftables ruleset that
# MASQUERADEs tunnel traffic to the internet AND — critically — drops any
# forwarded packet aimed at loopback, link-local (incl. 169.254.169.254 cloud
# metadata) or private ranges. That egress filter is what keeps the kernel exit
# from being an open proxy into the VPS's own host and internal network; the Go
# server (-exit kernel) only relays datagrams and enforces none of this itself.
#
#   sudo WG_KEY=/etc/turnvpn/wg.key WG_PEERS=/etc/turnvpn/peers \
#        ./scripts/kernel-exit-setup.sh up
#   sudo ./scripts/kernel-exit-setup.sh down      # tear everything down
#
# Then run the relay:  turnvpn-server -exit kernel -wg-port "$WG_PORT" -secret ...
#
# Env (all optional except WG_KEY/WG_PEERS for `up`):
#   WG_IF      interface name           (default wg0)
#   WG_PORT    kernel wg listen port    (default 51820)  — pass the same to -wg-port
#   WG_ADDR    server address on wg0    (default 10.6.0.1/24)
#   WG_SUBNET  tunnel subnet to NAT     (default 10.6.0.0/24)
#   EGRESS_IF  WAN interface           (default: auto from the default route)
#   WG_KEY     server private key: a file, or the hex key in the env var itself
#   WG_PEERS   peers file, "pubkey allowed-ip" per line (same format as -wg-peers)
set -euo pipefail

WG_IF="${WG_IF:-wg0}"
WG_PORT="${WG_PORT:-51820}"
WG_ADDR="${WG_ADDR:-10.6.0.1/24}"
WG_SUBNET="${WG_SUBNET:-10.6.0.0/24}"
NFT_TABLE="turnvpn"

# Ranges a tunnel client must never be able to reach THROUGH the exit. The first
# line is the whole point: 169.254.0.0/16 covers the 169.254.169.254 cloud
# metadata endpoint, 127/8 is the VPS's own loopback, the RFC1918 blocks are its
# internal network.
BLOCK4="169.254.0.0/16, 127.0.0.0/8, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 192.0.0.0/24, 198.18.0.0/15, 0.0.0.0/8"

need_root() { [ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)"; exit 1; }; }

detect_egress() {
	if [ -n "${EGRESS_IF:-}" ]; then echo "$EGRESS_IF"; return; fi
	ip route show default 2>/dev/null | awk '/default/ {print $5; exit}'
}

read_key() {
	# WG_KEY may be a path or the key itself.
	if [ -f "${WG_KEY:-}" ]; then tr -d '[:space:]' < "$WG_KEY"; else printf '%s' "${WG_KEY:-}" | tr -d '[:space:]'; fi
}

up() {
	need_root
	: "${WG_KEY:?set WG_KEY (file or hex key)}"
	: "${WG_PEERS:?set WG_PEERS (peers file: 'pubkey allowed-ip' per line)}"
	[ -f "$WG_PEERS" ] || { echo "peers file not found: $WG_PEERS"; exit 1; }
	local egress; egress="$(detect_egress)"
	[ -n "$egress" ] || { echo "cannot detect egress interface; set EGRESS_IF"; exit 1; }
	echo "kernel exit: if=$WG_IF port=$WG_PORT addr=$WG_ADDR subnet=$WG_SUBNET egress=$egress"

	# 1. Forwarding on.
	sysctl -qw net.ipv4.ip_forward=1

	# 2. Interface. The wg private key is hex (wgkey format); kernel wg wants
	#    base64, so convert via `wg pubkey`-compatible tooling. turnvpn keys are
	#    Curve25519 hex; xxd -r -p | base64 produces the base64 wg expects.
	ip link del "$WG_IF" 2>/dev/null || true
	ip link add "$WG_IF" type wireguard
	local keyb64; keyb64="$(read_key | xxd -r -p | base64)"
	wg set "$WG_IF" listen-port "$WG_PORT" private-key <(printf '%s' "$keyb64")
	ip addr add "$WG_ADDR" dev "$WG_IF"
	ip link set "$WG_IF" up

	# 3. Peers (hex pubkey -> base64).
	local pub ip n=0
	while read -r pub ip _; do
		case "$pub" in ''|\#*) continue;; esac
		wg set "$WG_IF" peer "$(printf '%s' "$pub" | xxd -r -p | base64)" allowed-ips "$ip"
		n=$((n+1))
	done < "$WG_PEERS"
	echo "configured $n peer(s)"

	# 4. nftables: SSRF egress filter + MASQUERADE.
	nft delete table inet "$NFT_TABLE" 2>/dev/null || true
	nft -f - <<EOF
table inet $NFT_TABLE {
	chain forward {
		type filter hook forward priority filter; policy accept;
		# SSRF guard: anything from the tunnel aimed at the host/internal net is dropped.
		iifname "$WG_IF" ip daddr { $BLOCK4 } drop
		# Return traffic for established flows.
		oifname "$WG_IF" ct state established,related accept
		# New outbound flows from the tunnel to the real internet.
		iifname "$WG_IF" accept
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr $WG_SUBNET oifname "$egress" masquerade
	}
}
EOF
	echo "nft table inet/$NFT_TABLE installed (egress filter + masquerade)"
	echo "up. start the relay:  turnvpn-server -exit kernel -wg-port $WG_PORT ..."
}

down() {
	need_root
	nft delete table inet "$NFT_TABLE" 2>/dev/null || true
	ip link del "$WG_IF" 2>/dev/null || true
	echo "down: removed $WG_IF and nft table inet/$NFT_TABLE (ip_forward left as-is)"
}

case "${1:-}" in
	up)   up ;;
	down) down ;;
	*)    echo "usage: $0 up|down"; exit 2 ;;
esac
