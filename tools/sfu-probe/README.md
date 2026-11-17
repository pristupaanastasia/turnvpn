# sfu-probe — Gate 1: does VK's SFU transcode or forward?

This answers the single make-or-break question for **Vector 2** (tunnelling data
inside a VK video call's RTP), *before* investing any work in it:

> Does VK's media server **re-encode** your video (MCU / transcode), or does it
> **forward** your encoded frames untouched (selective-forwarding SFU)?

If it transcodes, Vector 2 is dead — any data you hide in the bitstream is
destroyed on the server. If it forwards, Vector 2 is *technically* possible and
Gate 2 (byte-level survival with a native pion stack + reverse-engineered
signalling) becomes worth doing.

An SFU **always** terminates SRTP and rewrites RTP headers, so "passthrough" of
raw RTP never exists. The only question that matters is whether the **encoded
video payload** survives. That is observable from `getStats()` alone — no raw
frames, no native stack, no reverse engineering.

## Why this is the cheap gate

A VK sender almost certainly uses **simulcast**: it encodes the same camera at
~3 resolutions at once. A forwarding SFU *selects one of those exact layers* and
relays it. A transcoding server ingests the stream and re-encodes to its own
ladder. So:

| Signal (receiver vs sender)                     | Forwarding SFU | Transcoding MCU |
|-------------------------------------------------|----------------|-----------------|
| Receiver codec == one sender layer's codec      | yes            | may differ      |
| Receiver resolution == one sender layer exactly | yes            | normalised/own  |
| Receiver `sdpFmtpLine` profile matches          | yes            | may differ      |

Exact match on codec **and** resolution to one of the sender's simulcast layers
is strong evidence of forwarding. A receiver resolution that is *not* among the
sender's layers (or a codec swap) is transcoding → stop.

## Procedure

You need two VK accounts (or one call joinable twice) in **two separate browser
profiles** so you can read stats on both ends of the same call.

1. Open the VK call page in both profiles. **Before joining**, open DevTools
   console in each and paste **`1-hook.js`**. (Paste before the call starts; if
   step 4 says "no captured PCs", reload and paste it first thing.)
2. Start / join the call between the two profiles. Enable camera on at least the
   sender side.
3. Wait ~5 seconds for bitrate and resolution to settle.
4. Paste **`2-measure.js`** in both consoles.
5. From the **sender** tab, copy the `SENDER layers:` line. From the
   **receiver** tab, copy the `RECEIVER stream:` line.
6. Compare: does the receiver stream's `codec` + `res` exactly equal one of the
   sender layers? → **forwarding**. Otherwise → **transcoding**.

Paste both lines back and I'll call the verdict and, if it's green, spec Gate 2.

## Caveats

- Hooking after page load works only because WebRTC libs read the global
  `RTCPeerConnection` at call time. If VK bundled its own early reference, reload
  and paste `1-hook.js` as the very first thing.
- `targetBitrate` on inbound is the receiver's estimate, not proof of anything;
  codec + resolution are the decisive fields.
- Simulcast layers share the camera, so their *content* is identical — compare
  the structural fields (codec, resolution, fps), not pixels.
- This measures your own call passively. It sends nothing unusual to VK and does
  not touch anyone else's traffic.
