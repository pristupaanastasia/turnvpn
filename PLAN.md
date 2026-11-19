# turnvpn — план

Цель: восстановить доступ в открытый интернет для одного пользователя в сети, где
наружу пускают только адреса из белого списка, прокатив свой трафик поверх
инфраструктуры VK/OK из этого белого списка так, чтобы для сети это выглядело
легитимным звонком.

Дисциплина: лесенка «снизу вверх». Каждая ступень — максимально дешёвая проверка,
которая **гейтит** следующую. Поднимаемся только пока нижняя ступень зелёная.

> **РАЗВОРОТ 2026-11-18.** Изучён рабочий референс `cacggghp/vk-turn-proxy` (целая
> экосистема клиентов). Он гоняет WireGuard **через TURN-реле VK** — т.е. по
> **Vector 1**, который мы раньше объявили мёртвым. Наш вывод был **false
> negative**: в M0 мы гнали через реле **сырой UDP**, а реле, судя по всему,
> на data plane пропускает только то, что выглядит как **DTLS/медиа**. Референс
> оборачивает payload в DTLS 1.2 — и всё ходит. Переключаемся на Vector 1: он
> радикально короче SFU-пути (не нужен pion-webrtc, ws2, SDP-переговоры).

---

## Что уже выяснено

| Путь | Идея | Статус |
|------|------|--------|
| **Vector 1 — TURN-реле** | Гонять трафик через TURN VK (из белого списка) на свой VPS | ✅ **ЖИВОЙ, подтверждён end-to-end.** Реальный HTTPS/QUIC-браузинг выходит через VPS по инфраструктуре VK из белого списка (Stage 0–7 пройдены). Старый вывод «мёртв» был false negative — гоняли сырой UDP; реле на data plane пропускает только то, что похоже на DTLS/медиа. |
| **Variant A — data channel** | Байты в WebRTC DataChannel | ❌ **МЁРТВ**. `animoji`-DC эфемерный, серверные 15-байтовые кадры, инъекции не доходят. |
| **Vector 2 — media RTP** | Байты внутри видео-RTP, pion-участник звонка | ✅ жив, но **ДОРОГОЙ** (нужен pion-webrtc + реверс ws2 + framing-кодек). **Fallback**, если Vector 1 отвалится. Join-стек (`vkcall`/`vchat`/`wt*`) удалён при развороте — восстановим из git-истории. |

---

## Как это работает (Vector 1, по референсу)

```
                 ┌─ комп (клиент, в шатдауне достаёт только ВК) ─┐
  WireGuard  →   │  client: wrap в DTLS → pion/turn ChannelData  │
  (localhost)    └───────────────┬──────────────────────────────┘
                                  │  (белый список: только TURN VK)
                          TURN-реле VK (90.156.x.x:19302)
                                  │  UDP на твой VPS:PORT
                 ┌────────────────┴─────────────────────────────┐
                 │  server (VPS, открытый интернет):             │
                 │  приём UDP → DTLS-расшифровка → WireGuard →    │
                 │  MASQUERADE в открытый интернет               │
                 └──────────────────────────────────────────────┘
```

**Почему нужен VPS:** реле форвардит на **один** привязанный пир — это не открытый
прокси. Пир должен быть в открытом интернете (вне шатдауна) и реально выпускать
трафик (WireGuard+MASQUERADE). Комп в шатдауне сам отрезан от сети, выходом быть не
может. Клиентская крипта/обёртка крутится на компе; VPS — только выходная нода.

**Креды TURN добываются REST-цепочкой (без реального входа в звонок):**
```
get_anonym_token                         → token1 (анон-JWT, капча-free)
calls.getAnonymousToken(link,name,token1)→ token2 (call-token)   ← ЕДИНСТВЕННАЯ капча
auth.anonymLogin (session_data, version:2, БЕЗ auth_token) → session_key
vchat.joinConversationByLink(link, anonymToken=token2, session_key)
      → turn_server{username, credential, urls}   ← КРЕДЫ TURN (живут ~10 мин)
```

**Капча** — только на шаге `getAnonymousToken`, раз в ~10 мин. Решаем её
**человеком-в-цикле** (пользователь проходит в браузере / подставляет `-token`).
Авто-решатель анти-бота (PoW + подделка курсора/сенсоров/отпечатка под
`captchaNotRobot`) **НЕ реализуем** — это обход бот-детекта, вне рамок проекта.

---

## Vector 1 — стадии

> Каждая стадия гейтит следующую.

### Stage 0 — make-or-break: DTLS-реле форвардит на VPS  ✅ **ПРОЙДЕН (2026-11-18)**
Перепроверили M0 правильно: `pion/turn` Allocate с живыми TURN-кредами → `pion/dtls`
handshake сквозь relay на VPS, DTLS-эхо назад.
- [x] Клиент (`cmd/client`): получить `turn_server` из звонка (капчу прошёл человек).
- [x] Клиент: `pion/turn` Allocate → `pion/dtls` handshake до VPS через relay.
- [x] Сервер на VPS (`cmd/server`): `pion/dtls` server, принять, расшифровать, эхо.
- [x] **Критерий ВЫПОЛНЕН:** маркер прошёл Mac→реле→VPS→обратно; VPS принял DTLS от
      адреса аллокации реле. **Vector 1 жив, M0-негатив опровергнут** (см. `LAB.md`).

### Stage 1 — клиент добычи кредов  ✅ **ПРОЙДЕН**
- [x] `get_anonym_token` → анон-JWT (`vkapi.GetAnonymToken`, капча-free).
- [x] `calls.getAnonymousToken` → call-token (`vkapi.GetCallToken`; капча = человек).
- [x] `auth.anonymLogin` рабочая форма (version:2, `client_type:SDK_JS`, **без auth_token**)
      → session_key (`okapi.AnonymLogin` исправлен).
- [x] `vchat.joinConversationByLink` → дескриптор c `turn_server` (`cmd/client.fetchTurnCreds`).
- [~] Перезапрос кредов при обрыве туннеля (есть; авто-рефреш *до* протухания — Stage 5).

### Stage 2 — транспорт поверх DTLS-туннеля  ✅ **ПРОЙДЕН**
- [x] `internal/mux`: KCP+smux поверх DTLS (свой адаптер `net.Conn`→`net.PacketConn`).
- [x] `cmd/client`: `pion/turn` Allocate → `pion/dtls` сквозь relay → `mux.Client`;
      ленивое переподключение при мёртвой сессии.
- [x] Мультиплексирование: много потоков в одном DTLS/TURN-туннеле.

### Stage 3 — SOCKS5 end-to-end  ✅ **ПРОЙДЕН (вместо WireGuard)**
- [x] `internal/socks`: минимальный SOCKS5 CONNECT + per-stream target-заголовок.
- [x] `cmd/client`: локальный SOCKS5 на `127.0.0.1:1080` → поток в туннель.
- [x] `cmd/server` (VPS): DTLS → mux → читает `host:port` → `net.Dial` → pipe.
- [x] **Проверено:** `curl --socks5-hostname …` → exit-IP = VPS; `example.com` → `HTTP/2 200`.
      Реальный браузинг через TURN ВК (из белого списка) работает **без WireGuard, без root**.

### Stage 4 — WireGuard как data plane  ✅ **ПРОЙДЕН (локально, 2026-11-19)** ⬅️ **СЕЙЧАС ЗДЕСЬ**
Разворот: `KCP/smux/SOCKS` поверх DTLS заменены на **WireGuard** (внутри того же
DTLS-over-TURN carrier — сырой WG реле дропнет). Всё в userspace на gvisor-netstack,
**без root/TUN**.
- [x] `internal/wgbind` — `conn.Bind`, возящий WG поверх carrier-`net.Conn`.
- [x] `internal/wggw` — серверный WG-выход: netstack(promiscuous) + TCP/UDP-форвардеры
      → `net.Dial` в интернет; SSRF-guard по умолчанию; non-root.
- [x] `internal/wgclient` — клиентский WG на netstack + `DialContext` с remote-DNS.
- [x] `cmd/server`/`cmd/client` переписаны на WG (DTLS-PSK carrier/auth сохранён);
      ключи — `-genkey`/`-wg-key`/`-wg-peers`/`-wg-server-pub`.
- [x] Доказано локально: `TestClientDialThroughGateway`, `TestGatewayOverRealDTLS`
      (поверх настоящего pion/dtls), `-race` чисто.
- [ ] Редеплой VPS (заменит mux-сервер) + проверка через реальный TURN-реле.
- [ ] Удалить мёртвые `internal/mux`/`internal/tunnel` после миграции Android.

### Stage 5 — устойчивость  ✅ **ПРОЙДЕН**
- [x] Пул параллельных туннелей для throughput (`-tunnels N`, round-robin).
- [x] Авто-рефреш кредов **до** протухания: фоновый менеджер на туннель (`run()`),
      рефреш за ~8 мин (креды живут ~10), старая сессия гаснет с грейсом 90с
      (in-flight потоки доживают); smux-keepalive детектит смерть сессии.

### Stage 6 — безопасность (auth + анти-MITM)  ✅ **ПРОЙДЕН**
- [x] Взаимная аутентификация DTLS по общему секрету (`internal/authdtls`,
      cipher `ECDHE-PSK-CHACHA20-POLY1305`): без секрета чужой ни хендшейк не
      завершит, ни MITM не сделает. Закрывает **открытый прокси** на сервере и
      тревогу про данные внутри DTLS. `-secret`/`TURNVPN_SECRET` обязателен на
      обеих сторонах. Forward secrecy сохранён (ECDHE). Тест: `internal/authdtls`.

### Stage 7 — UDP  ✅ **ПРОЙДЕН**
- [x] Сервер: UDP-ассоциации per-flow через **ненадёжную дорожку** (P1.1):
      control-поток (`udp/`+target+flow-id) + lane-датаграммы по flow-id, idle-timeout.
      QUIC идёт без head-of-line blocking (`internal/mux/lane.go`, `tunnel.handleUDP`).
- [x] Редеплой `cmd/server` на VPS (non-root) + проверка: на эмуляторе
      `cloudflare-quic.com` → **HTTP/3** через туннель.
- [x] Десктопный `cmd/client`: SOCKS5 **UDP ASSOCIATE** (`serveUDPAssociate`,
      `internal/tunnel/udpflow.go`) — UDP теперь не только через mobile/tun2socks.
      Покрыто `TestUDPRelay`, `TestLaneDatagramAndStream`.

### Stage 8 — (опц.) whole-device через реальный TUN
- [ ] Сейчас WG-клиент крутится на userspace-netstack + локальный SOCKS5 (без root).
      Для **всего** трафика машины (не только прокси-аппов) — поднять WG на реальном
      TUN-устройстве (нужен root/caps). netstack+SOCKS хватает для веба.

### Stage 9 — footprint + живучесть звонка  🔶 **ЧАСТИЧНО**
- [x] Шейп битрейта (P2.3): token-bucket пейсит эмиссию DTLS под кап (`-shape 2mbit`,
      `internal/mux/shaper.go`). Ротация эндпоинтов (P2.2): `cmd/client -peer` —
      список через запятую, туннели round-robin по выходам.
- [x] Per-client auth + отзыв (P0.3): серверный keyring (`-keyring`, identity→secret),
      клиентская `-identity`; отзыв = убрать строку и перезапустить.
- [ ] Хрупкость: всё держится на живом браузерном звонке + call-token; хост завершил
      звонок — туннель упал. Нужен headless-держатель звонка / авто-переоткрытие (P2.4).
- [ ] Снижение частоты капчи (P2.1) — research по VK API (окно ~10 мин).
- [ ] (research) бенч/тюнинг KCP под потерями (P1.2/1.3).
- [ ] Реальность: устойчиво тяжёлый трафик через звонок аномален; footprint снижаем
      (шейп+ротация есть), но на невидимость не рассчитываем. **Отказались:** массовое
      создание аккаунтов; авто-обход капчи.

### Stage 10 — Android-клиент
- [ ] **Существующие приложения** есть в экосистеме `cacggghp/vk-turn-proxy`:
      `samosvalishe/turn-proxy-android` (Kotlin, Material 3, автоапдейты),
      `kiper292/wireguard-turn-android` (встроен в WireGuard), `oxsidee/vkpn`
      (Flutter, кроссплатформа), `WINGS-N/WINGSV`, `MYSOREZ/vk-turn-proxy-android`,
      `amurcanov/proxy-turn-vk-android`. **НО** они под не-PSK протокол того проекта —
      к нашему PSK-серверу как есть **не подключатся** (нужна совместимость по
      auth/wire-формату, либо адаптер).
- [x] **Дешёвый путь (свой) — ГОТОВ:** `scripts/build.sh android` собирает статический
      Go-клиент под `linux/arm64` (+ `arm`), запускается в **Termux** как на компе
      (SOCKS5 на `127.0.0.1:1080`). Капчу решает человек в браузере телефона. Доки — в
      `README.md`. Осталось: удобный SOCKS5→tun маршрут на телефоне (tun2socks).
- [x] **Полный путь — СОБРАН (APK):** `mobile/` (gomobile-биндинг) = `vpntun` (туннель
      как Dialer) + `xjasonlyu/tun2socks`; `./scripts/build.sh aar` → `.aar`. Приложение
      `android/` (Kotlin + `VpnService`) собирается в APK (`./gradlew assembleDebug`).
      DNS через туннель по TCP автоматически; UDP/QUIC — через ненадёжную дорожку (Stage 7).
      Оговорки: arm64-only.
- [x] UDP/QUIC добавлен (Stage 7) и проверен на эмуляторе (`cloudflare-quic.com` → HTTP/3).
- [ ] Обкатать APK на реальном устройстве (пока проверено на эмуляторе).

---

## Среда / факты

- **VPS** `<VPS>` (SSH по ключу с Mac). `cmd/server` крутится **не под root**
  (пользователь `turnvpn`) как persistent hardened systemd-юнит
  (`/etc/systemd/system/turnvpn-server.service`: NoNewPrivileges, ProtectSystem=strict,
  dropped caps), слушает DTLS на UDP `:56000`, секрет/keyring — в `/etc/turnvpn-server.env`
  (600). Порт открыт в фаерволе.
- **Референс** `github.com/cacggghp/vk-turn-proxy` — рабочая реализация Vector 1
  (TURN+DTLS+WireGuard). Берём архитектуру/детали протокола; **авто-решатель
  капчи оттуда не переносим**.
- **Капча:** только `getAnonymousToken`, ~10-мин окно, человек-в-цикле.
- **VK web instrumentation** — см. memory-ноту `turnvpn-feasibility`.

---

## Репо

- `cmd/client` — клиент (Mac): `turn_server` из звонка → TURN Allocate + DTLS сквозь
  реле → **WireGuard** (`wgclient`) → локальный SOCKS5 (CONNECT + UDP ASSOCIATE).
- `cmd/server` — выход (VPS): DTLS → **WireGuard-gateway** (`wggw`) → dial в интернет.
- `internal/okapi`, `internal/vkapi` — VK/OK REST-цепочка до `turn_server`.
- `internal/wgbind` — `conn.Bind`: WireGuard поверх carrier-`net.Conn` (DTLS/TURN).
- `internal/wggw` — серверный WG-выход (netstack promiscuous + форвардеры + SSRF-guard).
- `internal/wgclient` — клиентский WG на netstack + `DialContext` (remote-DNS).
- `internal/wgkey` — генерация/деривация WG-ключей (Curve25519, hex).
- `internal/socks` — SOCKS5 CONNECT + UDP ASSOCIATE, per-stream target-заголовок.
- `internal/authdtls` — взаимная PSK-аутентификация DTLS (ECDHE-PSK) + keyring.
- `internal/mux`, `internal/tunnel` — **legacy** KCP/smux/lane data plane; остались
  только под mobile (`internal/vpntun`), удалятся после миграции Android на WG.
- `internal/vpntun`, `mobile/`, `android/` — старый `Dialer` + gomobile `.aar` +
  Kotlin **HolubVPN** (ещё на старом протоколе).
- `internal/graceful`, `internal/dotenv` — инфра (signal ctx, .env).
- `internal/captcha` — решатель капчи (**WIP пользователя**, не часть моего кода;
  подключается как `CaptchaSolver` через интерфейс).
- `.env`, `.proxyai/`, `cap.json`, бинарники `client`/`server` — в `.gitignore`.

> Vector 2 (SFU-join стек `vkcall/vchat/wt*`, `cmd/vkjoin`) и старые пробы
> (`cmd/probe`, `cmd/vpsecho`, `tools/*`) **удалены** при развороте на Vector 1 —
> восстановимы из git-истории, если fallback-путь когда-нибудь понадобится.
