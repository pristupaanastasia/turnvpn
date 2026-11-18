# turnvpn

Прокидываем обычный интернет-трафик через **whitelisted** TURN-реле звонков VK,
чтобы машина в сети «только по белому списку» («суверенный режим») могла
достучаться до открытого интернета. Для сети трафик выглядит как WebRTC-звонок.

> **Личный исследовательский / учебный проект.** Возможность передавать данные
> в условиях включённых белых списков со скоростью до **~25 Мбит/с** — побочный эффект.

## Как это работает

TURN-серверы за звонками VK — в белом списке (одобренному приложению они нужны).
Они форвардят UDP на **один** пир, который ты сам привязал — если это твой VPS в
открытом интернете, реле становится «ступенькой» наружу из белого списка. Реле
пропускает только то, что похоже на WebRTC-медиа, поэтому мы заворачиваем всё в
**DTLS** (им оборачиваются настоящие звонки). Поверх — надёжный мультиплексированный
транспорт, наружу отдаём локальным SOCKS5-прокси.

```
  machine inside the whitelist                               open internet
┌──────────────────────────────┐                          ┌───────────────┐
│ browser / app                │                          │  any website  │
│   │ SOCKS5  127.0.0.1:1080   │                          └──────▲────────┘
│   ▼                          │                                 │ TCP
│ cmd/client                   │        VK TURN relay     ┌──────┴────────┐
│   pool of N tunnels, each:   │      (whitelisted IP)    │ cmd/server    │
│   smux → KCP → DTLS ─────────┼──► ChannelData ──► UDP ─►│  (your VPS)   │
│   over a pion/turn alloc     │   (looks like media)     │ DTLS→KCP→smux │
└──────────────────────────────┘                          │ → dial target │
                                                          └───────────────┘
```

Креды для реле берём из дескриптора join'а реального звонка
(`turn_server {username, credential, urls}`), который достаётся REST-цепочкой VK/OK
(`get_anonym_token` → `getAnonymousToken` → `anonymLogin` → `joinConversationByLink`).
Чтобы получить per-call токен, раз в ~10 минут нужно пройти проверку VK «я не робот» —
**человеком** (решаешь в браузере). turnvpn сам капчу **не** решает.

## Безопасность данных в DTLS-пакетах

Каждая датаграмма туннеля — вложенность слоёв, снаружи внутрь:

```
TURN ChannelData          — адресация реле; неотличимо от WebRTC-медиа
  └ DTLS 1.2 record       — AEAD ChaCha20-Poly1305, ECDHE-PSK (взаимная аутентификация)
      └ KCP segment       — надёжный ARQ (без крипты; DTLS уже шифрует)
          └ smux frame    — мультиплексирование потоков
              └ полезная нагрузка: заголовок "host:port" + проксируемый TCP-поток
                 (обычно сам по себе TLS/HTTPS, end-to-end до реального сайта)
```

**Что даёт DTLS**

- **Взаимная аутентификация (общий ключ).** Обе стороны знают секрет
  (`-secret` / `TURNVPN_SECRET`), cipher — **ECDHE-PSK**: хендшейк проходит,
  только если обе стороны вывели один ключ. Поэтому чужой без секрета не прикинется
  VPS'ом для клиента (**нет MITM**) и не использует VPS как открытый прокси.
  См. `internal/authdtls`.
- **Конфиденциальность + целостность** переданных байт. Cipher
  `ECDHE-PSK-WITH-CHACHA20-POLY1305` — AEAD: каждая запись зашифрована и защищена
  от подмены; эфемерный ECDHE даёт **forward secrecy** (компрометация секрета позже
  не расшифрует прошлый трафик). Реле, провайдер и любой на пути видят только шифротекст.
- **Маскировка.** DTLS — ровно то, во что обёрнуто WebRTC-медиа, поэтому реле его
  форвардит; сырые байты data plane реле молча дропает. Эта обёртка и делает весь
  подход рабочим.
- KCP специально **без** крипты (`NoneBlockCrypt`): DTLS уже шифрует датаграмму,
  второй шифр только жёг бы CPU.

**Эшелонированная защита.** DTLS уже аутентифицирует оба конца, но payload
**дополнительно** защищён прикладным слоем: веб — это HTTPS, т.е. вторая,
аутентифицированная TLS-сессия end-to-end до самого сайта — VPS и реле видят только
этот внутренний TLS-шифротекст, не твой plaintext. Итог: трафик зашифрован **дважды**
поверх недоверенного реле, и единственный, кто видит адрес назначения в открытую —
твой собственный VPS.

**Чего это всё равно НЕ скрывает: сам факт туннеля.** Payload зашифрован и
аутентифицирован, но устойчиво тяжёлый трафик через «звонок» и TURN-аллокация с
egress'ом на внешний IP палятся трафик-анализом. Шифрование защищает *что* ты шлёшь,
а не *факт*, что туннель работает. Снижай footprint (шейп битрейта, ротация
эндпоинтов); на невидимость не рассчитывай.

## Компоненты

- `cmd/client` — локальный SOCKS5-прокси; держит пул туннелей, каждый
  `smux → KCP → DTLS` поверх `pion/turn`-аллокации. Фоновый менеджер обновляет креды
  каждого туннеля до их протухания.
- `cmd/server` — на VPS: терминирует DTLS, демультиплексирует потоки, набирает цель
  каждого потока в открытом интернете и качает обратно.
- `internal/okapi`, `internal/vkapi` — REST-цепочка VK/OK, дающая креды `turn_server`.
- `internal/mux` — KCP + smux поверх одного DTLS-соединения.
- `internal/socks` — минимальный SOCKS5 (CONNECT) + per-stream target-заголовок.
- `internal/authdtls` — взаимная PSK-аутентификация DTLS (ECDHE-PSK).
- `internal/captcha` — пользовательский добытчик кредов (с решением капчи), втыкается
  в seam `CredsFunc` клиента. **Не часть ядра**; turnvpn сам капчу не решает.

## Запуск

Выбери общий секрет и задай его на **обеих** сторонах (`-secret` или
`TURNVPN_SECRET`) — он взаимно аутентифицирует туннель, чтобы выход не был открытым
прокси.

На VPS (открой UDP-порт в фаерволе):

```sh
go build -o server ./cmd/server
TURNVPN_SECRET='<общий секрет>' ./server -listen 0.0.0.0:56000
```

На своей машине (с per-call токеном, снятым из браузерного звонка):

```sh
go build -o client ./cmd/client
TURNVPN_SECRET='<тот же секрет>' TURN_CALL_TOKEN='<call-token>' ./client \
  -peer <IP_VPS>:56000 \
  -vk-link <токен из vk.ru/call/join/...> \
  -tunnels 2
```

После запуска клиент поднимает локальный **SOCKS5-прокси на `127.0.0.1:1080`**.
Пропиши его в браузере (или в системе) как прокси: тип **SOCKS5**, адрес
`127.0.0.1`, порт `1080`, и включи «проксировать DNS через прокси» (remote DNS) —
чтобы имена сайтов резолвились на VPS, а не локально (локальный DNS в шатдауне может
быть недоступен). Пример для Firefox: *Настройки → Настройки сети → Ручная настройка
прокси → узел SOCKS `127.0.0.1`, порт `1080`, SOCKS v5* + галочка *«Отправлять
DNS-запросы через прокси при использовании SOCKS v5»*. После этого весь трафик
браузера идёт через туннель. Держи браузерный звонок живым, пока туннель поднят.

Без `-token` клиент берёт креды из подключённого в `main` провайдера (напр.
решателя капчи) — его поставляешь ты.

## Статус

Vector 1 (TURN-реле + DTLS + SOCKS5) **работает end-to-end**: реальный HTTPS-браузинг
выходит через VPS по whitelisted-инфраструктуре VK, с пулом туннелей и проактивным
рефрешем кредов. Полная история — в `PLAN.md` и `LAB.md`, включая удалённый (в
git-истории) ранний media-RTP подход.

---

Tunnel ordinary internet traffic through the **whitelisted** TURN relays of VK
calls, so a machine stuck in a whitelist-only network ("суверенный режим") can
still reach the open internet. The traffic looks like a WebRTC call to the
network.

> **Personal research / educational project.** The ability to move data under
> active whitelist conditions at up to **~25 Mbit/s** is a side effect.

## How it works

The TURN servers behind VK's calls are whitelisted (the approved app needs them).
They forward UDP to one peer you bind — if that peer is your own VPS out on the
open internet, the relay becomes a stepping stone out of the whitelist. The relay
only forwards packets that look like WebRTC media, so we wrap everything in
**DTLS** (what real calls use). On top we run a reliable, multiplexed transport
and expose it locally as a SOCKS5 proxy.

```
  machine inside the whitelist                               open internet
┌──────────────────────────────┐                          ┌───────────────┐
│ browser / app                │                          │  any website  │
│   │ SOCKS5  127.0.0.1:1080   │                          └──────▲────────┘
│   ▼                          │                                 │ TCP
│ cmd/client                   │        VK TURN relay     ┌──────┴────────┐
│   pool of N tunnels, each:   │      (whitelisted IP)    │ cmd/server    │
│   smux → KCP → DTLS ─────────┼──► ChannelData ──► UDP ─►│  (your VPS)   │
│   over a pion/turn alloc     │   (looks like media)     │ DTLS→KCP→smux │
└──────────────────────────────┘                          │ → dial target │
                                                          └───────────────┘
```

Relay credentials come from a real call's join descriptor
(`turn_server {username, credential, urls}`), fetched via the VK/OK REST chain
(`get_anonym_token` → `getAnonymousToken` → `anonymLogin` → `joinConversationByLink`).
Getting the per-call token requires passing VK's "not a robot" check once per
~10 min — **human-in-the-loop** (you solve it in a browser). turnvpn does **not**
implement captcha solving.

## Data security in the DTLS packets

Each tunnel datagram is a nest of layers, outside in:

```
TURN ChannelData          — relay addressing; indistinguishable from WebRTC media
  └ DTLS 1.2 record       — AEAD ChaCha20-Poly1305, ECDHE-PSK (mutually authenticated)
      └ KCP segment       — reliable ARQ (no crypto; DTLS already encrypts)
          └ smux frame    — stream multiplexing
              └ payload: "host:port" header + the proxied TCP stream
                 (usually itself TLS/HTTPS, end-to-end to the real site)
```

**What DTLS gives us**

- **Mutual authentication (pre-shared key).** Both endpoints share a secret
  (`-secret` / `TURNVPN_SECRET`) and the suite is **ECDHE-PSK**: the handshake
  only completes if each side derives the same key, so an attacker without the
  secret can neither pose as the VPS to the client (**no MITM**) nor use the VPS
  as an open proxy. See `internal/authdtls`.
- **Confidentiality + integrity** of the tunneled bytes. The suite is
  `ECDHE-PSK-WITH-CHACHA20-POLY1305` — an AEAD, so every record is encrypted and
  tamper-evident; the ephemeral ECDHE part gives **forward secrecy**. The relay,
  the ISP and anyone on the path see only ciphertext.
- **Disguise.** DTLS is exactly what WebRTC media is wrapped in, so the relay
  forwards it; raw bytes are silently dropped by the relay's data plane.
- KCP deliberately uses **no** crypto (`NoneBlockCrypt`): DTLS already encrypts.

**Defense in depth.** The DTLS layer authenticates both ends, but the payload is
*also* protected by the application layer: browsing is HTTPS, a second
authenticated TLS session end-to-end to the real site — the VPS and relay only
ever see that inner ciphertext, never your plaintext. Your traffic is encrypted
twice over the untrusted relay.

**What it still does *not* hide: the tunnel's existence.** Sustained high-volume
traffic through a "call", and a TURN allocation egressing to an external IP, are
detectable by traffic analysis. Reduce footprint (bitrate shaping, endpoint
rotation); do not expect invisibility.

## Components

- `cmd/client` — local SOCKS5 proxy; a pool of tunnels, each `smux → KCP → DTLS`
  over a `pion/turn` allocation, with a background manager that refreshes
  credentials before they expire.
- `cmd/server` — on the VPS: terminates DTLS, demuxes streams, dials each
  stream's target and pipes it back.
- `internal/okapi`, `internal/vkapi` — the VK/OK REST chain for `turn_server` creds.
- `internal/mux` — KCP + smux over one DTLS connection.
- `internal/socks` — minimal SOCKS5 (CONNECT) + per-stream target header.
- `internal/authdtls` — mutual PSK DTLS authentication (ECDHE-PSK).
- `internal/captcha` — a user-supplied captcha fetcher that plugs into the
  client's `CredsFunc` seam. **Not part of the core.**

## Usage

Pick a shared secret and set it on **both** ends (`-secret` / `TURNVPN_SECRET`);
it mutually authenticates the tunnel so the exit is not an open proxy.

On the VPS (open the UDP port in the firewall):

```sh
go build -o server ./cmd/server
TURNVPN_SECRET='<shared secret>' ./server -listen 0.0.0.0:56000
```

On your machine (with a per-call token captured from a browser call):

```sh
go build -o client ./cmd/client
TURNVPN_SECRET='<same secret>' TURN_CALL_TOKEN='<call-token>' ./client \
  -peer <VPS_IP>:56000 \
  -vk-link <token from vk.ru/call/join/...> \
  -tunnels 2
```

The client opens a local **SOCKS5 proxy at `127.0.0.1:1080`**. Point the browser
(or system) at it: type **SOCKS5**, host `127.0.0.1`, port `1080`, and enable
"proxy DNS when using SOCKS v5" (remote DNS) so names resolve on the VPS, not
locally. Keep the browser call alive while the tunnel is up. Without `-token`,
the client uses the credential provider wired in `main` (e.g. a captcha fetcher)
— which you supply.

## Status

Vector 1 (TURN relay + DTLS + SOCKS5) is **working end-to-end**: real HTTPS
browsing exits via the VPS over whitelisted VK infrastructure, with a tunnel pool
and proactive credential refresh. See `PLAN.md` and `LAB.md` for the full history.
