# turnvpn — план

Цель: восстановить доступ в открытый интернет для одного пользователя в режиме
«белого списка» (суверенный режим), прокатив свой трафик поверх **whitelisted**
инфраструктуры VK/OK так, чтобы для сети это выглядело легитимным звонком.

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
| **Vector 1 — TURN-реле** | Гонять трафик через whitelisted TURN VK на свой VPS | 🔶 **ВЕРОЯТНО ЖИВОЙ (пересматриваем).** Экосистема `cacggghp` доказывает, что реле форвардит на произвольный внешний пир, **если payload завёрнут в DTLS**. Наш старый вывод «мёртв» — false negative (гоняли сырой UDP). Гейт Stage 0 = перепроверить DTLS-обёрткой. |
| **Variant A — data channel** | Байты в WebRTC DataChannel | ❌ **МЁРТВ**. `animoji`-DC эфемерный, серверные 15-байтовые кадры, инъекции не доходят. |
| **Vector 2 — media RTP** | Байты внутри видео-RTP, pion-участник звонка | ✅ жив, но **ДОРОГОЙ** (нужен pion-webrtc + реверс ws2 + framing-кодек). **Fallback**, если Vector 1 отвалится. Join-стек (`vkcall`/`vchat`/`wt*`) удалён при развороте — восстановим из git-истории. |

---

## Как это работает (Vector 1, по референсу)

```
                 ┌─ комп (клиент, в шатдауне достаёт только ВК) ─┐
  WireGuard  →   │  client: wrap в DTLS → pion/turn ChannelData  │
  (localhost)    └───────────────┬──────────────────────────────┘
                                  │  (whitelisted: только TURN VK)
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
      Реальный браузинг через whitelisted TURN ВК работает **без WireGuard, без root**.

### Stage 4 — (опц.) WireGuard поверх туннеля
- [ ] Если нужен **весь** трафик машины (не только прокси-приложения): WG-клиент →
      локальный UDP клиента → туннель → WG-сервер на VPS → MASQUERADE. SOCKS-варианта
      хватает для веба; WG — апгрейд при желании.

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

### Stage 7 — UDP  ⬅️ **СЕЙЧАС ЗДЕСЬ**
- [ ] SOCKS5 UDP ASSOCIATE на клиенте + проброс датаграмм в туннель; на сервере —
      UDP-ассоциации per-поток. Нужно для QUIC/HTTP3, DNS-over-UDP, игр.

### Stage 8 — (опц.) вся машина через WireGuard
- [ ] Для **всего** трафика машины (не только прокси-аппов): WG-клиент → локальный
      UDP клиента → туннель → WG-сервер на VPS → MASQUERADE. SOCKS хватает для веба.

### Stage 9 — footprint + живучесть звонка
- [ ] Шейп битрейта под правдоподобный «конверт» звонка; ротация эндпоинтов/звонков.
- [ ] Хрупкость: всё держится на живом браузерном звонке + call-token; хост завершил
      звонок — туннель упал. Нужна ротация/переоткрытие.
- [ ] Реальность: устойчиво тяжёлый трафик через звонок аномален; снижаем footprint,
      но не рассчитываем на невидимость. **Отказались:** массовое создание аккаунтов;
      авто-обход капчи.

---

## Среда / факты

- **VPS** `<VPS>` (root, SSH по ключу с Mac). `vpsecho` на `:51822`,
  ufw `51822/udp` открыт. (Старый raw-UDP эхо; для Stage 0 нужен DTLS-эхо.)
- **Референс** `github.com/cacggghp/vk-turn-proxy` — рабочая реализация Vector 1
  (TURN+DTLS+WireGuard). Берём архитектуру/детали протокола; **авто-решатель
  капчи оттуда не переносим**.
- **Капча:** только `getAnonymousToken`, ~10-мин окно, человек-в-цикле.
- **VK web instrumentation** — см. memory-ноту `turnvpn-feasibility`.

---

## Репо

- `cmd/client` — клиент (Mac): достаёт `turn_server` из звонка → TURN Allocate +
  DTLS сквозь реле. Stage 0 = проба эха; дальше обрастает SOCKS5 + KCP/smux.
- `cmd/server` — выход (VPS): Stage 0 = DTLS-эхо; дальше DTLS → mux → dial в интернет.
- `internal/okapi`, `internal/vkapi` — VK/OK REST-цепочка до `turn_server`.
- `internal/graceful`, `internal/dotenv` — инфра (signal ctx, .env).
- `internal/captcha` — решатель капчи (**WIP пользователя**, не часть моего кода;
  подключается как `CaptchaSolver` через интерфейс).
- `.env`, `.proxyai/`, `cap.json`, бинарники `client`/`server` — в `.gitignore`.

> Vector 2 (SFU-join стек `vkcall/vchat/wt*`, `cmd/vkjoin`) и старые пробы
> (`cmd/probe`, `cmd/vpsecho`, `tools/*`) **удалены** при развороте на Vector 1 —
> восстановимы из git-истории, если fallback-путь когда-нибудь понадобится.
