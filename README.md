# turnvpn

Experiment: tunneling traffic through a third-party TURN relay (whose IP sits on
a whitelist) to get around whitelist-only network modes. Personal research.

## Background

During internet shutdowns in Russia, networks are often switched to a
**whitelist** ("белый список" / "суверенный режим"): instead of blocking a list
of banned sites, the network blocks *everything* and allows only a small set of
approved domains and IPs — typically large Russian services (government portals,
banks, VK, etc.). Everything else, including the open internet and ordinary VPNs,
is simply unreachable.

The idea here is censorship circumvention: the TURN servers behind a whitelisted
service's voice/video calls (WebRTC) are themselves whitelisted, because the
approved app needs them to work. If such a relay can be made to forward traffic
to an external peer, it becomes a stepping stone out of the whitelist that looks
like a legitimate call to the network. The goal is restoring access to the open
internet for a single user, not attacking anyone's infrastructure.

## Development stages

A bottom-up test ladder. Each rung is as local and cheap as possible and gates
the one above it — climb only while the rung below stays green.

**Local first — no VPS, no third-party server, just our machine:**

1. **Connection establishes.** Two local peers and a local TURN server on
   loopback complete a relayed allocation (`Allocate` + `CreatePermission`). This
   proves the plumbing with zero network trust.
   → `go test ./cmd/probe -run TestRoundTripLocal` — ✅ green.
2. **Data crosses the relay.** A datagram round-trips *through* that local relay:
   peer → relay → echo → relay → peer. Proves the send/receive path, not just
   setup. → same test asserts the echo returns — ✅ green.

**Against the real whitelisted relay:**

3. **Control plane.** The real TURN grants an allocation and accepts
   `CreatePermission` for an **arbitrary external** peer (our VPS) — so it is not
   a hard peer-IP whitelist. → `./probe` prints `PERMISSION ACCEPTED` — ✅.
4. **Data plane (make-or-break).** A datagram actually traverses the real
   relay ↔ external VPS, both directions. → `./probe` round-trip, cross-checked
   with `tcpdump` on the VPS — ❌ **blocked**: control passes but no data reaches
   an external IP; the relay's data plane is firewalled to the provider's own
   infrastructure. The VPS-initiated "knock-first" variant was tried too and also
   saw nothing. Stages below are unreachable through this relay (see Status).

**Tunnel build — only once stage 4 is green:** transport shim → WireGuard over
it → credential auto-refresh & resilience. See **Later milestones**.

> Stage 4 is ❌ for the **TURN-relay path (Vector 1)**: the VK/OK relay's data
> plane is firewalled to the provider's own infra. That path is dead. The live
> path is **Vector 2** below.

## Vector 2 — media RTP forwarding (the live path)

Instead of abusing the TURN relay as a generic UDP relay, ride the **media leg**
of a real VK/OK call: become a call participant and carry tunnel bytes inside the
video RTP. All traffic then looks like (and is) a whitelisted VK call.

### Make-or-break: does the SFU forward or transcode? → **FORWARD** (resolved)

The one question this vector lived or died on: does the SFU **re-encode** each
video stream (transcode → any bytes we hide in the frame are destroyed) or
**forward the encoded frames as-is** (payload survives)?

**Answer: it forwards, it does not transcode.** Two grounds:

1. **Cost/architecture (decisive).** Transcoding every sender's stream for every
   receiver in real time is infeasible at scale (N·M live re-encodes). Any
   group-call service this size is a **selective-forwarding SFU**: it terminates
   SRTP on each leg, forwards the *encoded* frames untouched, re-encrypts per
   receiver. It does not touch the pixels.
2. **Confirmed empirically (2026-11-17).** Joining as an anonymous guest, we
   injected a synthetic VP8 track with
   `pc.addTransceiver(canvasTrack, {direction: 'sendonly'})`; VK **renegotiated
   it and relayed it to the other participants** — accepted and forwarded, not
   re-encoded. (The resolution "normalization" we saw is the *sender-side*
   encoder choosing standard simulcast rungs, client-side — not the SFU.)

So **data crafted into a valid encoded VP8/H.264 frame reaches the peer intact.**

### How to actually put our bytes on the wire

A **browser cannot author the bitstream** — Chrome re-encodes whatever pixels you
hand it (a canvas), so from a browser you only get lossy pixel-steganography:
useless bandwidth. To put arbitrary bytes into the frame payload you need a
**native client (pion/webrtc) that builds the RTP/frames itself** — *those*
frames the SFU forwards as-is.

- **Cost of entry:** reverse the **`calls.okcdn.ru/fb.do`** signaling (OK's
  custom base64 "TopGun"-style join protocol) so a pion client can join the call.
  This is the heavy part.
- **Frame constraints:** the crafted frames must be *structurally* valid — real
  keyframes, correct VP8/H.264 framing, and they must obey the SFU's PLI
  (keyframe requests) and BWE (bitrate) signals or they get dropped. But the
  **payload inside a valid frame is not touched** — that is where our bytes ride.

### Vector 2 plan (stages)

1. **Signaling reverse (`fb.do`).** Capture and replay the OK/VK call join
   handshake: auth, room join, SDP exchange, ICE. Deliverable: a headless pion
   peer that joins a call and negotiates an audio+video (+RTP) session. *Hardest
   rung; everything else gates on it.*
2. **Pass-through proof.** From the pion peer, send frames whose payload carries a
   known marker; on a second controlled participant, confirm the exact bytes
   arrive (SFU forwarded them untouched). Confirms the make-or-break end-to-end
   with real crafted frames, not a browser canvas.
3. **Framing codec.** Pack tunnel datagrams into valid VP8/H.264 frames: keyframe
   cadence, sequencing, reassembly on the far side, FEC/ARQ for the drops BWE and
   loss will cause. Bidirectional (both legs are call participants).
4. **Transport + WireGuard.** Expose the media channel as a datagram transport;
   run WireGuard over it (same as Vector 1's milestones 1–2). VPS peer forwards
   to the open internet.
5. **Resilience + footprint.** Re-join on drop; shape bitrate to a plausible-call
   envelope; rotate endpoints. (Sustained heavy traffic through a call is
   inherently anomalous — reduce footprint, do not expect invisibility.)

## Status: milestone 0 — feasibility check

Before building anything VPN-shaped, one assumption must be proven: will the
target TURN server relay UDP to an **arbitrary external peer** (your VPS)?
Everything else is pointless until this is confirmed.

### Step 1. Run the echo on the VPS

On a foreign VPS (the port must be open in the firewall / security group):

```sh
go build -o vpsecho ./cmd/vpsecho
./vpsecho -listen 0.0.0.0:51822
```

Verify the port is actually reachable from outside, with a plain client:
```sh
# from your laptop
echo hi | nc -u -w1 <VPS_IP> 51822   # should echo "hi" back
```
If that fails, fix the firewall before going further.

### Step 2. Obtain short-lived TURN credentials

TURN almost always requires a username/password with a TTL, handed out by the
signaling layer when a call starts. For a one-off test, capture them from a real
session:

1. Open `chrome://webrtc-internals` in Chrome.
2. Start a call in the service whose TURN you want to use.
3. In webrtc-internals find the `RTCPeerConnection` → `iceServers` field:
   it lists `urls: turn:HOST:PORT`, `username`, `credential`.
4. Note down `HOST:PORT`, `username`, `credential`.

> These credentials live for minutes/hours. A persistent tunnel (milestone 2+)
> will need to auto-refresh them by replaying the service's API handshake — the
> most fragile part of the project. For this step, one-off creds are enough.

### Step 3. Run the probe from your laptop

```sh
go build -o probe ./cmd/probe
./probe \
  -turn HOST:PORT \
  -user '<username>' \
  -pass '<credential>' \
  -vps <VPS_IP>:51822
```

Outcomes:
- **SUCCESS ✅** — the relay delivered the packet to the external VPS and the
  echo came back. The assumption holds; build the tunnel (milestone 1).
- **Allocate failed** — credentials wrong/expired, or the server refused the
  allocation.
- **TIMEOUT ❌** — the relay won't talk to external peers (or the VPS is
  unreachable). For this server, the approach is dead.

## Later milestones (after confirmation)

1. **Transport shim**: a local UDP proxy wrapping datagrams in TURN ChannelData
   → a VPS-side agent unwraps them and forwards to a local port.
2. **WireGuard on top**: wg between laptop and VPS, its UDP riding inside the
   shim. All crypto and the tun interface live in wg; TURN is just transport.
3. **Credential auto-refresh**: replay the service's handshake and keep the
   allocation alive (refresh before the lifetime expires).
4. **Resilience**: re-allocate on drop, keepalives, handle relay-address changes.
