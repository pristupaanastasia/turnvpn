// turnvpn / fbdo-capture — Stage 1 (Vector 2) signaling capture.
//
// Goal: snapshot the FULL signaling handshake of one real VK/OK call join so we
// can reverse the calls.okcdn.ru/fb.do protocol (OK "TopGun"-style base64) and,
// later, drive it from a native pion client. We do NOT decode here — we capture
// everything verbatim (request + response bodies, WebSocket frames) with order
// and timing preserved, and analyse offline.
//
// WHY it hooks three transports: fb.do POSTs almost certainly carry auth + room
// join, but the live SDP offer/answer and trickle-ICE of an ongoing call usually
// ride a WebSocket. Miss the WS and you miss the SDP channel. So we hook
// fetch + XMLHttpRequest + WebSocket, in the top window AND every same-origin
// frame, and re-install on a 150ms poller to catch frames/libs that load late
// (the proven approach from earlier VK instrumentation — VK caches method refs,
// so we patch broadly and keep re-patching).
//
// ---------------------------------------------------------------------------
// HOW TO RUN
//   1. Open the VK call page (vk.ru/call/... or the call-by-link join page).
//   2. Open DevTools console on that tab. Paste this whole file. It prints
//      "[cap] armed". (Re-paste is safe — it won't double-install.)
//   3. Do ONE clean call join with a second real participant (per the memory
//      note: a solo join with mic denied never negotiates — you need a genuine
//      2nd endpoint, e.g. an anonymous guest from a logged-out tab). Let it run
//      ~10-15s so SDP, ICE and any keepalives are all captured.
//   4. In the console run:  __capStats()      // see how much was captured
//                           __capSave()        // download cap.json
//      (__capSave downloads a JSON file; if the download is blocked, run
//       __capCopy() to copy the JSON to the clipboard instead.)
//   5. Hand cap.json back. We decode fb.do / WS frames offline with the Go
//      decoder in this same tool dir.
//
// Captures only YOUR OWN session's signaling. Sends nothing anywhere; it only
// observes requests the page already makes.
// ---------------------------------------------------------------------------
(() => {
  const G = window;
  if (G.__cap && G.__cap.armed) { console.log('[cap] already armed'); return; }

  const log = (G.__cap = {
    armed: true,
    t0: Date.now(),
    events: [],   // ordered capture records
    seq: 0,
  });

  const now = () => Date.now() - log.t0;
  const MATCH = /fb\.do|okcdn|calls\.|\/call|webrtc|signal|tgserver|vk\.(ru|com)\/.*call/i;

  // Best-effort body stringifier. Bodies may be strings, FormData, Blob,
  // ArrayBuffer, URLSearchParams — keep whatever we can get synchronously as
  // text, and flag binary we can't read inline.
  function bodyToText(body) {
    try {
      if (body == null) return null;
      if (typeof body === 'string') return body;
      if (body instanceof URLSearchParams) return body.toString();
      if (body instanceof ArrayBuffer) return '[ArrayBuffer ' + body.byteLength + 'B] ' + b64(new Uint8Array(body));
      if (ArrayBuffer.isView(body)) return '[TypedArray ' + body.byteLength + 'B] ' + b64(new Uint8Array(body.buffer, body.byteOffset, body.byteLength));
      if (body instanceof FormData) {
        const parts = [];
        for (const [k, v] of body.entries()) parts.push(k + '=' + (typeof v === 'string' ? v : '[file]'));
        return 'FORMDATA ' + parts.join('&');
      }
      return String(body);
    } catch (e) { return '[unreadable body: ' + e + ']'; }
  }

  function b64(u8) {
    let s = '';
    for (let i = 0; i < u8.length; i++) s += String.fromCharCode(u8[i]);
    try { return btoa(s); } catch (_) { return '[b64 fail]'; }
  }

  function record(rec) {
    rec.seq = ++log.seq;
    rec.t = now();
    log.events.push(rec);
    const tag = rec.kind.toUpperCase();
    const where = (rec.url || rec.wsUrl || '').slice(0, 90);
    console.log('[cap#' + rec.seq + ' @' + rec.t + 'ms] ' + tag + ' ' + (rec.dir || '') + ' ' + where);
  }

  function interesting(url) { try { return MATCH.test(url); } catch (_) { return false; } }

  // ---- fetch ----
  function hookFetch(win) {
    if (!win.fetch || win.fetch.__capped) return;
    const orig = win.fetch;
    const wrapped = function (input, init) {
      const url = (typeof input === 'string' ? input : (input && input.url)) || '';
      const method = (init && init.method) || (input && input.method) || 'GET';
      const reqBody = init && init.body;
      const hit = interesting(url);
      const p = orig.apply(this, arguments);
      if (!hit) return p;
      const base = { kind: 'fetch', dir: 'req', url, method, reqBody: bodyToText(reqBody) };
      record(base);
      return p.then((resp) => {
        try {
          resp.clone().text().then((txt) => {
            record({ kind: 'fetch', dir: 'resp', url, method, status: resp.status, respBody: txt, forSeq: base.seq });
          }).catch(() => {});
        } catch (_) {}
        return resp;
      });
    };
    wrapped.__capped = true;
    try { win.fetch = wrapped; } catch (_) {}
  }

  // ---- XMLHttpRequest ----
  function hookXHR(win) {
    const X = win.XMLHttpRequest;
    if (!X || X.__capped) return;
    const open = X.prototype.open;
    const send = X.prototype.send;
    X.prototype.open = function (method, url) {
      this.__cap = { method, url, hit: interesting(url) };
      return open.apply(this, arguments);
    };
    X.prototype.send = function (body) {
      const c = this.__cap;
      if (c && c.hit) {
        const base = { kind: 'xhr', dir: 'req', url: c.url, method: c.method, reqBody: bodyToText(body) };
        record(base);
        this.addEventListener('load', () => {
          let txt = '';
          try { txt = this.responseType === '' || this.responseType === 'text' ? this.responseText : '[' + this.responseType + ']'; } catch (_) { txt = '[resp read fail]'; }
          record({ kind: 'xhr', dir: 'resp', url: c.url, method: c.method, status: this.status, respBody: txt, forSeq: base.seq });
        });
      }
      return send.apply(this, arguments);
    };
    try { Object.defineProperty(X, '__capped', { value: true }); } catch (_) {}
  }

  // ---- WebSocket ----
  function hookWS(win) {
    const W = win.WebSocket;
    if (!W || W.__capped) return;
    function Wrapped(url, protocols) {
      const ws = protocols === undefined ? new W(url) : new W(url, protocols);
      const hit = interesting(url) || /ws/i.test(String(url));
      if (hit) {
        const open = { kind: 'ws', dir: 'open', wsUrl: String(url) };
        record(open);
        const sendOrig = ws.send;
        ws.send = function (data) {
          record({ kind: 'ws', dir: 'send', wsUrl: String(url), respBody: bodyToText(data) });
          return sendOrig.apply(this, arguments);
        };
        ws.addEventListener('message', (ev) => {
          record({ kind: 'ws', dir: 'recv', wsUrl: String(url), respBody: bodyToText(ev.data) });
        });
        ws.addEventListener('close', () => record({ kind: 'ws', dir: 'close', wsUrl: String(url) }));
      }
      return ws;
    }
    Wrapped.prototype = W.prototype;
    for (const k of ['CONNECTING', 'OPEN', 'CLOSING', 'CLOSED']) Wrapped[k] = W[k];
    try { Object.defineProperty(Wrapped, '__capped', { value: true }); } catch (_) {}
    try { win.WebSocket = Wrapped; } catch (_) {}
  }

  function installAll() {
    const wins = [G];
    try {
      for (let i = 0; i < G.frames.length; i++) {
        try { const f = G.frames[i]; void f.location.href; wins.push(f); } catch (_) { /* cross-origin, skip */ }
      }
    } catch (_) {}
    for (const w of wins) { try { hookFetch(w); hookXHR(w); hookWS(w); } catch (_) {} }
  }

  installAll();
  log.poller = setInterval(installAll, 150);

  G.__capStats = function () {
    const byKind = {};
    for (const e of log.events) { const k = e.kind + '.' + e.dir; byKind[k] = (byKind[k] || 0) + 1; }
    console.log('[cap] events=' + log.events.length, byKind, 'span=' + now() + 'ms');
    return byKind;
  };
  G.__capJSON = function () { return JSON.stringify({ t0: log.t0, span: now(), events: log.events }, null, 0); };
  G.__capSave = function () {
    const blob = new Blob([G.__capJSON()], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'cap.json';
    document.body.appendChild(a); a.click(); a.remove();
    console.log('[cap] saved cap.json (' + log.events.length + ' events)');
  };
  G.__capCopy = async function () {
    try { await navigator.clipboard.writeText(G.__capJSON()); console.log('[cap] copied to clipboard'); }
    catch (e) { console.warn('[cap] clipboard blocked, use __capSave()', e); }
  };
  G.__capStop = function () { clearInterval(log.poller); console.log('[cap] poller stopped (hooks stay)'); };

  console.log('[cap] armed — hooks on fetch/XHR/WebSocket (top + same-origin frames). '
    + 'Do the call join, then run __capStats() and __capSave().');
})();
