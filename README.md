<p align="center">
  <img src="docs/assets/holubvpn.png" width="120" alt="HolubVPN" />
</p>

<h1 align="center">HolubVPN</h1>

Прокидываем обычный интернет-трафик через TURN-реле звонков VK (они в белых
списках), чтобы машина из сети, где доступ открыт только к адресам из белого списка,
могла достучаться до открытого интернета. Для сети трафик выглядит как WebRTC-звонок.

> **Личный исследовательский / учебный проект.** Возможность передавать данные
> в условиях включённых белых списков со скоростью до **~25 Мбит/с** — побочный эффект.

## Как это работает

TURN-серверы за звонками VK — в белом списке (одобренному приложению они нужны).
Они форвардят UDP на **один** привязанный к аллокации пир — если это VPS в открытом
интернете, реле становится «ступенькой» наружу из белого списка. Реле пропускает
только то, что похоже на WebRTC-медиа, поэтому всё заворачивается в **DTLS** (им
оборачиваются настоящие звонки). Поверх — надёжный мультиплексированный транспорт,
наружу — локальный SOCKS5-прокси.

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
**человеком** (она решается в браузере). turnvpn сам капчу **не** решает.

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

- **Взаимная аутентификация.** Обе стороны знают секрет (`-secret` /
  `TURNVPN_SECRET`), cipher — **ECDHE-PSK**: хендшейк проходит, только если обе
  стороны вывели один ключ. Поэтому чужой без секрета не прикинется VPS'ом для
  клиента (**нет MITM**) и не использует VPS как открытый прокси. Вместо единого
  секрета сервер может принять **keyring** per-client идентичностей (`-keyring` +
  `-identity` у клиента): у каждого клиента свой ключ, отзыв — удалением строки.
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
этот внутренний TLS-шифротекст, а не исходный plaintext. Итог: трафик зашифрован
**дважды** поверх недоверенного реле, а адрес назначения в открытую виден только
собственному VPS.

**Выход — не открытый релей наружу (SSRF-guard).** Цель потока диктует клиент, но
сервер отклоняет loopback, приватные, link-local и metadata-адреса
(`169.254.169.254`) и диалит по уже проверенному IP — чтобы туннель нельзя было
навести на внутренние сервисы VPS или облачную метадату (и чтобы DNS-rebinding не
проскочил между проверкой и диалом). См. `internal/tunnel`.

**Чего это всё равно НЕ скрывает: сам факт туннеля.** Payload зашифрован и
аутентифицирован, но устойчиво тяжёлый трафик через «звонок» и TURN-аллокация с
egress'ом на внешний IP палятся трафик-анализом. Шифрование защищает *что* передаётся,
а не *факт* работы туннеля. Footprint можно снижать — шейп битрейта (`-shape`) и
ротация выходов (`-peer` списком) уже есть, — но на невидимость рассчитывать не стоит.

## Компоненты

- `cmd/client` — локальный SOCKS5-прокси (CONNECT + UDP ASSOCIATE); держит пул
  туннелей, каждый `smux → KCP → DTLS` поверх `pion/turn`-аллокации. Фоновый менеджер
  обновляет креды каждого туннеля до их протухания. `-peer` через запятую раскладывает
  туннели по нескольким выходам (ротация).
- `cmd/server` — на VPS: терминирует DTLS, демультиплексирует потоки, фильтрует цель
  (SSRF-guard) и набирает её в открытом интернете (TCP и UDP), качает обратно.
- `internal/okapi`, `internal/vkapi` — REST-цепочка VK/OK, дающая креды `turn_server`.
- `internal/mux` — KCP + smux поверх одного DTLS-соединения + `Lane` (тег-демукс на
  надёжную дорожку для TCP и ненадёжную датаграммную для QUIC/UDP) + шейп битрейта.
- `internal/tunnel` — реле потоков и UDP-flow + `SSRF-guard` (блок loopback/приватных/
  link-local/metadata-адресов, диал по проверенному IP).
- `internal/socks` — SOCKS5 CONNECT и UDP ASSOCIATE + per-stream target-заголовок.
- `internal/authdtls` — взаимная PSK-аутентификация DTLS (ECDHE-PSK); поддерживает
  как единый `-secret`, так и per-client keyring (identity→secret) с отзывом.
- `internal/vpntun`, `mobile/` — туннель как `Dialer` + gomobile-биндинг для Android.
- `internal/captcha` — пользовательский добытчик кредов (с решением капчи), втыкается
  в seam `CredsFunc` клиента. **Не часть ядра**; turnvpn сам капчу не решает.

## Запуск

Общий секрет задаётся на **обеих** сторонах (`-secret` или `TURNVPN_SECRET`) — он
взаимно аутентифицирует туннель, чтобы выход не был открытым прокси.

На VPS (открой UDP-порт в фаерволе):

```sh
go build -o server ./cmd/server
TURNVPN_SECRET='<общий секрет>' ./server -listen 0.0.0.0:56000
```

На клиентской машине (с per-call токеном, снятым из браузерного звонка):

```sh
go build -o client ./cmd/client
TURNVPN_SECRET='<тот же секрет>' TURN_CALL_TOKEN='<call-token>' ./client \
  -peer <IP_VPS>:56000 \
  -vk-link <токен из vk.ru/call/join/...> \
  -tunnels 2
```

После запуска клиент поднимает локальный **SOCKS5-прокси на `127.0.0.1:1080`**. Его
нужно указать в браузере (или в системе) как прокси: тип **SOCKS5**, адрес
`127.0.0.1`, порт `1080`, и включить «проксировать DNS через прокси» (remote DNS) —
чтобы имена сайтов резолвились на VPS, а не локально (локальный DNS в шатдауне может
быть недоступен). Пример для Firefox: *Настройки → Настройки сети → Ручная настройка
прокси → узел SOCKS `127.0.0.1`, порт `1080`, SOCKS v5* + галочка *«Отправлять
DNS-запросы через прокси при использовании SOCKS v5»*. После этого весь трафик
браузера идёт через туннель. Браузерный звонок нужно держать живым, пока туннель
поднят.

Без `-token` клиент берёт креды из подключённого в `main` провайдера (напр.
решателя капчи), который поставляет пользователь.

QUIC/HTTP-3 и прочий UDP идут через туннель наравне с TCP (SOCKS5 UDP ASSOCIATE на
клиенте, ненадёжная датаграммная дорожка внутри). DNS резолвится на VPS.

**Опциональные флаги:**

- `-peer a:56000,b:56000` — несколько выходов; туннели раскладываются по ним ротацией.
- `-shape 2mbit` (обе стороны) — шейп исходящего битрейта под «конверт» звонка.
- Per-client доступ вместо единого секрета: на сервере `-keyring file`
  (строки `identity secret`), у клиента `-identity <id>`. Отзыв клиента — удалить его
  строку и перезапустить сервер.

## Android

Нативное приложение **HolubVPN** (Kotlin + `VpnService`, Go-ядро через gomobile
`.aar`: tun2socks читает tun → туннель) — в каталоге [`android/`](android/README.md):
там сборка APK, запуск и оговорки. DNS идёт через туннель по TCP автоматически.

## Статус

Vector 1 (TURN-реле + DTLS + SOCKS5) **работает end-to-end**: реальный HTTPS-браузинг
и QUIC/HTTP-3 выходят через VPS по инфраструктуре VK из белых списков, с пулом
туннелей и проактивным обновлением кредов. Поднят харденинг: взаимная аутентификация
(единый секрет или per-client keyring), SSRF-guard на выходе, non-root systemd на
VPS, секрет в Keystore на Android, шейп битрейта и ротация выходов. Полная история —
в `PLAN.md` и `LAB.md`, включая удалённый (в git-истории) ранний media-RTP подход.

---

Tunnel ordinary internet traffic through the **whitelisted** TURN relays of VK
calls, so a machine stuck in a whitelist-only network can still reach the open
internet. The traffic looks like a WebRTC call to the network.

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

- **Mutual authentication.** Both endpoints share a secret (`-secret` /
  `TURNVPN_SECRET`) and the suite is **ECDHE-PSK**: the handshake only completes if
  each side derives the same key, so an attacker without the secret can neither pose
  as the VPS to the client (**no MITM**) nor use the VPS as an open proxy. Instead of
  one shared secret the server can accept a **keyring** of per-client identities
  (`-keyring` + the client's `-identity`): each client has its own key, revoked by
  deleting its line. See `internal/authdtls`.
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

**The exit is not an open relay (SSRF guard).** The client dictates each stream's
target, but the server rejects loopback, private, link-local and metadata addresses
(`169.254.169.254`) and dials the already-verified IP — so the tunnel can't be
pointed at the VPS's internal services or cloud metadata, and DNS rebinding can't
slip in between the check and the dial. See `internal/tunnel`.

**What it still does *not* hide: the tunnel's existence.** Sustained high-volume
traffic through a "call", and a TURN allocation egressing to an external IP, are
detectable by traffic analysis. Footprint can be reduced — bitrate shaping
(`-shape`) and exit rotation (a `-peer` list) are in place — but do not expect
invisibility.

## Components

- `cmd/client` — local SOCKS5 proxy (CONNECT + UDP ASSOCIATE); a pool of tunnels,
  each `smux → KCP → DTLS` over a `pion/turn` allocation, with a background manager
  that refreshes credentials before they expire. A comma-separated `-peer` spreads
  tunnels across several exits (rotation).
- `cmd/server` — on the VPS: terminates DTLS, demuxes streams, filters the target
  (SSRF guard) and dials it on the open internet (TCP and UDP), piping it back.
- `internal/okapi`, `internal/vkapi` — the VK/OK REST chain for `turn_server` creds.
- `internal/mux` — KCP + smux over one DTLS connection, plus a `Lane` (tag-demux into
  a reliable path for TCP and an unreliable datagram path for QUIC/UDP) and rate shaping.
- `internal/tunnel` — stream/UDP-flow relay + an `SSRF guard` (blocks loopback/private/
  link-local/metadata addresses, dials the verified IP).
- `internal/socks` — SOCKS5 CONNECT and UDP ASSOCIATE + per-stream target header.
- `internal/authdtls` — mutual PSK DTLS authentication (ECDHE-PSK); supports a single
  `-secret` or a per-client keyring (identity→secret) with revocation.
- `internal/vpntun`, `mobile/` — the tunnel as a `Dialer` + gomobile binding for Android.
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

QUIC/HTTP-3 and other UDP ride the tunnel alongside TCP (SOCKS5 UDP ASSOCIATE on
the client, an unreliable datagram lane inside). DNS resolves on the VPS.

**Optional flags:**

- `-peer a:56000,b:56000` — multiple exits; tunnels are spread across them (rotation).
- `-shape 2mbit` (both ends) — shape the outbound bitrate toward a call-like envelope.
- Per-client access instead of one shared secret: `-keyring file` on the server
  (`identity secret` lines) and `-identity <id>` on the client; revoke a client by
  deleting its line and restarting the server.

## Android

The native Android app **HolubVPN** (Kotlin + `VpnService`, Go core bound through a
gomobile `.aar`: tun2socks reads the tun → our tunnel) lives in
[`android/`](android/README.md), with its own build & run notes.

## Status

Vector 1 (TURN relay + DTLS + SOCKS5) is **working end-to-end**: real HTTPS
browsing and QUIC/HTTP-3 exit via the VPS over whitelisted VK infrastructure, with
a tunnel pool and proactive credential refresh. Hardening is in place: mutual
authentication (a single secret or a per-client keyring), an SSRF guard on the exit,
a non-root systemd service on the VPS, the secret in the Android Keystore, bitrate
shaping and exit rotation. See `PLAN.md` and `LAB.md` for the full history.
