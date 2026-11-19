# Kernel exit (variant B)

The server has two interchangeable exit data planes behind the same carrier
(TURN + DTLS) and the same client WireGuard identity. The client cannot tell them
apart; pick one on the server with `-exit`.

| | `-exit userspace` (default) | `-exit kernel` (B) |
|---|---|---|
| WireGuard termination | wireguard-go in process | kernel `wg0` |
| Routing / NAT | gVisor netstack, explicit `net.Dial` | kernel routing + nftables MASQUERADE |
| Root | no | **yes** (interface + nft) |
| Portability | any OS | Linux only |
| SSRF safety | **by design** (explicit dial + filter) | **only** via the nft egress filter below |
| Per-client cost | ~2–5 MB (own netstack) | ~0.2 MB (kernel peer + relay socket) |
| Throughput | userspace (~150–250 Mbit/s·core) | kernel (~1–2 Gbit/s·core) |

Why both: the userspace exit is the safe, portable default; the kernel exit scales
far better (memory and throughput) when you control the VPS and accept root. The
Go server in kernel mode is a thin relay — it shuttles raw WireGuard datagrams
between each DTLS carrier and the kernel's wg port on localhost and enforces no
policy itself.

## ⚠️ Security: the egress filter is not optional

With MASQUERADE the kernel will forward tunnel traffic **anywhere**, including the
VPS's own loopback, its internal network, and `169.254.169.254` (cloud metadata).
That is an open proxy into the host. `scripts/kernel-exit-setup.sh` installs an
nftables rule that **drops** forwarded traffic from the tunnel to loopback,
link-local and RFC1918 ranges. Do **not** run `-exit kernel` without it.

## Setup (on the VPS, as root)

```sh
# server key (hex) and peers file, same formats as the userspace exit uses
sudo WG_KEY=/etc/turnvpn/wg.key WG_PEERS=/etc/turnvpn/peers \
     ./scripts/kernel-exit-setup.sh up

# then run the relay (no -wg-key/-wg-peers needed; those live in wg0 now)
turnvpn-server -exit kernel -wg-port 51820 -secret "$TURNVPN_SECRET" -listen 0.0.0.0:56000

# tear the kernel exit back down
sudo ./scripts/kernel-exit-setup.sh down
```

The peers file is `pubkey allowed-ip` per line (hex pubkey, e.g.
`a82f… 10.6.0.2/32`) — the same file the userspace exit reads with `-wg-peers`.
The script converts the hex key/pubkeys to the base64 the kernel expects.

Env knobs: `WG_IF`, `WG_PORT`, `WG_ADDR`, `WG_SUBNET`, `EGRESS_IF` (auto-detected
from the default route if unset). Pass the same port to the relay's `-wg-port`.

## What is and isn't done yet

Done (this stage):
- `internal/kernelexit` relay + `-exit kernel` wiring (default stays `userspace`).
- `wg0` + nft (MASQUERADE **and** SSRF egress filter) setup/teardown script.

Not yet (later stages):
- Dynamic peer provisioning: peers are static (from the file at setup). Adding a
  client still means editing the file and re-running `up` (or `wg set wg0 peer`).
- `SO_REUSEPORT` sharding of the DTLS listener — a single UDP socket caps aggregate
  throughput well below what the kernel exit can push.
- Per-peer bandwidth shaping via `tc` (the userspace `-shape` has no kernel-mode
  equivalent yet).

## Verifying on the VPS

```sh
wg show wg0                       # peers, handshakes, transfer
sudo nft list table inet turnvpn  # egress filter + masquerade
sudo conntrack -L | head          # live forwarded flows
```
