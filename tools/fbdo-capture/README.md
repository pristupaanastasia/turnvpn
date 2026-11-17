# fbdo-capture — Stage 1 (Vector 2): reverse the `fb.do` signaling

Vector 2 rides the media leg of a real VK/OK call. To put a native **pion**
client into that call we must speak the call's signaling: the custom base64
("TopGun"-style) protocol over `calls.okcdn.ru/fb.do`, plus any WebSocket channel
carrying the live SDP offer/answer and trickle ICE. Everything else in Vector 2
gates on this rung.

This tool does the **capture → understand** half (no replay yet):

- **`capture.js`** — paste into the call tab's DevTools console. Hooks
  `fetch` + `XMLHttpRequest` + `WebSocket` in the top window and every
  same-origin frame, re-installing on a 150ms poller (VK caches method refs, so
  we patch broadly and keep re-patching). Records every request/response/WS frame
  verbatim, in order, with timing. Decodes nothing — we analyse offline.
- **`analyze/`** — a Go tool that reads the captured `cap.json`, prints an
  ordered timeline, pretty-prints JSON bodies, finds base64 blobs, decodes them,
  hexdumps, and flags signaling markers (SDP `m=`/`a=`, `ice-ufrag`,
  `fingerprint`, `candidate`, `turn:`/`stun:`, codecs).

## Capture procedure

The hard constraint (from prior instrumentation): a **solo** join with mic denied
never negotiates — you need a genuine **second participant**. One browser = one VK
identity, and two tabs under the same account kick each other, so use an
**anonymous guest**: log out in a normal tab (not incognito — the extension can't
see incognito), open the `vk.ru/call/join/<token>` link, enter a name. That guest
coexists with your logged-in session and forces a real SDP negotiation.

1. Open the VK call page. Open DevTools console. Paste **`capture.js`**
   → it prints `[cap] armed`.
2. Join / start the call with the second (guest) participant. Enable camera.
3. Let it run ~10–15s (SDP, ICE, keepalives all captured).
4. In the console:
   ```js
   __capStats()   // how much was captured, by kind
   __capSave()    // downloads cap.json  (or __capCopy() to clipboard)
   ```
5. Put `cap.json` somewhere and run the analyzer:
   ```sh
   go run ./tools/fbdo-capture/analyze -in cap.json            # full timeline
   go run ./tools/fbdo-capture/analyze -in cap.json -grep sdp  # only SDP-bearing events
   go run ./tools/fbdo-capture/analyze -in cap.json -seq 7     # one event, full detail
   ```

## What we're looking for in the output

To let pion join, we need to locate and map, across the timeline:

- **Auth / session** — the token(s) the call page already holds (we reuse a
  captured session; we do **not** automate account creation).
- **Room / conversation id** — what identifies the call to join.
- **The SDP channel** — which transport (fb.do response vs WS frame) carries the
  offer and answer, and how SDP is encoded inside the base64 envelope.
- **ICE** — trickle candidates: same channel or separate, and their framing.
- **Keepalive / sequence** — any counters or heartbeats the server expects.

Once those are mapped we can spec the pion join client (next sub-stage): replay
the handshake with our own SDP, negotiate audio+video, and reach "connected".

## Scope / safety

Captures only your own session's signaling; sends nothing anywhere. Credentials
in `cap.json` are short-lived call tokens — treat the file as a secret, keep it
out of git (it is not committed), delete it when done.
