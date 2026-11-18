---
name: security-audit
description: Run a defensive security review of HolubVPN/turnvpn across three surfaces — the Android app (android/ + mobile/), the VPS server (cmd/server), and the tunnel between them (TURN → DTLS → KCP/smux, incl. the UDP relay). Use when asked to check security / audit / review for vulnerabilities / "безопасно ли", or before publishing the repo, shipping an APK, redeploying the server, or running on a real device.
---

# security-audit — проверка безопасности HolubVPN / turnvpn

Оборонительный аудит **своего** проекта. Три поверхности: **приложение**, **сервер**,
**соединение**. Цель — найти утечки секретов, open-proxy/SSRF, и слабости транспорта
до публикации или реального запуска. Находки — фактические, с `файл:строка` и
конкретным фиксом.

## Когда запускать

- По запросу «проверь безопасность / аудит / уязвимости / ничего не светим?».
- Перед тем как сделать репозиторий публичным, выложить APK, редеплоить `cmd/server`
  или запустить на реальном телефоне.
- После изменений в транспорте, аутентификации, обработке кредов/токенов, в WebView
  капчи или в UDP-реле.

## Поверхность 1 — Приложение (`android/`, `mobile/`)

- **Секреты в хранилище.** Shared secret лежит в `SharedPreferences("turnvpn")` в
  открытом виде → читается на рутованном устройстве и **попадает в бэкап**. Проверить
  `android:allowBackup` в манифесте (должно быть `false`, либо prefs исключены
  `fullBackupContent`/`dataExtractionRules`).
- **Утечки в логах.** Секрет, per-call токен, креды TURN не должны уходить в `Log.*`,
  в crash-репорты и аналитику. `grep -rn 'Log\.' android/app/src` и проверить, что
  рядом нет `secret`/`token`/`credential`. Go-сторона: `cfg.Logf`, `GoLog` — не печатать
  креды.
- **WebView капчи (`CaptchaActivity`).** Грузится только `vk.ru/call/join/...` (не
  произвольный URL из intent без валидации). `HOOK_JS` инжектится в доверенную
  страницу; `addJavascriptInterface` отдаёт наружу **только** `onToken(...)` и ничего
  шире. Токен не логируется. `shouldOverrideUrlLoading`/редиректы не уводят на левые
  origin с активным мостом.
- **VpnService.** tun-fd отдаётся в Go как независимый `dup()` (иначе `fdsan SIGABRT`);
  нет двойного `close`. `addDisallowedApplication(packageName)` — трафик самого
  приложения (контроль/креды) не заворачивается в туннель. Маршруты/DNS осознанны.
- **Экспортируемые компоненты.** В манифесте `MainActivity` — launcher (ок);
  `CaptchaActivity` и `TurnvpnService` — `exported="false"`. Service под
  `BIND_VPN_SERVICE`. Permissions минимальны (INTERNET/FOREGROUND_SERVICE/
  POST_NOTIFICATIONS).
- **Хардкод.** Нет реальных секретов/IP VPS/личных токенов в коде, ресурсах, строках.
  `DEFAULT_APPKEY` — публичный web-ключ OK (не секрет) — подтвердить это.

## Поверхность 2 — Сервер (`cmd/server` на VPS)

- **Open-proxy / аутентификация (критично).** Выход НЕ должен быть открытым прокси:
  сервер принимает только взаимный **ECDHE-PSK** DTLS (`internal/authdtls`), без
  секрета хендшейк не проходит. Проверить, что нет пути приёма без PSK, нет fallback
  на обычный/анонимный cipher, сервер **не** ставит `WithCertificates` (pure-PSK).
- **SSRF / фильтрация цели (критично).** Клиент диктует `host:port`, сервер делает
  `net.Dial`. Может ли клиент заставить сервер подключиться к `127.0.0.1`,
  `169.254.169.254` (cloud metadata), `10/172.16/192.168/*`, `::1`, доменам,
  резолвящимся в приватные адреса? Сейчас фильтра нет — это главный риск. Фикс:
  blocklist приватных/loopback/link-local диапазонов и для TCP (`handle`), и для UDP
  (`handleUDP`) в `internal/tunnel`.
- **DoS / ресурсы.** Есть ли лимит одновременных потоков/аллокаций на клиента;
  UDP-ассоциации с idle-timeout (`udpIdle`) и границей датаграммы (`maxDatagram`);
  таймаут диала (`DialTimeout`); нет безлимитного роста горутин/буферов.
- **Листенер и привилегии.** Открыт только нужный UDP-порт (фаервол); сервис не под
  `root` без нужды; TURN-креды и секрет только в памяти; логи без секретов.

## Поверхность 3 — Соединение (TURN → DTLS → KCP → smux)

- **DTLS.** Cipher `ECDHE-PSK-WITH-CHACHA20-POLY1305` — AEAD (конфиденциальность +
  целостность), ECDHE → **forward secrecy**, PSK → взаимная аутентификация и анти-MITM.
  Не допускать слабых/анонимных cipher. PSK выводится как
  `sha256("turnvpn-psk-v1:"+secret)` — значит **энтропия секрета** решает всё: секрет
  должен быть длинным случайным, не словарным.
- **Нет plaintext до DTLS.** KCP специально без крипты (`NoneBlockCrypt`) — это ок,
  т.к. всё уже внутри DTLS; проверить, что ни один байт data-plane не уходит мимо DTLS.
- **UDP-реле (Stage 7).** Framing `uint16` длины — есть bounds-check; сервер шлёт в
  поток **только** то, что реально пришло от цели (нет усиления/reflection на третьих);
  idle-cleanup ассоциаций; цель UDP подпадает под тот же SSRF-фильтр, что и TCP.
- **Распространение секрета.** Общий секрет передаётся out-of-band, **не** коммитится
  в гит. Проверить, что в коде/истории нет секрета и IP VPS (`git log -p | grep`).
- **Метаданные.** Сам факт туннеля не скрывается (трафик-анализ видит «тяжёлый звонок»
  + egress на внешний IP) — это принятое ограничение, не дыра; но личность
  (имя/аккаунт хоста звонка, IP VPS) в репо/логи/коммиты попадать не должна.

## Как провести

1. Прочитать реальные файлы по трём чеклистам:
   `android/app/src/main/AndroidManifest.xml`, `android/app/src/main/java/com/turnvpn/app/*.kt`,
   `mobile/*.go`, `cmd/server/main.go`,
   `internal/{authdtls,tunnel,socks,mux,vpntun}/*.go`.
2. Прогнать grep на утечки: секреты/токены в логах, хардкод секрета/IP/токена,
   `Log.*`/`fmt.Print*`/`GoLog` рядом с `secret|token|cred`.
3. Для каждого пункта — вердикт **✅ ок / ⚠️ риск / ❌ дыра** с `файл:строка` и
   конкретным фиксом.
4. Приоритет находок (сверху — страшнее):
   **open-proxy → SSRF (metadata/localhost/приватные сети) → утечка секрета/токена →
   секрет в бэкапе prefs → DoS → прочее.**
5. Итог — короткий отчёт: критичные / средние / низкие + «что починить до
   публикации / реального запуска».

## Правила

- Это **оборонительный** аудит своего проекта. НЕ добавлять обход капчи/бот-защиты и
  НЕ ослаблять аутентификацию ради «удобства».
- Реальные секреты/токены/личные данные в отчёт не выводить — маскировать.
- Находки только фактические, с `файл:строка`; не выдумывать и не раздувать severity.
