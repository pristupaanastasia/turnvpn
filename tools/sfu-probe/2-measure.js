// turnvpn / sfu-probe — Gate 1 instrument, part 2 of 2.
//
// Run this in the SAME console AFTER video is actually flowing (a few seconds in,
// so bitrate/resolution have settled). Run it on BOTH tabs.
//
// It reads getStats() from every captured peer connection and prints, per video
// stream: codec, resolution, fps, target bitrate, and the simulcast rid/ssrc.
//
// HOW TO READ THE RESULT (see README.md for the full checklist):
//   * On the SENDER tab you see the OUTBOUND layers — usually several (simulcast).
//   * On the RECEIVER tab you see the INBOUND stream the SFU chose to deliver.
//   * FORWARDING SFU  -> receiver's codec+resolution EXACTLY equal one sender layer.
//   * TRANSCODING MCU -> receiver's resolution is server-normalised / not among the
//                        sender layers, or the codec differs. => Vector 2 is dead.
(async () => {
  const pcs = window.__turnvpnPCs || [];
  if (!pcs.length) {
    console.warn('[turnvpn] no captured PCs — paste 1-hook.js BEFORE the call, then reload and retry.');
    return;
  }

  const num = (v) => (typeof v === 'number' ? v : undefined);
  const kbps = (v) => (num(v) !== undefined ? Math.round(v / 1000) : undefined);

  for (const [i, pc] of pcs.entries()) {
    const stats = await pc.getStats();
    const codecs = new Map();
    const out = [], inb = [];
    stats.forEach((r) => { if (r.type === 'codec') codecs.set(r.id, r); });
    stats.forEach((r) => {
      if (r.kind !== 'video') return;
      if (r.type === 'outbound-rtp') out.push(r);
      if (r.type === 'inbound-rtp') inb.push(r);
    });

    const codecOf = (r) => {
      const c = codecs.get(r.codecId);
      return c ? c.mimeType + (c.sdpFmtpLine ? ' [' + c.sdpFmtpLine + ']' : '') : '?';
    };
    const fmt = (r) => ({
      dir: r.type === 'outbound-rtp' ? 'OUT' : 'IN',
      rid: r.rid,
      ssrc: r.ssrc,
      codec: codecOf(r),
      res: (r.frameWidth || '?') + 'x' + (r.frameHeight || '?'),
      fps: num(r.framesPerSecond),
      targetKbps: kbps(r.targetBitrate),
      frames: num(r.framesEncoded) ?? num(r.framesDecoded),
    });

    if (!out.length && !inb.length) {
      console.log(`[turnvpn] PC#${i + 1}: no video streams yet — wait for video, re-run.`);
      continue;
    }
    console.log(`[turnvpn] PC#${i + 1} — paste these two lines back:`);
    if (out.length) console.log('[turnvpn] SENDER layers:', JSON.stringify(out.map(fmt)));
    if (inb.length) console.log('[turnvpn] RECEIVER stream:', JSON.stringify(inb.map(fmt)));
  }
})();
