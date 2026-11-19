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
оборачиваются настоящие звонки). Внутри DTLS бежит **WireGuard**: клиент — на нём
через userspace-стек (без root/TUN), выход на VPS терминирует WireGuard и выпускает
трафик в интернет. Наружу на клиенте — локальный SOCKS5-прокси.

```
  machine inside the whitelist                               open internet
┌──────────────────────────────┐                          ┌───────────────┐
│ browser / app                │                          │  any website  │
│   │ SOCKS5  127.0.0.1:1080   │                          └──────▲────────┘
│   ▼                          │                                 │ TCP/UDP
│ cmd/client                   │        VK TURN relay     ┌──────┴────────┐
│   WireGuard (userspace)      │      (whitelisted IP)    │ cmd/server    │
│   → DTLS ────────────────────┼──► ChannelData ──► UDP ─►│  (your VPS)   │
│   over a pion/turn alloc     │   (looks like media)     │ DTLS→WireGuard│
└──────────────────────────────┘                          │ → dial target │
                                                          └───────────────┘
```

### Серверный выход — сменный (`-exit`)

Идентичность клиента = его WireGuard-pubkey и развязана с транспортом, поэтому
**выход можно менять, не трогая клиента и протокол** (carrier один и тот же):

```
          DTLS (взаимный PSK)  ─►  идентичность клиента = WG-pubkey
                                          │
                 ┌────────────────────────┴────────────────────────┐
      -exit userspace (по умолчанию)                  -exit kernel (вариант B)
   ┌───────────────────────────────┐          ┌───────────────────────────────┐
   │ internal/wggw                 │          │ internal/kernelexit           │
   │  gVisor-netstack (userspace)  │          │  релей WG-датаграмм в ядро     │
   │  → net.Dial (явный фильтр)    │          │  → wg0 (kernel WireGuard)      │
   │                               │          │  → nftables: MASQUERADE        │
   │  non-root · SSRF-safe         │          │    + egress-фильтр (анти-SSRF) │
   │  ~2–5 МБ/клиент               │          │  пиры: reconcile по SIGHUP     │
   │                               │          │  ~0.2 МБ/клиент · нужен root    │
   └───────────────┬───────────────┘          └───────────────┬───────────────┘
                   └───────────►  открытый интернет  ◄─────────┘
```

Детали варианта B и его обязательный egress-фильтр — в `docs/kernel-exit.md`.

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
      └ WireGuard         — Noise/ChaCha20-Poly1305: второй шифр + маршрутизация
          └ IP-пакеты     — TCP/UDP приложения (веб обычно сам по себе TLS/HTTPS,
                            end-to-end до реального сайта)
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
- **WireGuard внутри** добавляет собственную взаимную аутентификацию по ключам и
  второй шифр (Noise + ChaCha20-Poly1305), и заодно маршрутизирует IP-пакеты до
  выхода. Сырой WireGuard реле бы дропнуло — поэтому он едет внутри DTLS, а не вместо.

**Эшелонированная защита.** Поверх недоверенного реле трафик зашифрован в несколько
слоёв: внешний DTLS (ECDHE-PSK) + внутренний WireGuard (ключи), а для веба ещё и
HTTPS — третья, аутентифицированная TLS-сессия end-to-end до самого сайта, которую
ни реле, ни даже собственный VPS расшифровать не могут. Адрес назначения в открытую
виден только собственному VPS.

**Выход — не открытый релей наружу (SSRF-guard).** Цель диктует клиент, но выход
отклоняет loopback, приватные, link-local и metadata-адреса (`169.254.169.254`) и
диалит по уже проверенному IP — чтобы туннель нельзя было навести на внутренние
сервисы VPS или облачную метадату (и чтобы DNS-rebinding не проскочил между
проверкой и диалом). SSRF-guard включён по умолчанию. См. `internal/wggw`.

**Чего это всё равно НЕ скрывает: сам факт туннеля.** Payload зашифрован и
аутентифицирован, но устойчиво тяжёлый трафик через «звонок» и TURN-аллокация с
egress'ом на внешний IP палятся трафик-анализом. Шифрование защищает *что* передаётся,
а не *факт* работы туннеля. Footprint можно снижать — шейп битрейта (`-shape`) и
ротация выходов (`-peer` списком) — но на невидимость рассчитывать не стоит.

## Компоненты

- `cmd/client` — локальный SOCKS5-прокси (CONNECT + UDP ASSOCIATE); поверх DTLS/TURN
  поднимает WireGuard (`wgclient`) и диалит через него. Фоновый менеджер
  переустанавливает туннель до протухания кредов; `-peer` через запятую — ротация выходов.
- `cmd/server` — на VPS: терминирует DTLS и гоняет WireGuard-выход (`wggw`), который
  набирает цель в открытом интернете (SSRF-guard по умолчанию) и качает обратно.
- `internal/okapi`, `internal/vkapi` — REST-цепочка VK/OK, дающая креды `turn_server`.
- `internal/wgbind` — `conn.Bind`: WireGuard поверх carrier-`net.Conn` (DTLS/TURN).
- `internal/wggw` — серверный WG-выход (по умолчанию): gvisor-netstack (promiscuous) + TCP/UDP-форвардеры.
- `internal/kernelexit` — альтернативный выход `-exit kernel`: релей в ядро (`wg0` + nft MASQUERADE). Масштабируемее, но нужен root; см. `docs/kernel-exit.md`.
- `internal/wgclient` — клиентский WG на netstack + `DialContext` (resolve через туннель).
- `internal/wgkey` — генерация/деривация WG-ключей (Curve25519, hex).
- `internal/socks` — SOCKS5 CONNECT и UDP ASSOCIATE + per-stream target-заголовок.
- `internal/authdtls` — взаимная PSK-аутентификация DTLS (ECDHE-PSK); поддерживает
  как единый `-secret`, так и per-client keyring (identity→secret) с отзывом.
- `internal/carrier` — установка несущего слоя (креды → TURN Allocate → PSK-DTLS),
  общая для десктопа и Android.
- `internal/captcha` — пользовательский добытчик кредов (с решением капчи), втыкается
  в seam `CredsFunc` клиента. **Не часть ядра**; turnvpn сам капчу не решает.

## Запуск

Два уровня аутентификации: **DTLS-секрет** (`-secret`/`TURNVPN_SECRET`) на обеих
сторонах аутентифицирует carrier, а **ключи WireGuard** — сам туннель. Сначала
сгенерируй по паре ключей:

```sh
go build -o server ./cmd/server && go build -o client ./cmd/client
./server -genkey   # → server private + public
./client -genkey   # → client private + public
```

На VPS: приватный ключ сервера + файл пиров (`<публичный ключ клиента> <его адрес>`
на строку) + DTLS-секрет (открой UDP-порт в фаерволе):

```sh
echo '<публичный ключ клиента> 10.6.0.2/32' > peers.txt
echo '<приватный ключ сервера>' > wg.key
TURNVPN_SECRET='<общий секрет>' ./server \
  -listen 0.0.0.0:56000 -wg-key wg.key -wg-peers peers.txt
```

На клиентской машине (с per-call токеном, снятым из браузерного звонка):

```sh
TURNVPN_SECRET='<тот же секрет>' TURN_CALL_TOKEN='<call-token>' \
TURNVPN_WG_KEY='<приватный ключ клиента>' TURNVPN_WG_SERVER_PUB='<публичный ключ сервера>' \
./client \
  -peer <IP_VPS>:56000 \
  -vk-link <токен из vk.ru/call/join/...> \
  -wg-addr 10.6.0.2
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
клиенте). DNS резолвится на VPS (WireGuard несёт весь IP, имена резолвятся на выходе).

**Опциональные флаги:**

- `-peer a:56000,b:56000` — несколько выходов; туннель ротируется по ним при переподключении.
- `-shape 2mbit` (обе стороны) — шейп исходящего битрейта под «конверт» звонка.
- Несколько клиентов: добавь по строке на каждого в `-wg-peers` (у каждого свой
  `-wg-addr`). Дополнительно можно включить per-client DTLS-доступ: на сервере
  `-keyring file` (строки `identity secret`), у клиента `-identity <id>`.

## Android

Нативное приложение **HolubVPN** (Kotlin + `VpnService`, Go-ядро через gomobile
`.aar`: WireGuard на tun → туннель) — в каталоге [`android/`](android/README.md):
там сборка APK, запуск и оговорки. DNS идёт через туннель по TCP автоматически.

## Статус

Vector 1 (TURN-реле + DTLS + **WireGuard**) **работает end-to-end**, в том числе на
Android: реальный HTTPS-браузинг и QUIC/HTTP-3 выходят через VPS по инфраструктуре VK
из белых списков, с проактивным обновлением кредов. Data plane — **WireGuard** и на
десктопе (`cmd/client`/`cmd/server`), и в Android-ядре (`wireguard-go` на
VpnService-tun); старый KCP/smux/SOCKS/tun2socks удалён. Подтверждено вживую: приложение
**HolubVPN** через реальный TURN-реле VK выпускает трафик через VPS (в браузере
эмулятора `api.ipify.org` вернул IP VPS). Поднят харденинг: взаимная аутентификация
(единый секрет или per-client keyring), SSRF-guard на выходе, non-root systemd на VPS,
секрет и приватный WG-ключ в Android Keystore. Полная история — в `PLAN.md` и `LAB.md`,
включая удалённый (в git-истории) ранний media-RTP подход.

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
**DTLS** (what real calls use). Inside the DTLS we run **WireGuard**: the client
on a userspace stack (no TUN, no root), and the VPS exit terminates WireGuard and
egresses to the internet. Locally the client exposes a SOCKS5 proxy.

```
  machine inside the whitelist                               open internet
┌──────────────────────────────┐                          ┌───────────────┐
│ browser / app                │                          │  any website  │
│   │ SOCKS5  127.0.0.1:1080   │                          └──────▲────────┘
│   ▼                          │                                 │ TCP/UDP
│ cmd/client                   │        VK TURN relay     ┌──────┴────────┐
│   WireGuard (userspace)      │      (whitelisted IP)    │ cmd/server    │
│   → DTLS ────────────────────┼──► ChannelData ──► UDP ─►│  (your VPS)   │
│   over a pion/turn alloc     │   (looks like media)     │ DTLS→WireGuard│
└──────────────────────────────┘                          │ → dial target │
                                                          └───────────────┘
```

### Server exit — pluggable (`-exit`)

A client's identity is its WireGuard public key, decoupled from the transport, so
**the exit can be swapped without touching the client or the protocol** (the
carrier is identical):

```
          DTLS (mutual PSK)  ─►  client identity = WG pubkey
                                        │
                 ┌──────────────────────┴──────────────────────┐
      -exit userspace (default)                      -exit kernel (variant B)
   ┌───────────────────────────────┐          ┌───────────────────────────────┐
   │ internal/wggw                 │          │ internal/kernelexit           │
   │  gVisor netstack (userspace)  │          │  relay WG datagrams to kernel  │
   │  → net.Dial (explicit filter) │          │  → wg0 (kernel WireGuard)      │
   │                               │          │  → nftables: MASQUERADE        │
   │  non-root · SSRF-safe         │          │    + egress filter (anti-SSRF) │
   │  ~2–5 MB/client               │          │  peers: reconcile on SIGHUP    │
   │                               │          │  ~0.2 MB/client · needs root   │
   └───────────────┬───────────────┘          └───────────────┬───────────────┘
                   └───────────►  open internet  ◄─────────────┘
```

Variant B and its mandatory egress filter are documented in `docs/kernel-exit.md`.

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
      └ WireGuard         — Noise / ChaCha20-Poly1305: a second cipher + routing
          └ IP packets    — the app's TCP/UDP (web is itself TLS/HTTPS,
                            end-to-end to the real site)
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
- **WireGuard inside** adds its own key-based mutual authentication and a second
  cipher (Noise + ChaCha20-Poly1305), and routes the IP packets to the exit. Raw
  WireGuard would be dropped by the relay, so it rides inside DTLS, not instead of it.

**Defense in depth.** Over the untrusted relay traffic is encrypted in layers:
outer DTLS (ECDHE-PSK) + inner WireGuard (keys), and for the web also HTTPS — a
third authenticated TLS session end-to-end to the real site that neither the relay
nor even your own VPS can decrypt. Only your own VPS ever sees the destination.

**The exit is not an open relay (SSRF guard).** The client dictates the target, but
the exit rejects loopback, private, link-local and metadata addresses
(`169.254.169.254`) and dials the already-verified IP — so the tunnel can't be
pointed at the VPS's internal services or cloud metadata, and DNS rebinding can't
slip in between the check and the dial. On by default. See `internal/wggw`.

**What it still does *not* hide: the tunnel's existence.** Sustained high-volume
traffic through a "call", and a TURN allocation egressing to an external IP, are
detectable by traffic analysis. Footprint can be reduced (bitrate shaping via
`-shape`, exit rotation via a `-peer` list) but do not expect invisibility.

## Components

- `cmd/client` — local SOCKS5 proxy (CONNECT + UDP ASSOCIATE); over the DTLS/TURN
  carrier it runs WireGuard (`wgclient`) and dials through it. A background manager
  re-establishes the tunnel before credentials expire; comma-separated `-peer` rotates exits.
- `cmd/server` — on the VPS: terminates DTLS and runs a WireGuard exit (`wggw`) that
  dials each target on the open internet (SSRF-guarded by default) and pipes it back.
- `internal/okapi`, `internal/vkapi` — the VK/OK REST chain for `turn_server` creds.
- `internal/wgbind` — a `conn.Bind` carrying WireGuard over a carrier `net.Conn` (DTLS/TURN).
- `internal/wggw` — the server WireGuard exit, default (gVisor netstack, promiscuous + forwarders).
- `internal/kernelexit` — the alternative `-exit kernel` data plane: relay into a kernel `wg0` (+ nft MASQUERADE). Scales further but needs root; see `docs/kernel-exit.md`.
- `internal/wgclient` — the client WireGuard endpoint on netstack + `DialContext` (remote DNS).
- `internal/wgkey` — WireGuard (Curve25519) key generation/derivation, hex.
- `internal/socks` — SOCKS5 CONNECT and UDP ASSOCIATE + per-stream target header.
- `internal/authdtls` — mutual PSK DTLS authentication (ECDHE-PSK); supports a single
  `-secret` or a per-client keyring (identity→secret) with revocation.
- `internal/carrier` — brings the transport up (creds → TURN allocate → PSK-DTLS),
  shared by the desktop client and Android.
- `internal/captcha` — a user-supplied captcha fetcher that plugs into the
  client's `CredsFunc` seam. **Not part of the core.**

## Usage

Two layers of authentication: a **DTLS secret** (`-secret` / `TURNVPN_SECRET`) on
both ends authenticates the carrier, and **WireGuard keys** authenticate the tunnel.
First generate a keypair on each side:

```sh
go build -o server ./cmd/server && go build -o client ./cmd/client
./server -genkey   # → server private + public
./client -genkey   # → client private + public
```

On the VPS: the server private key + a peers file (`<client public key> <its addr>`
per line) + the DTLS secret (open the UDP port in the firewall):

```sh
echo '<client public key> 10.6.0.2/32' > peers.txt
echo '<server private key>' > wg.key
TURNVPN_SECRET='<shared secret>' ./server \
  -listen 0.0.0.0:56000 -wg-key wg.key -wg-peers peers.txt
```

On your machine (with a per-call token captured from a browser call):

```sh
TURNVPN_SECRET='<same secret>' TURN_CALL_TOKEN='<call-token>' \
TURNVPN_WG_KEY='<client private key>' TURNVPN_WG_SERVER_PUB='<server public key>' \
./client \
  -peer <VPS_IP>:56000 \
  -vk-link <token from vk.ru/call/join/...> \
  -wg-addr 10.6.0.2
```

The client opens a local **SOCKS5 proxy at `127.0.0.1:1080`**. Point the browser
(or system) at it: type **SOCKS5**, host `127.0.0.1`, port `1080`, and enable
"proxy DNS when using SOCKS v5" (remote DNS) so names resolve on the VPS, not
locally. Keep the browser call alive while the tunnel is up. Without `-token`,
the client uses the credential provider wired in `main` (e.g. a captcha fetcher)
— which you supply.

QUIC/HTTP-3 and other UDP ride the tunnel alongside TCP (SOCKS5 UDP ASSOCIATE on
the client). DNS resolves on the VPS (WireGuard carries all IP; names resolve at
the exit).

**Optional flags:**

- `-peer a:56000,b:56000` — multiple exits; the tunnel rotates across them on reconnect.
- `-shape 2mbit` (both ends) — shape the outbound bitrate toward a call-like envelope.
- Multiple clients: add a line per client to `-wg-peers` (each with its own
  `-wg-addr`). You can also layer per-client DTLS access: `-keyring file` on the
  server (`identity secret` lines) and `-identity <id>` on the client.

## Android

The native Android app **HolubVPN** (Kotlin + `VpnService`, Go core bound through a
gomobile `.aar`: WireGuard on the tun → our tunnel) lives in
[`android/`](android/README.md), with its own build & run notes.

## Status

Vector 1 (TURN relay + DTLS + **WireGuard**) is **working end-to-end**, Android
included: real HTTPS browsing and QUIC/HTTP-3 exit via the VPS over whitelisted VK
infrastructure, with proactive credential refresh. The data plane is **WireGuard** on
both desktop (`cmd/client`/`cmd/server`) and the Android core (`wireguard-go` on the
VpnService tun); the old KCP/smux/SOCKS/tun2socks path is gone. Proven live: the
**HolubVPN** app, through a real VK TURN relay, egresses via the VPS (`api.ipify.org`
in the emulator browser returned the VPS IP). Hardening is in place: mutual DTLS
authentication (a single secret or a per-client keyring), an SSRF guard on the exit
(on by default), a non-root systemd service on the VPS, and the secret plus the WG
private key in the Android Keystore. See `PLAN.md` and `LAB.md` for the full history.
