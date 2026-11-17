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

## Последний незакрытый кусок

**Формат сообщений внутри ws2** (как завёрнуты SDP/ICE: JSON? OK TopGun base64?).
Не снят — движок кеширует `WebSocket` до инъекции хука. Нужен перехват WebSocket
до загрузки движка (ранняя инъекция) или патч конструктора `WebSocket` в самом
начале документа. Это единственное, что осталось до полной pion-join-спеки.

## Приватность

Ответы `web.api.vk.ru` (`getParticipantsByIds`) несут персданные (имя,
screen_name, фото) — не сохранять. Все токены/креды здесь заредакчены. Токены
короткоживущие и привязаны к сессии/звонку.
