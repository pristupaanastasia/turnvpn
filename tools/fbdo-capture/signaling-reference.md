# VK Звонки — сигналинг (полный флоу, captured 2026-11-17)

Снято с живого гостевого звонка через Claude-in-Chrome. **Секреты не включены:**
session_key, session_secret_key, anonymToken, ws-token, TURN credential/username,
OK `id` — все заредакчены. Здесь — структура протокола (методы, эндпоинты,
имена параметров), которой достаточно, чтобы спроектировать pion-join.

## Главный вывод

Сигналинг VK Звонков — это **OK (Одноклассники) API поверх `calls.okcdn.ru/fb.do`**
плюс **медиа-сигнальный WebSocket к SFU**. Канал VK (`web.api.vk.ru`,
`subscribeToQueue`) — для чата/реакций/участников, НЕ для медиа (у гостя
`subscribeToQueue` вообще возвращает error, а звонок работает). fb.do — это OK
API: часть вызовов = джойн/логин, часть = телеметрия.

## VK-бутстрап: как получить `callToken` (из `webCallsBridge.js`)

Перед OK-частью клиент добывает гостевой `callToken` через VK. Реализовано в
`internal/vkapi`. Два шага:

1. **`get_anonym_token`** (login.vk.ru) → анонимный VK access-token (JWT с
   `anonym_id_long`). Параметры (из чанка): `client_id=6287487` (VK web app,
   он же `client_id` в `subscribeToQueue`), `client_secret=QbYic1K3lEV5kTGiqlq2`
   (публичный), `token_type=messages`, `version=5.226`, домен `login.vk.ru`.
   **Точная HTTP-форма (`act`/endpoint/кодирование) — в VK login-модуле, которого
   нет; помечено EXPERIMENTAL.** Уточнить захватом реального `get_anonym_token`
   (свежий гость с очищенными cookie) или скачав login-чанк.
2. **`calls.getAnonymousToken`** — форма подтверждена живым захватом (2026-11-18):
   `POST web.api.vk.ru/method/calls.getAnonymousToken?v=5.289&client_id=6287487`
   (**`v` и `client_id` в query**), тело: `vk_join_link`, `name`, `access_token`
   (288-симв. JWT). → `response.token` = **`callToken`**. Грунтовано + тесты.

   **⚠️ Captcha-гейт:** VK может вернуть ошибку `{"error_code":14,"error_msg":
   "Captcha need","is_enabled_captcha":true,"redirect_uri":...}` — анти-абуз,
   триггерится частыми джойнами с одного IP. Headless-клиент капчу решать не может
   и не должен (запрещено). `GetCallToken` отдаёт это отдельной понятной ошибкой.
   Это реальный потолок полной автоматизации: при captcha нужен ручной
   шаг/ротация IP.

`callToken` далее идёт в OK: `auth.anonymLogin` (`auth_token`) и
`vchat.joinConversationByLink` (`anonymToken`). Токены короткоживущие.

## Поток джойна (OK API над `fb.do`, POST form-encoded)

Все запросы — `POST https://calls.okcdn.ru/fb.do`, тело form-encoded с общими
полями `method`, `format=JSON`, `application_key=CGMMEJLGDIHBABABA` (публичный
app-key веб-клиента), `session_key`, подпись.

1. **`auth.anonymLogin`** → `{ session_key, session_secret_key, api_server:
   "https://calls.okcdn.ru/" }`. Анонимная OK-сессия для гостя. (У залогиненного
   VK-юзера — свой аналог авторизации; для гостя это `anonymLogin` + `anonymToken`,
   который приходит с VK-стороны из `calls.getAnonymousToken`.)
2. **`system.getInfo`** → `{ serverTime }`.
3. **`vchat.joinConversationByLink`** — ДЖОЙН. Запрос:
   | параметр | значение |
   |----------|----------|
   | `joinLink` | токен из ссылки `vk.ru/call/join/<token>` |
   | `isVideo` | false/true |
   | `protocolVersion` | `5` |
   | `capabilities` | hex-флаги (напр. `6F7F`) |
   | `anonymToken` | гостевой токен (32 симв.) |
   | `application_key` | `CGMMEJLGDIHBABABA` |
   | `session_key` | из `anonymLogin` |

   Ответ — **дескриптор конференции**:
   ```
   {
     "client_type": "VK",
     "device_idx": <n>,
     "id": "<conversation/participant id>",
     "p2p_forbidden": <bool>,
     "token": "<ws session token>",
     "endpoint": "wss://videowebrtc.okcdn.ru/ws2",      ← СИГНАЛЬНЫЙ WEBSOCKET
     "wt_endpoint": "https://videowebrtc.okcdn.ru:23456/wt",  ← WebTransport-альтернатива
     "ws_ip_addresses": ["155.212.204.173", ...],
     "wt_ip_addresses": ["155.212.204.173", ...],
     "stun_server": { "urls": ["stun:90.156.236.124:19302"] },
     "turn_server": {
       "urls": ["turn:90.156.236.124:19302", "turn:90.156.236.85:19302"],
       "username": "<redacted>", "credential": "<redacted>"
     }
   }
   ```
   (STUN/TURN IP `90.156.236.x:19302` — те же OK-реле, что светились в Vector-1.)

4. **`vchat.clientStats`** (многократно, ~каждые 5с) — телеметрия
   (`call_topology`, nack/pli/fir, `is_simulcast`, …). НЕ сигналинг.

## Где SDP/ICE

**Внутри WebSocket `wss://videowebrtc.okcdn.ru/ws2`** (или WebTransport `wt`),
не в HTTP. Движок (in-page chunk `60828.js`) открывает этот WS с закешированным
конструктором `WebSocket` (до того, как страничный хук успевает обернуть), поэтому
страничный WS-хук его не видит. SDP offer/answer, снятый через
`RTCPeerConnection` (см. `sdp-reference.md`), идёт именно по ws2:
SFU шлёт offer → клиент отвечает answer; ICE trickle — тоже там.

## Что нужно pion (Vector 2) — спека джойна

1. Получить VK anonymToken: `web.api.vk.ru/method/calls.getAnonymousToken`
   (для гостя; залогиненный — свой путь).
2. OK `auth.anonymLogin` (fb.do) → session_key.
3. OK `vchat.joinConversationByLink(joinLink, protocolVersion=5, capabilities,
   anonymToken, session_key)` (fb.do) → endpoint ws2 + token + STUN/TURN.
4. Открыть WS `wss://videowebrtc.okcdn.ru/ws2` с token → принять offer от SFU,
   ответить answer (answerer, DTLS active, VP8+rtx+opus — см. sdp-reference.md),
   ICE через выданные STUN/TURN.
5. Слать `vchat.clientStats` для правдоподобия (телеметрия).

## Транспорт сигнального канала: WebTransport (подтверждено)

Хук конструкторов показал: движок открывает к `videowebrtc.okcdn.ru`
**WebTransport** (HTTP/3), а не WebSocket — поймано 3 WebTransport-сокета, 0
WebSocket. `endpoint: wss://.../ws2` — это фолбэк; основной путь — `wt_endpoint:
https://videowebrtc.okcdn.ru:23456/wt`. Движок читает глобальный конструктор при
подключении (хук конструктора сработал после ре-джойна).

### Wire-формат снят (статический разбор `60828.23368b36.js`, 2026-11-17)

**Транспорт.** Движок использует WebTransport через **WebSocket-совместимую
обёртку**: `connect()` → `new WebTransport(url)` → `await ready` → **один**
`createBidirectionalStream()` → `writer = stream.writable.getWriter()`, чтение
`readLoop(stream.readable)`. Наверх отдаёт события `open`/`message`/`close` как
обычный WebSocket. (`ws2` WebSocket — тот же верхний интерфейс, другой транспорт.)

**Фрейминг (эмуляция сообщений поверх байтового стрима).** Каждое сообщение:
```
[ length-prefix ][ payload ]
payload = (опц. gzip/deflate)( UTF-8 строка )
```
- Энкодер (`am`): `TextEncoder.encode(str)` → `compress()` → длина-префикс
  (`lengthEncoder`) + тело.
- Декодер (`af`): копит буфер, читает `readLength()` → ждёт N байт → `decompress`
  → `TextDecoder.decode` → строка-сообщение (их может быть несколько в одном
  WT-чанке).
- **Компрессия** выбирается из query-параметра URL (`gzip`/`deflate`/
  `deflate-raw`/нет).
→ Т.е. это **length-prefixed, опц. сжатые, UTF-8 (JSON) сообщения** — НЕ сырой
TopGun-base64. На прикладном уровне — обычные строковые сообщения.

**OK REST — подписи НЕТ.** Сборщик запроса `sQ(method, params, skipAuth)`:
кладёт `method`, `format="JSON"`, `application_key` (=`ry.apiKey`), `session_key`
(или `access_token`), JSON-стрингует вложенные объекты, и возвращает
`k=encodeURIComponent(v)&...`. **Никакого `sig`/md5** (md5 в чанке отсутствует) —
авторизация только по `session_key`/`access_token`. Это сильно упрощает pion.

**`auth.anonymLogin` (точные параметры):**
```
method=auth.anonymLogin, application_key=<apiKey>,
session_data = { device_id:<uuid>, client_version:<apiClientVersion>,
                 client_type:"SDK_JS", auth_token:<callToken>, version:3 }
```
`callToken` — токен звонка (гостевой), из VK-стороны. Ответ кешируется
(`session_key`, `session_secret_key`, `uid`).

**Realtime-опкоды сигналинга** (строковые консты в движке) — прикладной протокол
поверх строковых сообщений: `SIGNALLED_ACTIVE_PARTICIPANTS`,
`SIGNALLED_SPEAKER_CHANGED`, `SIGNALLED_STALLED_PARTICIPANTS`, `TOPOLOGY_CHANGED`,
`SPEAKER_CHANGED`, `REMOTE_TRACK_ADDED`/`REMOTE_TRACK_REMOVED`, `TRACK_REPLACED`,
`STATE_CHANGED`, `PEER_CONNECTION_CLOSED`, `NETWORK_STATUS`, `VOLUMES_DETECTED`,
`WAITING_HALL`/`WAIT_FOR_ADMIN`, `ANIMOJI*`, `ASR_TRANSCRIPTION`. Внутри этих
сообщений едут SDP offer/answer и ICE. Точные JSON-схемы каждого опкода — читать
из `60828.js` по мере имплементации (большая, но прямолинейная поверхность).

**Прочее из чанка:** `patchLocalSDP(...)` — как SDK правит SDP (выбор H264/VP8/VP9,
`red`, `rtcp-fb`); VP8-декод — libvpx в Worker (`decoder-init-fail-libvpx`).
Чанки лежат на `https://st.vk.ru/dist/projects/vk-web/chunks/js/` (имена с хешем
меняются при релизе — брать актуальные из рантайма).

### Сигнальный URL: query-ключи (из `_buildSignalingEndpoint` + `_buildUrl`)

Клиент берёт `endpoint`/`wt_endpoint` из дескриптора как базу и добавляет query.
Источник — чанк `60828.js`. Реализовано в `internal/wttransport`.

`_buildSignalingEndpoint` (идентичность), добавляет:
- `userId` (всегда, даже 0), `entityType`, `deviceIdx` (всегда, даже 0),
  `conversationId`, `token` (per-session из дескриптора), и опц.
  `ispAsNo`/`ispAsOrg`/`locCc`/`locReg`.

`_buildUrl` (клиент/соединение), добавляет:
- `platform`, `appVersion`, `version` (=`protocolVersion`, 5), `device`,
  `capabilities` (hex-флаги), `clientType`; опц. `tgt`/`recoverTs`/`partIdx`/
  `partCount`/`peerId`.
- **только для WebTransport:** `compression=deflate-raw` и `ua` (user-agent).
  → WT-leg всегда `deflate-raw` (значит `wtframe`/`wtsignal` на нём = DeflateRaw).

Сериализация — `URLSearchParams` (пробел → `+`, что совпадает с Go `url.Values`).
`userId`/`entityType` берутся НЕ из `Descriptor` (там только `id`/`device_idx`/
`token`) — `userId` = `uid` из `anonymLogin`/join-ответа; `entityType`/
`conversationId` — из параметров разговора. Развести при dial (rung 5).

### Схема сообщений ws2/WebTransport (СНЯТА ЖИВЬЁМ 2026-11-18)

Снято перехватом WebTransport-стрима в странице (tap getReader/getWriter +
deflate-raw inflate + varint-фрейминг). Сообщения — **JSON-строки** (или текст
`ping`/`pong`). SDP — **plaintext в JSON**, без base64.

**Клиент → сервер (команды):** `{ "command": <name>, "sequence": <n>, ... }`
- `transmit-data` — несёт WebRTC-примитив в `data` (союз):
  - offer/answer: `data = { "sdp": { "type": "offer"|"answer", "sdp": "<SDP>" }, "animojiVersion": <n> }`
  - ICE: `data = { "candidate": { "candidate": "<cand>", "sdpMid": "0", "sdpMLineIndex": <n>, "usernameFragment": "<ufrag>" } }`
  - плюс `participantId`, `participantType: "USER"`.
- `update-media-modifiers` — `{ mediaModifiers }`.
- `change-media-settings` — `{ mediaSettings: { isAudioEnabled, isVideoEnabled, isScreenSharingEnabled, isAnimojiEnabled, … } }`.
- `custom-data` — `{ data: { sdk: { type:"bad-net", rtt, loss } }, participantId }` (стат-сигналы).

**Сервер → клиент (нотификации):** `{ "type": "notification", "notification": <name>, "stamp": <n>, "peerId": {id, type:"WEB_TRANSPORT"}, ... }`
- `connection` — джойн подтверждён: `conversation { id, joinLink, participants[], state:"ACTIVE", topology:"DIRECT", features, options }` + `conversationParams { serverTime, stun{urls}, turn{urls, username:"<ts>:<id>", credential} }` (TURN-креды здесь, эфемерные).
- `transmitted-data` — SDP/ICE **от пира/SFU**: тот же `data`-союз (`sdp` offer/answer, или `candidate`), `participantId`, `participantType`.
- прочие: участники/медиа/роли.

**Ack:** `{ "type": "response", "response": <commandName>, "sequence": <n>, "stamp": <n> }` — по `sequence`.
**Heartbeat:** текст `ping` → ответ `pong` (6 байт).

→ pion-сигналинг: принять `connection` → дождаться `transmitted-data` с
`data.sdp` offer → отправить `transmit-data` c `data.sdp` answer (+ `sequence`,
`participantId`, `participantType:"USER"`) → трикл ICE через `transmit-data`
`data.candidate` ↔ `transmitted-data`; отвечать `pong` на `ping`.

### Итог: что ещё реально нужно для pion

Архитектура снята полностью. Для рабочего клиента осталось (всё из статики/кода,
без браузера):
1. Точные JSON-схемы realtime-опкодов (как именно завёрнуты offer/answer/ICE) —
   дочитать обработчики в `60828.js`.
2. VK-цепочка до `calls.getAnonymousToken` (как получить `callToken`).
3. Реализовать: длина-префикс фрейминг + (де)компрессию над WT bidi stream.

## Приватность

Ответы `web.api.vk.ru` (`getParticipantsByIds`) несут персданные (имя,
screen_name, фото) — не сохранять. Все токены/креды здесь заредакчены. Токены
короткоживущие и привязаны к сессии/звонку.
