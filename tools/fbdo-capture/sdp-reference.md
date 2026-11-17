# VK Звонки — SDP контракт звонка (captured 2026-11-17)

Снято с живого звонка через Claude-in-Chrome: хук **конструктора**
`RTCPeerConnection` (в top-окне, гостевой контекст), вооружён на пред-джойн
лобби того же документа, затем join. **PeerConnection живёт в странице** (не в
воркере) — весь SDP-обмен виден.

Эфемерные значения (ice-ufrag/pwd, fingerprint, candidate, ssrc, msid, IP) и
base64 в `a=fmtp` (напр. H264 `sprop-parameter-sets`) **заредакчены** — это
сессионные/секретные поля, pion генерит свои. Здесь — структура, которую pion
должен воспроизвести.

## Направление негоциации (важно)

**SFU — offerer, клиент — answerer.** pion заходит как **отвечающий**:
получает offer от SFU → `setRemoteDescription(offer)` → `createAnswer` →
`setLocalDescription(answer)`.

- Offer: `setup:actpass` (SFU отдаёт выбор DTLS-роли).
- Answer: `setup:active` → **pion = DTLS client (active)**.

## Сессия

- `a=group:BUNDLE 0 1 2` — все три m-секции в одном bundle (один ICE/DTLS
  транспорт, rtcp-mux).
- `a=extmap-allow-mixed`.
- 3 m-секции: audio (mid 0), video (mid 1), application/datachannel (mid 2).

## m=audio (mid:0) — UDP/TLS/RTP/SAVPF, sendrecv, rtcp-mux, rtcp-rsize

| PT | codec | fmtp | rtcp-fb |
|----|-------|------|---------|
| 63 | red/48000/2 | `111/111` | — |
| 111 | opus/48000/2 | (opus params, redacted) | transport-cc |

extmap: `ssrc-audio-level`, `abs-send-time`, `transport-wide-cc`, `sdes:mid`.

## m=video (mid:1) — UDP/TLS/RTP/SAVPF, sendrecv, rtcp-mux, rtcp-rsize

Полный кодек-ладдер (каждый основной PT + его rtx):

| PT | codec | rtx PT |
|----|-------|--------|
| 96 | **VP8/90000** | 97 |
| 102 | H264/90000 | 103 |
| 104 | H264/90000 | 107 |
| 108 | H264/90000 | 109 |
| 114 | H264/90000 | 115 |
| 116 | H264/90000 | 117 |
| 39 | H264/90000 | 40 |
| 45 | AV1/90000 | 46 |
| 118 | H264/90000 | 119 |
| 49 | H265/90000 | 50 |
| 122 | red/90000 | 123 |
| 124 | ulpfec/90000 | — |

(H264 — 6 профилей через разные `profile-level-id`/`packetization-mode`,
значения заредакчены.) rtcp-fb на видео-кодеках: nack, nack pli, ccm fir,
transport-cc, goog-remb (стандартный набор; точные — в сыром SDP).

extmap: `toffset`, `abs-send-time`, `urn:3gpp:video-orientation`,
`transport-wide-cc`, `playout-delay`, `video-content-type`, `video-timing`,
`color-space`, `sdes:mid`, **`sdes:rtp-stream-id`**,
**`sdes:repaired-rtp-stream-id`** → последние два = RID-симулкаст.

## m=application (mid:2) — UDP/DTLS/SCTP, webrtc-datachannel

`setup:actpass` (offer) / `setup:active` (answer).

## Что это даёт pion (Vector 2)

- pion заходит **answerer**, DTLS **active**, один bundled транспорт, rtcp-mux.
- Для смузгл-канала достаточно оставить в answer **VP8 (96) + rtx (97)** на видео
  и opus (111) на audio; остальные видео-кодеки можно не предлагать (answer —
  подмножество offer). datachannel (mid 2) можно принять или отклонить
  (`m=application 0`).
- Крафт-кадры — валидный **VP8** (PT 96), payload с нашими байтами; слушать
  PLI/REMB/transport-cc из rtcp-fb.
- **Открытый вопрос Stage 1:** транспорт сигналинга. offer пришёл в
  `setRemoteDescription`, но в странице НЕ видно ни fetch/XHR/WS, несущих SDP →
  доставка идёт, вероятно, через **воркер/бридж** (`webCallsBridge`) движка VK
  Calls SDK. Чтобы pion завести без браузера, надо выяснить, куда реально
  отправляется answer и откуда берётся offer (VK API `getCallPreview`/
  `getConversationByCall` → OK endpoint, затем обмен — предположительно в
  воркере). Это следующая ступень (1c-слой-B).
