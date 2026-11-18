# turnvpn

Tunnel ordinary internet traffic through the **whitelisted** TURN relays of VK
calls, so a machine stuck in a whitelist-only network ("суверенный режим") can
still reach the open internet. The traffic looks like a WebRTC call to the
network. Personal research / educational.

## How it works

The TURN servers behind VK's calls are whitelisted (the approved app needs them).
They forward UDP to one peer you bind — if that peer is your own VPS out on the
open internet, the relay becomes a stepping stone out of the whitelist. The relay
only forwards packets that look like WebRTC media, so we wrap everything in
**DTLS** (what real calls use). On top we run a reliable, multiplexed transport
and expose it locally as a SOCKS5 proxy.

```
  machine inside the whitelist                               open internet
┌──────────────────────────────┐                          ┌───────────────┐
│ browser / app                │                          │  any website  │
│   │ SOCKS5  127.0.0.1:1080   │                          └──────▲────────┘
│   ▼                          │                                 │ TCP
│ cmd/client                   │        VK TURN relay     ┌──────┴────────┐
│   pool of N tunnels, each:   │      (whitelisted IP)    │ cmd/server    │
│   smux → KCP → DTLS ─────────┼──► ChannelData ──► UDP ─►│  (your VPS)   │
│   over a pion/turn alloc     │   (looks like media)     │ DTLS→KCP→smux │
└──────────────────────────────┘                          │ → dial target │
                                                          └───────────────┘
```

Credentials for the relay are read from a real call's join descriptor
(`turn_server {username, credential, urls}`), fetched via the VK/OK REST chain
(`get_anonym_token` → `getAnonymousToken` → `anonymLogin` → `joinConversationByLink`).
Getting the per-call token requires passing VK's "not a robot" check once per
~10 min — **human-in-the-loop** (you solve it in a browser). turnvpn does **not**
implement captcha solving.

## Data security in the DTLS packets

Each tunnel datagram is a nest of layers; from outside in:

```
┌ TURN ChannelData — relay addressing; indistinguishable from WebRTC media ─────┐
│ ┌ DTLS 1.2 record — AEAD (AES-128-GCM), ECDHE key exchange ─────────────────┐ │
│ │ ┌ KCP segment — reliable ARQ (no crypto; DTLS already encrypts) ────────┐ │ │
│ │ │ ┌ smux frame — stream multiplexing ─────────────────────────────────┐ │ │ │
│ │ │ │  "host:port" header + the proxied TCP stream                       │ │ │ │
│ │ │ │  (usually itself TLS/HTTPS, end-to-end to the real site)           │ │ │ │
│ │ │ └────────────────────────────────────────────────────────────────────┘ │ │ │
│ │ └──────────────────────────────────────────────────────────────────────┘ │ │
│ └────────────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────────────┘
```

**What DTLS gives us**

- **Confidentiality + integrity** of the tunneled bytes. The cipher is
  `ECDHE-ECDSA-AES128-GCM` — an AEAD, so every record is encrypted *and*
  tamper-evident; ephemeral ECDHE gives **forward secrecy** (a later key
  compromise doesn't decrypt past traffic). The relay, the ISP and anyone on the
  path see only ciphertext.
- **Disguise.** DTLS is exactly what WebRTC media is wrapped in, so the relay
  forwards it; raw/un-wrapped bytes are silently dropped by the relay's data
  plane. The wrapper is what makes the whole approach work at all.
- KCP deliberately uses **no** crypto (`NoneBlockCrypt`): DTLS already encrypts
  the datagram, so a second cipher would only waste CPU.

**What DTLS here does *not* give — and why it's still safe**

- **No endpoint authentication.** We use self-signed certs with
  `InsecureSkipVerify`, so the DTLS layer confirms *encryption*, not *who* the
  peer is. On its own that leaves it open to an active man-in-the-middle who can
  impersonate the VPS. We accept this because:
  - the party in the middle *is* the VK relay, which forwards blindly and has no
    incentive to mount an active MITM; and
  - **defense in depth**: the bytes inside are independently protected by the
    application layer. Browsing is HTTPS, so there is a second, *authenticated*
    TLS session end-to-end to the real website — the VPS and the relay only ever
    see that inner TLS ciphertext, never your plaintext. If you run **WireGuard**
    over the tunnel instead of a browser proxy, WireGuard adds its own
    authenticated encryption with pinned public keys, closing the MITM gap
    entirely.
- **It hides content, not the tunnel's existence.** Payloads are encrypted, but
  sustained high-volume traffic through a "call", and a TURN allocation that
  egresses to an external IP, are detectable by traffic analysis. Encryption
  protects *what* you send, not the *fact* that a tunnel is running. Reduce
  footprint (bitrate shaping, endpoint rotation); do not expect invisibility.

**Net:** your actual traffic is encrypted twice over an untrusted relay — once by
DTLS (transport + disguise) and once by the app layer (HTTPS/WireGuard, which also
authenticates the far end). The relay carries bytes it cannot read or usefully alter.

## Components

- `cmd/client` — local SOCKS5 proxy; maintains a pool of tunnels, each
  `smux → KCP → DTLS` over a `pion/turn` allocation. Background manager refreshes
  each tunnel's credentials before they expire.
- `cmd/server` — runs on the VPS: terminates DTLS, demuxes streams, dials each
  stream's target on the open internet, pipes it back.
- `internal/okapi`, `internal/vkapi` — the VK/OK REST chain that yields
  `turn_server` credentials.
- `internal/mux` — KCP + smux over a single DTLS connection.
- `internal/socks` — minimal SOCKS5 (CONNECT) and the per-stream target header.
- `internal/captcha` — a user-supplied captcha fetcher that plugs into the
  client's `CredsFunc` seam. **Not part of the core**; turnvpn itself does no
  captcha solving.

## Usage

On the VPS (open the UDP port in the firewall):

```sh
go build -o server ./cmd/server
./server -listen 0.0.0.0:56000
```

On your machine (with a per-call token captured from a browser call):

```sh
go build -o client ./cmd/client
TURN_CALL_TOKEN='<call-token>' ./client \
  -peer <VPS_IP>:56000 \
  -vk-link <token from vk.ru/call/join/...> \
  -tunnels 2
```

Then point the browser (or system) at SOCKS5 `127.0.0.1:1080` with remote DNS.
Keep the browser call alive while the tunnel is up. Without `-token`, the client
falls back to the pluggable credential provider wired in `main` (e.g. a captcha
fetcher) — which you supply.

## Status

Vector 1 (TURN relay + DTLS + SOCKS5) is **working end-to-end**: real HTTPS
browsing exits via the VPS over whitelisted VK infrastructure, with a tunnel pool
and proactive credential refresh. See `PLAN.md` and `LAB.md` for the full history,
including the earlier (removed) media-RTP approach kept in git history.

## Disclaimer

For education and personal censorship-circumvention research. Use only
infrastructure and accounts you are authorised to use.
