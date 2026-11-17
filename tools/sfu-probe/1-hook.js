// turnvpn / sfu-probe — Gate 1 instrument, part 1 of 2.
//
// Paste this into the DevTools console of the VK call tab BEFORE you join the
// call (right after the page has loaded, before clicking "call"). Do it on BOTH
// tabs (sender and receiver), each in its own profile/account.
//
// It wraps RTCPeerConnection so every peer connection the page creates is stashed
// on window.__turnvpnPCs. Most WebRTC libraries resolve `new RTCPeerConnection`
// against the global at call time, so hooking after page load but before the
// call is enough. If 2-measure.js later reports "no captured PCs", the page
// grabbed its own reference earlier — reload the tab and paste this first thing.
(() => {
  const Native = window.RTCPeerConnection;
  if (!Native) { console.warn('[turnvpn] no RTCPeerConnection on this page'); return; }
  if (Native.__turnvpnWrapped) { console.log('[turnvpn] already hooked'); return; }

  const pcs = (window.__turnvpnPCs = []);
  function Wrapped(...args) {
    const pc = new Native(...args);
    pcs.push(pc);
    console.log('[turnvpn] captured RTCPeerConnection #' + pcs.length);
    return pc;
  }
  Wrapped.prototype = Native.prototype;
  Object.defineProperty(Wrapped, '__turnvpnWrapped', { value: true });
  // Carry over static members (generateCertificate, etc.).
  for (const k of Object.getOwnPropertyNames(Native)) {
    if (!(k in Wrapped)) { try { Wrapped[k] = Native[k]; } catch (_) {} }
  }
  window.RTCPeerConnection = Wrapped;
  window.webkitRTCPeerConnection = Wrapped;
  console.log('[turnvpn] RTCPeerConnection hooked — now start/join the VK call.');
})();
