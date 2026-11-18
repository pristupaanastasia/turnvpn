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
│ ┌ DTLS 1.2 record — AEAD ChaCha20-Poly1305, ECDHE-PSK (mutually authed) ─────┐ │
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

- **Mutual authentication (pre-shared key).** Both endpoints share a secret
  (`-secret` / `TURNVPN_SECRET`) and the suite is **ECDHE-PSK**: the handshake
  only completes if each side derives the same key, so an attacker without the
  secret can neither pose as the VPS to the client (**no MITM**) nor use the VPS
  as an open proxy. See `internal/authdtls`.
- **Confidentiality + integrity** of the tunneled bytes. The suite is
  `ECDHE-PSK-WITH-CHACHA20-POLY1305` — an AEAD, so every record is encrypted
  *and* tamper-evident; the ephemeral ECDHE part gives **forward secrecy** (a
  later secret compromise does not decrypt past traffic). The relay, the ISP and
  anyone on the path see only ciphertext.
- **Disguise.** DTLS is exactly what WebRTC media is wrapped in, so the relay
  forwards it; raw/un-wrapped bytes are silently dropped by the relay's data
  plane. The wrapper is what makes the whole approach work at all.
- KCP deliberately uses **no** crypto (`NoneBlockCrypt`): DTLS already encrypts
  the datagram, so a second cipher would only waste CPU.

**Defense in depth.** The DTLS layer already authenticates both ends, but the
payload is *also* independently protected by the application layer: browsing is
HTTPS, so there is a second, authenticated TLS session end-to-end to the real
website — the VPS and the relay only ever see that inner TLS ciphertext, never
your plaintext. So your traffic is encrypted twice over the untrusted relay, and
the only party that ever sees a cleartext destination is your own VPS.

**What it still does *not* hide: the tunnel's existence.** Payloads are encrypted
and authenticated, but sustained high-volume traffic through a "call", and a TURN
allocation that egresses to an external IP, are detectable by traffic analysis.
Encryption protects *what* you send, not the *fact* that a tunnel is running.
Reduce footprint (bitrate shaping, endpoint rotation); do not expect invisibility.

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

Pick a shared secret and set it on **both** ends (`-secret` or `TURNVPN_SECRET`);
it mutually authenticates the tunnel so the exit is not an open proxy.

On the VPS (open the UDP port in the firewall):

```sh
go build -o server ./cmd/server
TURNVPN_SECRET='<shared secret>' ./server -listen 0.0.0.0:56000
```

On your machine (with a per-call token captured from a browser call):

```sh
go build -o client ./cmd/client
TURNVPN_SECRET='<same shared secret>' TURN_CALL_TOKEN='<call-token>' ./client \
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
