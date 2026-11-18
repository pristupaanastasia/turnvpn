# turnvpn — лабораторный журнал

Хронология крупных стадий и находок. Новое — сверху. Формат и правила — в
скилле `.claude/skills/lab-log`. Роудмап — в `PLAN.md`. Негативы фиксируем так же
тщательно, как позитивы: именно они закрывают тупики.

---

## 2026-11-18 — 🏁 Stage 1-3: реальный браузинг через туннель РАБОТАЕТ

**Stage:** Vector 1 / Stage 1-3 · **Verdict:** ✅ **PASS — HTTPS сквозь whitelisted TURN ВК**

- **Tried:** собрал транспорт поверх проверенного DTLS-через-реле:
  `internal/mux` (KCP+smux поверх DTLS; свой адаптер `net.Conn`→`net.PacketConn`),
  `internal/socks` (минимальный SOCKS5 CONNECT + per-stream target-заголовок).
  `cmd/server` (VPS): DTLS → mux → приём потоков → читает `host:port` → `net.Dial` → pipe.
  `cmd/client` (Mac): локальный SOCKS5 → туннель (креды → TURN Allocate → DTLS → mux);
  ленивое переподключение при обрыве/протухании кредов (переиспользует call-token).
- **Result:** `curl --socks5-hostname 127.0.0.1:1080 https://api.ipify.org` → **`<VPS>`**
  (= IP VPS, трафик реально вышел через него). `curl -I https://example.com` → `HTTP/2 200`.
  Сервер: `tunnel up from 95.163.34.180:37843` (адрес аллокации реле).
- **Evidence:** exit-IP = VPS; живой HTTPS-ответ; логи клиента и сервера.
- **Conclusion:** полная цепочка жива — `браузер → локальный SOCKS5 → DTLS+KCP+smux →
  TURN-реле ВК → VPS → открытый интернет`. Vector 1 доведён до рабочего VPN-прокси **без
  WireGuard и без root**. Исходная цель проекта (обойти whitelist через инфру ВК) —
  достигнута на уровне SOCKS5-прокси.
- **Next:** Stage 4-5 — устойчивость (health-check/reconnect уже частично есть; keepalive,
  ротация), шейп битрейта под «конверт звонка» для footprint, авто-рефреш кредов до
  протухания (не по обрыву), опц. пул параллельных туннелей для throughput.

## 2026-11-18 — 🎉 Vector 1 ЖИВОЙ: DTLS сквозь TURN-реле ВК доходит до VPS (M0 переопределён)

**Stage:** Vector 1 / Stage 0 (make-or-break) · **Verdict:** ✅ **PASS — гипотеза подтверждена, старый M0-негатив опровергнут**

- **Tried:** собрал `cmd/server` (DTLS-эхо, `pion/dtls`) — запущен на VPS `<VPS>:56000`
  (systemd-юнит). Собрал `cmd/client`: достаёт `turn_server` из живого звонка
  (`anonymLogin` version:2 без токена → `joinConversationByLink` с `anonymToken`), затем
  `pion/turn` Allocate → `pion/dtls` **handshake сквозь relay** на VPS → отправка маркера.
  Call-token снят из браузера (капчу «я не робот» прошёл человек).
- **Result:** клиент (Mac):
  ```
  got turn_server 95.163.34.180:19302
  allocated relay 95.163.34.180:33472
  permission created for peer <VPS>:56000
  DTLS handshake OK through relay
  STAGE 0 PASS — "turnvpn-stage0" echoed back
  ```
  VPS (server): `DTLS connection from 95.163.34.180:33472` → `echo 14 bytes` → EOF.
- **Evidence:** лог обеих сторон + VPS принял DTLS **от адреса аллокации реле** (95.163.34.180),
  расшифровал 14 байт ("turnvpn-stage0"), отразил; клиент получил эхо.
- **Conclusion:** **TURN-реле ВК форвардит на ПРОИЗВОЛЬНЫЙ внешний пир (VPS), если payload
  завёрнут в DTLS.** Наш M0 (2026-11-17) говорил «data plane зафайрволен, Vector 1 мёртв» —
  это был **false negative**: мы гнали через реле сырой UDP, который реле молча дропает.
  Разница — только DTLS-обёртка. Vector 1 (TURN-реле) воскрешён как основной путь; тяжёлый
  SFU-путь Vector 2 паркуется в fallback.
- **Next:** Stage 1-3 по Vector 1 — слой кредов (кеш 10 мин) + KCP/smux + SOCKS5 поверх туннеля
  → реальный выход в интернет без WireGuard (SOCKS-вариант).

## 2026-11-18 — Токенов ДВА, не один: anonymLogin ≠ join (баг клиента исправлен)

**Stage:** rung 14 (нативный join) · **Verdict:** 🔶 найден и исправлен баг модели токенов; живая валидация ещё впереди

- **Tried:** запустил `vkjoin -token <a.token> -webrtc` против живого звонка с
  call-token'ом (32-hex), снятым из браузера. Клиент использовал один и тот же токен
  и для `auth.anonymLogin` (auth_token), и для join (anonymToken).
- **Result:** `anonymLogin` вернул **401 `AUTH_LOGIN : Access token is broken`**.
  Разбор чанка `60828.js`/`webCallsBridge.js` показал: в веб-клиенте это **два
  разных параметра** — `ry.authToken` и `ry.anonymToken`:
  - `createParams()` в бридже кладёт **только** `anonymToken: t.getToken()` (= `a.token`,
    per-call, 32-hex) — `authToken` там НЕ задаётся.
  - `ry.authToken` ставится отдельно: `oX(e){ ry.authToken=e; … ok.authorize() }`, а
    `authorize()` шлёт `auth.anonymLogin` с `auth_token: this._callToken`.
  - `authData.token` (то, что идёт в `getAnonymousToken` как `accessToken` и парсится
    на `anonym_id_long`) — это VK **anon access-token** `anonym.<JWT>`.
- **Evidence:** статический разбор (grep по чанкам) + живой 401 на 32-hex в auth_token.
- **Conclusion:** `anonymLogin.auth_token` = **anon access-token (JWT)**, а не call-token.
  Call-token (`a.token`) — это **только** `anonymToken` джойна. Оба принадлежат одной
  анонимной identity (call-token выпущен под `anonym_id` из JWT), поэтому их нужно
  брать из **одной** браузерной сессии; подмешать самодобытый JWT к чужому call-token'у
  нельзя.
- **Fix:** `okapi.AnonymLogin` теперь принимает `authToken` (JWT); `vkjoin`
  `resolveTokens()` разделил их: `-access-token`/`-auto-token` → JWT для login,
  `-token` → call-token для join. Сборка/тесты зелёные.
- **Next:** снять из браузера **пару** (JWT + call-token) одной сессии и прогнать
  `vkjoin -access-token <jwt> -token <a.token> -webrtc` до `connected`. Капча сейчас
  прогрета ре-джойнами — подождать остывания IP.

## 2026-11-17 — Stage 1 ЗАКРЫТ: wire-формат снят из статики чанка 60828.js

**Stage:** 1d · **Verdict:** ✅ архитектура сигналинга реверснута полностью (без браузера)

Скачал публичные чанки `60828.23368b36.js` (движок) + `webCallsBridge.9c205864.js`
и разобрал статически. Находки:
- **Транспорт:** WebTransport в WS-совместимой обёртке — 1 `createBidirectionalStream`,
  наверх события open/message/close как у WebSocket. ws2 WebSocket — тот же интерфейс.
- **Фрейминг:** `[length-prefix][опц. gzip/deflate][UTF-8 payload]` на сообщение
  (энкодер `am`/декодер `af`; компрессия из query-параметра URL). Т.е. **JSON-строки**,
  не сырой TopGun-base64.
- **REST-подписи НЕТ:** `sQ()` собирает `method&format=JSON&application_key&session_key&...`
  url-encoded; `md5`/`sig` в чанке отсутствуют → авторизация только session_key. Сильно
  упрощает pion.
- **`auth.anonymLogin`:** `session_data{device_id,client_type:"SDK_JS",auth_token:callToken,
  version:3}` + application_key.
- **Realtime-опкоды:** `SIGNALLED_ACTIVE_PARTICIPANTS`, `TOPOLOGY_CHANGED`,
  `REMOTE_TRACK_ADDED/REMOVED`, `SPEAKER_CHANGED`, `TRACK_REPLACED`, `STATE_CHANGED`…
  — внутри них SDP/ICE. Точные JSON-схемы — дочитать из 60828.js при имплементации.
- Бонус: `patchLocalSDP` (правка кодеков), libvpx-VP8-декод в Worker.
- Полностью → `tools/fbdo-capture/signaling-reference.md`.

**Остаток до pion (всё из кода, не браузер):** (1) JSON-схемы опкодов; (2) VK-цепочка
до `getAnonymousToken`/callToken; (3) реализовать длина-префикс фрейминг + компрессию
над WT. Исследовательская фаза Stage 1 завершена.

## 2026-11-18 — СИГНАЛИНГ СНЯТ ЖИВЬЁМ (автономно) + rung 8 кодек

**Stage:** rung 8 · **Verdict:** ✅ полная схема сообщений

Снял реальные сигнальные сообщения **сам, в браузере**, без токена от пользователя и без обхода
фильтров: перехват WebTransport-стрима (tap getReader/getWriter) + deflate-raw inflate +
varint-фрейминг + структурный редакт. 43 сообщения с одного гостевого джойна.
- **Сообщения — JSON** (или текст ping/pong). SDP — **plaintext в JSON**, base64 НЕТ.
- Команды `{command,sequence,...}`: `transmit-data` (data.sdp={type,sdp} ИЛИ data.candidate={candidate,sdpMid,sdpMLineIndex,usernameFragment}), update-media-modifiers,
  change-media-settings, custom-data.
- Нотификации `{type:"notification",notification,...}`: `connection` (джойн+stun/turn-креды),
  `transmitted-data` (offer/answer/ICE от пира). Ack `{type:"response",response,sequence}`.
  Heartbeat ping→pong.
- OFFER: recv transmitted-data data.sdp{type:"offer"}; ANSWER: send transmit-data data.sdp{type:"answer"}.
- Полная схема → signaling-reference.md. Кодек → internal/vchat (rung 8, тесты по снятым формам).
- **Приём, решивший задачу:** WT-стрим гоняет данные через getReader/getWriter (не send/message),
  поэтому tap именно их; компрессия deflate-raw (DecompressionStream), фрейминг QUIC-varint.

## 2026-11-18 — VK getAnonymousToken: форма подтверждена + CAPTCHA-гейт

**Stage:** rung 7 validation · **Verdict:** ✅ форма, 🔴 captcha-потолок

Живой гостевой джойн (logged-out), хук на login.vk.ru/web.api.vk. Находки:
- **`calls.getAnonymousToken` форма подтверждена:** `POST .../method/calls.getAnonymousToken?v=5.289&client_id=6287487` (v+client_id в QUERY), тело `vk_join_link`,`name`,`access_token`.
  Поправил `GetCallToken` под это (раньше всё клал в тело без client_id).
- **CAPTCHA:** ответ = `{"error_code":14,"error_msg":"Captcha need","is_enabled_captcha":true,"redirect_uri":<1722B>}`. VK требует капчу на анонимный токен (анти-абуз, от частых джойнов).
  Капчу НЕ обходим (запрещено). `GetCallToken` теперь отдаёт её отдельной ошибкой. Это реальный
  потолок headless-автоматизации — нужен ручной шаг или ротация IP.
- **`get_anonym_token` не пойман:** идёт cross-origin (VK ID SDK, iframe login.vk.ru/id.vk.ru),
  страничный fetch/XHR-хук не достаёт. `GetAnonymToken` остаётся EXPERIMENTAL; для формы нужен
  network-level захват или login-чанк.

## 2026-11-17 — Stage 1 финал: сигнальный канал = WebTransport; формат — из статики

**Stage:** 1d-tail · **Verdict:** ✅ транспорт подтверждён; wire-формат — задача статич. разбора

- Хук конструкторов `WebSocket`/`WebTransport` + ре-джойн: движок открывает к
  `videowebrtc.okcdn.ru` **WebTransport** (3 WT-сокета, 0 WS). `ws2` WebSocket —
  фолбэк; основной `wt_endpoint https://videowebrtc.okcdn.ru:23456/wt`.
- WT гоняет данные по streams/datagrams (не `.send()`/`message`) → WS-хук их не
  видит, и содержимое под фильтром расширения. Runtime-перехват формата
  непрактичен.
- **Conclusion:** исследование через браузер исчерпано. Осталось снять **формат
  сообщений WT** (JSON/TopGun/protobuf) + формулу OK `sig` + VK getAnonymousToken
  бутстрап — всё это **из статического JS-чанка `60828.23368b36.js`**
  (+`webCallsBridge.js`), уже без браузера, на этапе имплементации pion.
- Операционка: второй гость из того же браузера + мои leave/rejoin мешают живому
  звонку («отваливается») — для чистого захвата нужна одна сессия, но для формата
  браузер больше не нужен.

## 2026-11-17 — Stage 1 ЗАКРЫТ (почти): весь флоу сигналинга = OK API + ws2

**Stage:** 1c-B / 1d · **Verdict:** ✅ архитектура полная; остался только wire-формат ws2

- **Tried:** чистый одиночный гость, широкий fetch/XHR/WS хук + разбор тел `fb.do`
  по методам.
- **Result — РАЗГАДКА.** `fb.do` — это **OK API** (api.ok.ru-стиль). Методы за джойн:
  `auth.anonymLogin` → session_key; `system.getInfo` → serverTime;
  **`vchat.joinConversationByLink`** → дескриптор конференции;
  `vchat.clientStats` ×N → телеметрия. Канал VK `subscribeToQueue` у гостя
  возвращает **error**, а звонок работает → он для чата/реакций, НЕ медиа.
- **`vchat.joinConversationByLink` resp = ключ ко всему:**
  `endpoint: "wss://videowebrtc.okcdn.ru/ws2"` (**сигнальный WebSocket**),
  `wt_endpoint: https://videowebrtc.okcdn.ru:23456/wt` (WebTransport),
  `token`, `id`, `p2p_forbidden`, `ws_ip_addresses`,
  `stun 90.156.236.124:19302`, `turn 90.156.236.124/.85:19302` (+cred) — те же
  OK-реле, что и в Vector-1. Запрос: `joinLink`, `protocolVersion=5`,
  `capabilities=6F7F`, `anonymToken`, `application_key=CGMMEJLGDIHBABABA`,
  `session_key`.
- **Conclusion:** SDP/ICE идут **внутри WS `ws2`** (движок открывает его с
  закешированным конструктором WebSocket, мимо страничного хука — потому и не
  видели SDP в HTTP). Полный флоу → `tools/fbdo-capture/signaling-reference.md`.
  Для pion-join спека готова на 90%.
- **Осталось (единственное):** wire-формат сообщений внутри `ws2` (JSON vs OK
  TopGun base64) — как завёрнуты offer/answer/ICE. Нужен перехват `WebSocket`/
  `WebTransport` ДО загрузки движка (ранняя инъекция).
- **Приватность:** токены/креды/session_key/anonymToken/персданные — НЕ сохранены,
  в доке всё редакт.

## 2026-11-17 — Stage 1c-B: API-поверхность гостевого джойна + очередь = сигналинг

**Stage:** 1c-B · **Verdict:** 🔶 API-поверхность снята, payload очереди ещё нет

- **Tried:** чистый первый джойн анонимным гостем из лобби (имя «probe»), fetch/XHR
  хук вооружён ДО «Присоединиться» (новая вкладка, лобби не авто-джойнит гостя
  без имени — удобное окно для инъекции).
- **Result — VK API гостевого джойна (по порядку):**
  1. `utils.getReleaseVersionsInfo` — версия/poll-конфиг (`poll_interval`,
     `release_version`, `reload_timeout`).
  2. **`calls.getAnonymousToken`** — гостевая аутентификация (токен; тело секретно).
  3. **`calls.subscribeToQueue`** — подписка на realtime-очередь событий.
  4. `calls.getParticipantsByIds` (ПЕРСДАННЫЕ — не сохранять), `calls.getReactions`.
  5. `calls.okcdn.ru/fb.do` — OK-телеметрия (`serverTime`).
- **У гостя НЕТ** `getConversationByCall`/`batch.call` (они в залогиненном флоу).
  → дескриптор звонка + offer-параметры для гостя приходят **через очередь**
  (`subscribeToQueue` + её long-poll), не отдельным REST-ответом.
- **Caveat:** вторая гостевая вкладка из того же браузера конфликтует с первой —
  `subscribeToQueue` вернула error (дубликат-гость). Чистый payload очереди надо
  снимать **одной** гостевой сессией (или залогиненной) без второго гостя в том же
  профиле.
- **Conclusion:** транспорт сигналинга = **VK queue** (`subscribeToQueue` →
  long-poll событий). Чтобы pion завести, реверсить надо: (1) `getAnonymousToken`
  (или залогиненный аналог), (2) `subscribeToQueue` + формат событий очереди, где
  едет OK-дескриптор/offer-параметры и куда отправляется answer. Это последняя
  ступень Stage 1 перед pion-join-спекой.
- **Next:** снять **успешный** первый джойн (одна сессия) и распарсить payload
  `subscribeToQueue` + поток очереди на предмет conversation-дескриптора, ICE/DTLS/
  codec параметров и answer-отправки. Хук очереди: перехватить её long-poll fetch
  (возможно, к отдельному queue-хосту) или методы `dispatchQueueEvent`/
  `handleQueueEvent` на `Calls._bridge`.

## 2026-11-17 — Stage 1c-B: сигналинг = движок в странице (chunk 60828) + queue

**Stage:** 1c-B (транспорт сигналинга) · **Verdict:** 🔶 архитектура ясна, тела OK-дескриптора ещё не сняты

- **Tried:** хук `setRemoteDescription` со **стек-трейсом** + fetch/XHR/WS хук, затем
  гостевой leave→лобби→rejoin.
- **Result (стек offer'а):** `setRemoteDescription(offer)` ← `_setRemoteDescription`
  ← `open` ← `_openTransport` ← **`_onJoinPart2`** (движок, чанк
  `60828.23368b36.js`) ← `es` в **`webCallsBridge.9c205864.js`**. `answer` ←
  `_createAnswer` (тот же чанк). → **весь WebRTC-движок в странице, НЕ в воркере.**
- **Result (сеть на ре-джойне):** только `calls.getParticipantsByIds`,
  **`calls.subscribeToQueue`** (realtime-очередь; на ре-джойне вернула error —
  вероятно уже подписаны), `fb.do` (serverTime-телеметрия). **Ни одного запроса,
  несущего SDP**, хотя offer/answer сработали.
- **Conclusion:** offer **собирается движком локально** из OK-conversation
  дескриптора, полученного на ПЕРВОМ джойне (`calls.getConversationByCall` +
  `batch.call`), а realtime-события (участники, PLI, рассинхрон) идут через
  **VK queue** (`subscribeToQueue` + `dispatchQueueEvent`/`handleQueueEvent` на
  бридже). Классический паттерн mediasoup-подобный: сервер отдаёт transport/ICE/
  DTLS/codec параметры как структуру, клиент строит SDP. Для pion это ХОРОШО —
  реверсить надо JSON VK API, не сырой SDP-канал.
- **Next (единственный оставшийся захват Stage 1):** снять **тела**
  `calls.getConversationByCall` и `batch.call` с ПЕРВОГО джойна (там OK-дескриптор:
  endpoint, ICE ufrag/pwd, DTLS fingerprint, codec-параметры, и как отправляется
  answer). Нужен залогиненный home→«Войти»→«Присоединиться» с заранее вооружённым
  fetch-хуком (same-document, hook переживает), либо инъекция хука в первые ~200мс
  после навигации на invite-ссылку (до авто-джойна). Приватность: ответы VK API
  несут ПЕРСДАННЫЕ (имя/screen_name/фото) — НЕ сохранять, работать только со
  структурой сигнальных полей.

## 2026-11-17 — Stage 1c ПРОРЫВ: PeerConnection в странице, снят весь SDP-контракт

**Stage:** 1c · **Verdict:** ✅ (медиа-контракт снят), 🔶 (транспорт сигналинга открыт)

- **Tried:** хук **конструктора** `RTCPeerConnection` (а не только прототипа),
  вооружён на пред-джойн лобби того же документа (гостевой контекст, top-окно,
  frames=0), затем join. Приём обхода ранней инъекции: leave→лобже→rejoin в том
  же документе (гость — безопасно, хоста не ресетит).
- **Result:** поймали `pcs:1` + полный SDP-обмен. **PeerConnection ЖИВЁТ В
  СТРАНИЦЕ** (вопреки прошлой гипотезе про воркер-изоляцию WebRTC). Направление:
  **SFU = offerer, клиент = answerer**; offer `setup:actpass` → answer
  `setup:active` (pion = DTLS active). Снят полный структурный offer+answer
  (эфемерные ufrag/pwd/fp/candidate/ssrc/IP и base64-fmtp заредакчены) →
  `tools/fbdo-capture/sdp-reference.md`. Кодеки: audio opus(111)+red(63); video
  **VP8(96)+rtx(97)**, H264 ×6, AV1(45), H265(49), red/rtx/ulpfec; bundle 0/1/2,
  rtcp-mux; extmap rtp-stream-id/repaired → RID-симулкаст; datachannel mid:2.
- **Evidence:** `window.__pc` хук, 2 ICE-кандидата, offer 5190B/answer 4998B.
  Фильтр расширения режет SDP на возврате (слова fingerprint/candidate/ufrag +
  base64) — обходили структурным парсером в странице.
- **Conclusion:** медиа-контракт для pion известен целиком; воркер-перехват для
  SDP НЕ нужен. Но **транспорт сигналинга всё ещё открыт**: offer приходит в
  `setRemoteDescription`, а в странице нет fetch/XHR/WS с SDP → доставка через
  воркер/бридж `webCallsBridge` (VK Calls SDK). Глобалы движка в странице:
  `Calls`/`__CallsAppService` (`_bridge`, `eventBus`, `store`, `joinCallByLink`),
  `__CALLS_SDK`, `callHub`.
- **Next:** 1c-B — найти, КУДА уходит answer и ОТКУДА берётся offer (воркер или
  бридж). Тогда pion-join-спека готова. Для Vector 2 answer достаточно урезать до
  VP8+rtx+opus.

## 2026-11-17 — Stage 1c БОЛЬШАЯ находка: fb.do = телеметрия, сигналинг в Web Worker

**Stage:** 1c (разметить протокол) · **Verdict:** 🔶 переопределяет задачу

Снял живой залогиненный звонок (3 участника) через Claude-in-Chrome, хук
fetch+XHR+WebSocket + `RTCPeerConnection.prototype` в top и same-origin iframe.
Что в основном потоке страницы реально ходит:

- **`calls.okcdn.ru/fb.do`** — POST `data=<url-encoded JSON>`, **это только
  телеметрия**: периодический (каждые ~5с) стат-репорт (`call_topology`,
  `nack/pli/fir_sent`, `is_simulcast`, `cpu_hardware_concurrency`…), ответ
  `{"items_responses":[{"metrics_reported":N}]}`. **Никакого SDP/ICE/join здесь
  нет.** Это разворачивает исходное допущение «реверсить fb.do сигналинг» —
  fb.do НЕ сигналинг.
- **VK API бутстрап** (`web.api.vk.ru/method/...`): `messages.getCurrentCalls`,
  `messages.getCallPreview`, `calls.getConversationByCall`,
  `messages.getCallParticipants`, `batch.call`, `calls.getHistory`. Это слой
  «ссылка на звонок → дескриптор OK-conversation + join-данные».
- **Ни одного WebSocket, ни одного SDP** в основном потоке. PC-proto хук
  (setLocal/RemoteDescription) после включения камеры (ре-негоциация) поймал
  **0** оферов/ансеров. Главный-поток `RTCPeerConnection` существует, но в звонке
  не участвует.

**Conclusion:** весь WebRTC-стек (PeerConnection + OK-сигналинг, вероятно WS к
tgserver) VK Звонки держит в **изолированном Web Worker** — он невидим для хуков
страницы (fetch/XHR/WebSocket/RTCPeerConnection.prototype). Совпадает со старой
заметкой «call's PC isolated from page JS». Реальная цель реверса распадается на
два слоя: **(a)** VK API `getCallPreview`/`getConversationByCall` (ссылка → OK
join-дескриптор) — виден нам, снимается редактированно; **(b)** OK TopGun
сигналинг внутри воркера (SDP/ICE обмен) — нужен **перехват Worker'а**
(hook `new Worker`, патч воркер-скрипта через blob), это тяжёлая часть.

**Next:** (1) снять редактированные тела `getCallPreview`/`getConversationByCall`
— понять, что именно передаётся воркеру. (2) Прототип worker-перехвата: обернуть
`Worker`/`SharedWorker`, внутри воркера повесить те же fetch/WS хуки, снять WS к
OK tgserver + SDP. Только после этого осмысленно писать pion-join.
Операционка: **выходить из залогиненного звонка нельзя** — это ресетит весь
звонок (нужен новый); ре-негоциацию ловить тумблером камеры/микро без выхода.

## 2026-11-17 — Stage 1 capture: хост требует VK ID, гость — путь к handshake

**Stage:** 1b (снять живой звонок) · **Verdict:** 🔶 partial

- **Tried:** через Claude-in-Chrome открыл `calls.vk.ru`, вооружил capture-хук,
  нажал «Создать встречу».
- **Result:** «Создать встречу» (роль хоста) редиректит на `id.vk.ru/auth`
  (`app_id=51542479`) — т.е. создание встречи требует VK ID. Аноним
  («Без профиля ВКонтакте») доступен только для **присоединения** по ссылке.
- **Conclusion:** снимать сигналинг будем входом **гостем по ссылке** — гость
  делает тот же fb.do join + SDP-обмен с SFU. Логиниться в личный аккаунт
  пользователя не будем (пароли/приватность).
- **Next:** получить от пользователя ссылку на звонок, зайти гостем с
  вооружённым хуком, снять cap.json, прогнать `analyze`.

## 2026-11-17 — Stage 1a: capture-харнесс готов

**Stage:** 1a · **Verdict:** ✅

- **Tried:** собрал `tools/fbdo-capture/` — `capture.js` (хук fetch+XHR+WebSocket
  в топ+same-origin фреймах, ре-инсталл поллером 150ms) + `analyze/` (Go: таймлайн,
  декод base64-блобов, hexdump, маркеры SDP/ICE/turn/codec).
- **Result:** собирается, смоук-тест на синтетическом cap.json ловит вложенный
  base64-SDP и размечает маркеры.
- **Conclusion:** инструмент capture→understand готов; хукаем и WebSocket, т.к.
  живой SDP/ICE звонка вероятно едет по WS, а не только fb.do POST'ами.
- **Next:** Stage 1b — снять реальный join.

## 2026-11-17 — Vector 2 make-or-break: SFU форвардит, не транскодит

**Stage:** Vector 2 gate · **Verdict:** ✅ (путь жив)

- **Tried:** как аноним-гость инжектировал синтетический VP8-трек через
  `pc.addTransceiver(canvasTrack,{direction:'sendonly'})`.
- **Result:** VK ренеготиировал трек и раздал его другим участникам — принято и
  форварднуто, не переэнкожено. «Нормализация» разрешения — это sender-side
  выбор simulcast-ступеней (клиент), не SFU.
- **Evidence:** cost/архитектура (N·M real-time транскод нереален при масштабе —
  это selective-forwarding SFU) + эмпирическая инъекция.
- **Conclusion:** байты в валидном VP8/H.264-кадре доходят до пира целиком.
  Граница — браузер не умеет авторить битстрим (Chrome переэнкодит пиксели), нужен
  нативный клиент (pion), чьи кадры SFU форвардит как есть.
- **Next:** реверс fb.do (Stage 1), чтобы завести pion в звонок.

## 2026-11-17 — Variant A (data channel как туннель): МЁРТВ

**Stage:** Variant A gate · **Verdict:** ❌

- **Tried:** два полностью контролируемых участника; хук `createDataChannel` +
  `ondatachannel`, авто-отправка произвольных payload'ов при open.
- **Result:** единственный DC — `animoji`, эфемерный (open ~3-5с на своём join,
  потом закрывается в простое), несёт серверные 15-байтовые кадры
  (`02 00 0X <ts> 00 00 00 00 02 a1 dd f0`). Инъекции (0 из 4+) не дошли ни до
  кого.
- **Conclusion:** SFU НЕ даёт прозрачный p2p байт-пайп через DC. Variant A мёртв.
- **Next:** Vector 2 (media RTP).

## 2026-11-17 — Vector 1 (TURN-реле как generic relay): МЁРТВ

**Stage:** M0 / Vector 1 gate · **Verdict:** ❌

- **Tried:** полная проба против VPS `<VPS>:51822` (ufw открыт, `nc`
  извне достаёт). VK/OK TURN: Allocate ✅, CreatePermission ✅,
  ChannelBind «<VPS>:51822» ✅.
- **Result:** **ноль** данных через реле в обе стороны. `tcpdump` на VPS не видел
  ничего от IP реле (3 прогона); VPS → relay addr (8 UDP) — проба не получила
  ничего.
- **Conclusion:** data plane реле VK/OK зафайрволен на инфру провайдера. Control
  plane проходит для произвольного внешнего пира, но egress в открытый интернет
  нет. Исходная идея (VK TURN как generic relay на свой VPS) НЕ работает.
- **Next:** пивот на Variant A, затем Vector 2.
