# 12. Реальные подписки: форматы, параметры, классификация против матрицы D26

Статус на 2026-10-17: **готово на реальных данных** (с оговорками §0). Владелец передал содержимое двух реальных подписок (секреты остались только в scratchpad сессии, в репозиторий не попадали). Сделано:

- инвентарь строк и параметров (§4);
- прогон текущего TS (`scd generate`, фильтры scd, фильтры sfc) (§5, §6);
- прогон запланированного Go-конвейера (1.7 → 4.2–4.6) и живого Xray 26.10.10 (`conf.Build` → `AddOutbound` → `ListOutbounds` → `proto.Equal` + адресная проверка каждого параметра) (§5);
- сверка фильтров меток D29 с TS-регулярками (§6);
- разбор предложений из HANDOFF на данных (§8) и список правок плана (§9).

Обезличенные фикстуры: `docker/fixtures/real-shapes/remnawave.txt` (4 строки), `provider2.txt` (50 строк), `provider2.b64` (base64 от `provider2.txt`). Они дают **те же** результаты, что и сырые данные (§4.3).

Секреты (URL подписок, UUID, хосты и базовые домены, ключи, xhttp-путь) в этот документ, фикстуры и отчёты не попадают. Проверка: токен-скрипт (§11) и leak guard координатора — `LEAK-GUARD: clean`.

## 0. Источники и оговорки

| | A — Remnawave | B — провайдер 2 |
|---|---|---|
| Как получено | владелец открыл ссылку в браузере; Remnawave отдал **HTML/JSON-страницу** подписки (`{isFound,user,links[],ssConfLinks,subscriptionUrl}`), A = `links[]` как есть | выход `en-server` боевого **TS-sfc** владельца (сырой base64-ответ + декодированный текст) |
| Что это значит | ответ в клиентском формате (base64 по UA `scd/…`) **не снят**. Ветка `BROWSER` в Remnawave возвращает страницу **до** проверки HWID и до выбора шаблона (`subscription.service.ts:89-103`). Поэтому вопрос HWID (§8b) по этим данным не решается. Ссылки в `links[]` строит тот же генератор, поэтому форма строк та же, что в base64-ответе | строки **уже прошли** белый список TS-sfc (`type,path,security,alpn,encryption,fp,sni,flow,host,allowinsecure,headertype,pbk,sid`). Строки с `mode`/`extra`/`pqv`/`spx`… из апстрима sfc тут физически не могли оказаться. B недопредставляет апстрим |
| Снимков | 1 | 1 |

Два последовательных скачивания (для оценки рандомизации хостов) сделать нельзя: в песочнице есть по одному снимку, к реальным серверам и хостам подписок не подключались.

## 1. Remnawave: какой формат отдаётся при каком UA (по исходникам)

Источник: `remnawave/backend` @ `010b365` (2026-10-06). Репозиторий `remnawave/panel` содержит только сайт документации. Генераторы подписок лежат в backend. Маршрут — `GET /api/sub/<shortUuid>[/<clientType>]`. `clientType` ∈ `stash|singbox|mihomo|json|v2ray-json|clash` (`libs/contract/constants/subscription-template/template-type/request-template-type.constant.ts:1-8`). Отдельного типа «base64» нет: base64 отдаётся **только** правилом-fallback.

Формат выбирают «Subscription Response Rules» (SRR). Админ панели может их менять, ниже — правила по умолчанию (`prisma/seed/default/response-rules.ts:8-95`), они проверяются по порядку:

| # | Условие | Ответ | `content-type` (`constants/config-types.ts`) |
|---|---|---|---|
| 1 | `Accept` содержит `text/html` | HTML-страница подписки | `text/html` |
| 2 | UA ~ `^(?:flclash\|rabbit\|flowvy\|murge\|mihomo\|prizrak-box\|koala-clash\|clash(?:-verge\|-nyanpasu\|x meta\|[-.]?meta))` (i) | Mihomo YAML | `text/yaml` |
| 3 | UA ~ `^stash` (i) | Stash YAML | `text/yaml` |
| 4 | UA ~ `^sfa\|sfi\|sfm\|sft\|karing\|singbox` (i) | sing-box JSON | `application/json` |
| 5 | UA ~ `^clash` (i) | Clash YAML | `text/yaml` |
| 6 | всё остальное | **XRAY_BASE64**: `base64(lines.join("\n"))` | `text/plain` |

Если ни одно правило не совпало (например, админ убрал fallback), сервер отвечает 403 (`response-rules.middleware.ts:71-77`). Правило может также вернуть `BLOCK`, 404, 451 или оборвать сокет (`:134-155`).

Ожидаемое распределение наших UA при правилах по умолчанию:

| UA | Формат | Примечание |
|---|---|---|
| curl по умолчанию, `node`, `scd/1.0`, `v2rayN/7.0`, `v2rayNG/1.9` | base64 share-links | `v2rayN`/`v2rayNG` переходят на Xray JSON только при включённом `serveJsonAtBaseSubscription` **и** версии вида `x.y.z` (`constants/extended-clients.ts:14-24`, `subscription.service.ts:249-257`). У `7.0` и `1.9` только две части версии, поэтому остаётся base64 |
| `Happ/1.0` | base64, но **extended client** | к строкам добавляется `?serverDescription=<base64>` **после фрагмента** (`xray.generator.service.ts:73-74`, `extended-clients.ts:1-12`); при `serveJsonAtBaseSubscription` — Xray JSON |
| `Streisand` | base64 (или Xray JSON при `serveJsonAtBaseSubscription`) | |
| `clash-verge/2.0`, `clash.meta` | Mihomo YAML | |
| `sing-box/1.11` | **base64** | регэксп правила 4 ждёт `singbox` без дефиса |

Заголовки ответа (`subscription.service.ts:614-648`) всегда содержат `content-disposition: attachment; filename=<username>` и `subscription-userinfo: upload=…; download=…; total=…; expire=…`. Опционально приходят `subscription-refill-date`, `x-hwid-*` и `customResponseHeaders` из настроек. По умолчанию это `profile-title: base64:…` (шаблон `rwEncodeBase64:`), `profile-update-interval: 12`, `support-url`, `profile-web-page-url` (`prisma/seed/seeders/8_seed-subscription-settings.ts:84-89`).

**HWID-лимит.** Если в панели включён `hwidSettings.enabled` и у пользователя `hwidDeviceLimit ≠ 0`, то запрос без заголовка `x-hwid` (наш случай) получает `hwidNotSupported` (`subscription.service.ts:765-770`). Ответ тогда — пустое тело или «custom remarks»: строки-заглушки (`createFallbackHosts`, см. §2) и `x-hwid-not-supported: true`. Тот же механизм заглушек срабатывает у пользователей в статусе expired, disabled и limited, а также при пустом списке хостов (`resolve-proxy-config.service.ts:147-184`). Заголовки HWID: `x-hwid`, `x-device-os`, `x-ver-os`, `x-device-model` (`common/utils/extract-hwid-headers/extract-hwid-headers.util.ts:16-27`).

## 2. Remnawave: как строится vless-ссылка

`src/modules/subscription-template/generators/xray.generator.service.ts`:

- Сериализация (`:476-516`): `vless://${encodeURIComponent(id)}@${addr}:${port}?${query}#${encodeURIComponent(remark)}`. IPv6 берётся в `[]` (`:501-505`). Ключ и значение кодируются через `encodeURIComponent`, поэтому `/`, `,`, `:`, `{`, `"` в значениях всегда percent-encoded. Пары со значением `undefined`/`null` пропускаются, а пустые строки остаются (`sni=`).
- **Порядок параметров фиксирован**: `encryption`, `flow`?, `type`, параметры транспорта, `security`, параметры security, `fm`? (`:101-130`).
  - транспорт: tcp — `headerType`?, при `http` ещё `path` (join `,`) и `host` (join `,`) (`:255-268`); ws — `path`?, `host`?, **`heartbeatPeriod`?** (расширение Remnawave, `:271-285`); xhttp — `path`?, `host`?, `mode` (всегда, по умолчанию `auto` — `resolve-proxy-config.service.ts:237-250`), `extra`? = `JSON.stringify(host.xhttpExtraParams ⊕ inbound.xhttpSettings.extra)` (`:317-334`); grpc — `serviceName`?, `mode=gun|multi`, `authority`?; httpupgrade — `path`, `host`; kcp — `mtu`, `tti`.
  - tls (`:355-390`): `sni` (если не `null`, может быть пустым), `fp`, `alpn`, `pcs`, `vcn`, **`cs`** (cipherSuites, ссылка на #716 comment-17851670). **`ech` в ссылку не попадает** (только в JSON-форматы).
  - reality (`:393-426`): `sni` (может быть пустым при `keepSniBlank`), `fp` (по умолчанию `chrome`), `pbk` (base64url без паддинга — JWK `x`), `sid` (только если `shortIds[0]` непустой), `pqv` (base64url ML-DSA-65 public key из `mldsa65Seed`), `spx` (если в inbound задан `spiderX`).
- `encryption` (`common/helpers/vless-encryption/generate-encryption-from-decryption.ts:218-275`) выводится из `decryption` inbound'а: `mlkem768x25519plus.<native|xorpub|random>.<0rtt|1rtt>[.<padding>].<ключи base64url>`. `1rtt` ставится только при ticket `0s`, иначе `0rtt`. Ключи: X25519 (32 байта → 43 символа) и/или ML-KEM-768 (из seed 64 байта → публичный ключ 1184 байта → 1579 символов), в порядке из `decryption`. Без `decryption` выдаётся `encryption=none`.
- `flow` (`common/utils/flow/get-vless-flow.ts:23-46`): явный `settings.flow` inbound'а (`xtls-rprx-vision` либо пусто), иначе vision для tcp/raw + tls/reality. Vision **на xhttp/ws возможен**, если админ явно задал flow на inbound'е.
- **UUID с vlessRoute** (`common/utils/vless-route/set-vless-route-for-uuid.util.ts:3-14`): если у хоста задан `vlessRouteId`, символы 14–17 UUID (версия и 3 hex) заменяются на route id в hex. Получается «UUID» с версией `0`/`6`–`f`, например `xxxxxxxx-xxxx-0007-…`.
- Remark: дубли получают суффикс ` ^~N~^` (`resolve-proxy-config.service.ts:642-653`). Шаблоны в remark раскрываются; флаг-эмодзи и город — свободный текст админа.
- **Случайность per-request** (`resolve-proxy-config.service.ts:623-636`, `:71`): `*` в address/host/sni заменяется на случайные 10 символов `[0-9a-hjkmnp-z]`, список через `,` даёт случайный элемент, опция `randomizeHosts` перемешивает порядок строк.
- Заглушки (`resolve-proxy-config.service.ts:707-753`): `vless://00000000-0000-0000-0000-000000000000@0.0.0.0:1?encryption=none&type=tcp&security=none#<текст>`.
- Не-vless в том же ответе: `trojan://`, `ss://`, `hysteria2://…/?…` (`:132-212`).

## 3. Инвентарь параметров Remnawave против матрицы 11 и плана

| Параметр / форма | Есть в 11 §4 / плане | Xray 26.10.10 (живой прогон, §10–§11) | Go-план сегодня | Нужно |
|---|---|---|---|---|
| `encryption=none` / `mlkem768x25519plus.*` (1 или 2 ключа, padding) | да | ✓ (`xor=1`, `pad=…` дошли) | entry | — |
| `flow=xtls-rprx-vision` на tcp+reality/tls | да | ✓ | entry | — |
| `type=xhttp`, `mode=auto` (всегда), `path`, `host`, `extra` (`xPaddingBytes`, `noGRPCHeader`, `xmux{…}`) | да | ✓ | entry | — |
| `sni=` (пусто) у reality | O15 | ✓ | entry | — |
| reality без `sid` | D17 | ✓ (`shortId` = 8 нулей) | entry | — |
| `pqv` (1952 байта), `spx` | да | ✓ | entry | — |
| `fm` (finalmask JSON) | да | ✓ | entry | — |
| tcp `headerType=http` + `path`/`host` через `,` | да | ✓ | entry | — |
| **`cs`** (TLS cipherSuites) | **нет** | ✓ → `tls.Config.CipherSuites`; `conf` и `AddOutbound` **не проверяют** значение (`BOGUS_SUITE` принят) | D28: `param_ignored` | по D26 («любой параметр, который Xray поддерживает») добавить маппинг `cs` → `tlsSettings.cipherSuites` (строка как есть, как `ech`) |
| **`heartbeatPeriod`** (ws) | **нет** | ✓ → `websocket.Config.HeartbeatPeriod` (uint32) | D28: `param_ignored` | добавить маппинг `heartbeatPeriod` → `wsSettings.heartbeatPeriod` (целое; не целое → skip `unsupported_value` в normalize, иначе `json.Unmarshal` в conf упадёт) |
| UUID с vlessRoute (версия ≠ 1–5) | D18 | ✓ | entry (TS: skip) | тест-строка в корпус |
| заглушка `0.0.0.0:1`, nil-UUID, `security=none` | 11 §3 #15 | conf **✓** (0.0.0.0 — приватный) | skip `unsupported_combo` (явная проверка в normalize) | тест: проверка #15 не должна полагаться на conf |
| `?serverDescription=…` после `#` (extended-клиенты) | нет | — | попадёт в label → в city/тег | см. §10 п.3 |
| remark-суффикс ` ^~N~^` | нет | — | идёт в city-slug (`…-testcity-2-…`) | только тест (поведение как у TS) |
| `serviceName`, `mode=gun` (grpc), `mtu`/`tti` (kcp) | 11 §4.1 | — | skip combo | — |
| `ech` | да | — | Remnawave его в ссылки не пишет | — |

## 4. Инвентарь реальных строк

### 4.1 A — Remnawave (4 строки)

- 4/4 `vless://`, порт `443`, **один** UUID (v4, канонический lowercase) на все строки. 4 разных хоста одного базового домена, форма `xxx-cc-N.<домен>` (20 символов), без `*` и без 10-символьных случайных меток (§2 «случайность»). `sni` == хост во всех строках.
- **Порядок параметров** одинаков: `encryption,type,path,mode,extra,security,sni,fp,alpn`. Совпадает с порядком генератора Remnawave (§2): `encryption`, (`flow` нет), `type`, транспорт, `security`, параметры tls.
- `type=xhttp`, `security=tls`, `flow` — нет, `host` — нет, `headerType` — нет.
- `encryption=mlkem768x25519plus.native.0rtt.<ключ>`: 1610 символов, **один** ключ, 1579 символов base64url → 1184 байта (ML-KEM-768). X25519-ключа и padding-сегментов нет. Значение не percent-encoded (алфавит `[A-Za-z0-9._-]`). Ключ одинаков во всех строках.
- `path=%2F<…>`: в декодированном виде 37 символов, `/` + строка формы UUID (lowercase hex). С UUID пользователя не совпадает. Одинаков во всех строках. **Секретоподобное значение** (см. §7 п.9).
- `mode=stream-up` (а не `auto`).
- `extra`: 318 символов, полностью percent-encoded (`encodeURIComponent`), одинаков во всех строках. JSON (значения — не секрет):
  `{"xmux":{"cMaxReuseTimes":0,"maxConcurrency":"8-16","maxConnections":0,"hKeepAlivePeriod":0,"hMaxRequestTimes":"600-900","hMaxReusableSecs":"1800-3000"},"headers":{},"noGRPCHeader":false,"xPaddingBytes":"100-1000"}`.
  Диапазоны записаны строками, нули — числами, `headers` — пустой объект.
- `fp=chrome`, `alpn=h2`.
- Метки (не секрет): `🇩🇪DE-1-X`, `🇩🇪DE-2-X`, `🇳🇱NL-1-X`, `🇳🇱NL-2-X` — флаг **без пробела** перед текстом, суффикс `-X`. В ссылке метка percent-encoded (uppercase hex). Дублей нет.
- Длина каждой строки — 2174 символа.

### 4.2 B — провайдер 2 (50 строк)

- Сырой ответ: одна строка std-base64 (10608 символов, с `==`) + `\n`. Декодированный текст — 50 строк через `\n`, **без** завершающего перевода строки. `base64 -d` == текст.
- 50/50 `vless://`, порт `8443`, **один** UUID (v4) на все строки. 50 разных хостов одного базового домена, 10 форм первой метки (`aaa-aaa`×14, `aaa-aa-99`×10, `aa9`×10, `aaa-aa`×5, `aaa9`×4, `aaa-9`×3, по одной `aaa-aaa-99`, `aa-aaa`, `aa-9`, `aaa9a`), без `*`. Повторов хоста нет.
- **Все строки — одна форма**: `type=tcp&path=%2F&security=tls&alpn=h2` (порядок именно такой). Нет `sni`, `fp`, `encryption`, `flow`, `headerType`, `host`.
- Метки: `<флаг> <Страна>, <Город>`, percent-encoded (`%20`, `%2C`, UTF-8 uppercase hex), 50 разных; 45 разных флагов (🇺🇸×5, 🇩🇪×2). 🇳🇬 — вне карты 45 стран (01 §7). Названия стран в метках не совпадают с ICU (`Czech Republic`, `Turkey`, `UAE`, `Hong Kong`); это не важно, страна берётся по флагу. Есть диакритика (`Bogotá`) и дефис (`San-Jose`).

### 4.3 Обезличенные фикстуры (`docker/fixtures/real-shapes/`)

Скрипт `scratchpad/realsubs/work/sanitize.py` заменяет **только** значения-секреты, на месте, сохраняя разделители, порядок и сырое percent-кодирование остальных параметров:

| Что | Замена |
|---|---|
| UUID | A: `00000000-0000-4000-8000-000000000001`, B: `…000000000002` |
| хост (+ `sni` в A) | A: `srv-<cc>-<n>.example.com` (форма и длина реальных, `cc`/`n` — из метки); B: первая метка хоста — случайные буквы и цифры той же формы (дефисы на месте) + `.example.com` (базовый домен на 1 символ короче реального) |
| `encryption` (A) | свежая клиентская строка `bin-xray-26.10.10 vlessenc` (аутентификация ML-KEM-768): тот же режим `native.0rtt`, ключ той же длины (1579 → 1184 байта, валидный) |
| `path` (A) | `%2F` + случайный UUID v4 lowercase (та же длина и алфавит) |
| `extra`, `mode`, `fp`, `alpn`, `type`, `security`, порты, метки | без изменений |

Проверено:
- инвентарь (§4.1/§4.2) по фикстурам совпадает с сырыми данными, кроме алфавита UUID и базового домена;
- TS: `buildManifest`, фильтры scd, разбор и фильтры sfc, решения регулярок дают **идентичный** JSON (теги — с точностью до `sha8(raw)`);
- Go-конвейер и живой Xray выдают **побайтно тот же** отчёт, что на сырых данных (`diff` пуст для всех трёх файлов);
- токен-скрипт `leakcheck.py`: 337 секретных токенов из сырых строк (UUID, хосты, их метки ≥6 символов, базовые домены, `sni`, `path`, окна по 24 символа из ключа) против фикстур, `docs/go-migration/**/*.md` и `docker/*/config.real-shape.yml` — **0 совпадений**; `provider2.b64` декодируется ровно в `provider2.txt`. Плюс leak guard координатора (рабочее дерево, индекс, история) — `LEAK-GUARD: clean`.

## 5. Результаты: TS сегодня, Go-план, живой Xray 26.10.10

| Источник | TS `scd generate` | Go-план (1.7 → 4.3–4.6) | Живой Xray 26.10.10 |
|---|---|---|---|
| A (4) | **0 parsed**, 4× `unsupported_param` «Unsupported VLESS parameter(s): mode, extra.» → exit 1 `produced no valid outbound entries` (вся подписка падает в sync) | 4 entry `xhttp-tls`, warnings нет, неприменимых параметров нет; ML-KEM-ключ: 1 сегмент, 1184 байта, `mlkem.NewEncapsulationKey768` OK | 4/4 `BUILD-OK LIVE-OK listEqual=true`, paramcheck — все параметры |
| B, текст (50) | 50 parsed, все `tcp-tls` | 50 entry `tcp-tls`; `path` — неприменим (O16), отброшен **молча**; warnings нет; **теги побайтно = TS (50/50)** | 50/50 `BUILD-OK LIVE-OK listEqual=true`, paramcheck — все |
| B, base64 (50) | то же (`encoding: base64`) | то же | то же |

Что именно проверено в `ListOutbounds` (адресно, по каждому параметру ссылки, кроме неприменимых):
- A: `Account.Id`; адрес/порт; `ProtocolName=splithttp`; `tls.Config`: `ServerName` == `sni`, `Fingerprint=chrome`, `NextProtocol=[h2]`; `splithttp.Config`: `Path`, `Mode=stream-up`, `XPaddingBytes={100,1000}`, `Xmux={MaxConcurrency 8-16, MaxConnections 0, CMaxReuseTimes 0, HMaxRequestTimes 600-900, HMaxReusableSecs 1800-3000, HKeepAlivePeriod 0}`, `NoGRPCHeader=false`, `Headers` пуст; `vless.Account`: `Encryption` = ключ без префикса, `XorMode=0` (native), `Seconds=1` (0rtt), `Padding=""`.
- B: `ProtocolName=tcp`, `tls.Config.NextProtocol=[h2]`, `ServerName` пуст (Xray возьмёт домен адреса), `Account.Encryption="none"`, `tcp.Config.HeaderSettings` отсутствует (`path` не дошёл, как и должно быть).
- После прогона `XRAY ALIVE`. Xray запускался с конфигом только из API (`HandlerService` на `127.0.0.1`) и `freedom`, без inbound'ов и routing. В его логе нет ни одной попытки соединения, к реальным серверам ничего не шло. Процесс остановлен.

Теги Go-плана (4.2): A — `vless-de-de-1-x-xhttp-tls-<sha8>`, `vless-de-de-2-x-…`, `vless-nl-nl-1-x-…`, `vless-nl-nl-2-x-…`. B — `vless-<cc>-<страна>-tcp-tls-<sha8>`: город = часть метки до первой `,`, то есть **название страны** (`vless-us-united-states-tcp-tls-…` ×5, `vless-de-germany-…` ×2, `vless-xx-nigeria-…`). Поведение TS такое же. Теги различаются только хэшем `sha8(raw)`.

## 6. Фильтры

### 6.1 scd (`docker/scd/config.real-shape.yml`: `countryAllowlist` из 24 кодов + `/^(?!.*Extra)(?!.*WhiteList).*$/i`)

| Источник | TS | Go (D29: `labelExcludeRegex: '/Extra\|WhiteList/i'`) |
|---|---|---|
| A | подписка падает раньше фильтров (0 entries) | 4/4 остаются (DE, NL в allowlist; `-X` не содержит `Extra`) |
| B | 25 остаются, 25 `filteredByCountry`: CO, AR, HK, ZA, JP, KR, KZ, PE, US×5, MX, BR, SE, SG, RU, AU, IL, UA, CA, AE, MY, 🇳🇬 (`xx`); `filteredByLabelRegex` 0 | то же (решения регулярки совпадают, §6.3) |

### 6.2 sfc (`docker/sfc/config.real-shape.yml`, три output'а)

| Источник | TS-sfc: разобрано | `en-server` | `extra-server` | `extra-whitelist` |
|---|---|---|---|---|
| A | **0** (`parseSubscriptionLine` молча отбрасывает строки с `mode`/`extra`) | 0 → «Filtered output is empty.» | 0 → empty | 0 → empty |
| B | 50 | 50 | 0 → empty | 0 → empty |

В Go (D27 O7 + D28: общий белый список, неизвестное игнорируется) строки A проходят sfc: `en-server` = 4. Это **видимое изменение выхода sfc после миграции**: если апстрим sfc владельца содержит xhttp/ML-KEM-строки, после перехода на Go они появятся в выходах (TS их тихо выбрасывал). Пункт для `docs/CONFIG-MIGRATION.md` (D32, раздел «Подписки»).

### 6.3 Эквивалентность D29

Харнесс `realcheck labels`: Go-формы из D29 (RE2, `(?i)`) против решений TS-регулярок, вычисленных `parseRegexLiteral` sfc в Node 22.22.0, на 54 реальных метках и 13 граничных.
- **Реальные метки: 54/54 совпадают** по всем трём фильтрам (`en`: 54 true; `extra`/`extraWhitelist`: 0).
- Граничные: 12/13 совпадают (`🇩🇪 Germany, Extra`, `Extra Whitelist`, `WhiteList Extra`, `EXTRA`, `🇩🇪DE-1-Extra`, `🇳🇱NL-2-X WhiteList`, пустая строка, кириллица, `extrawhitelist`).
- **Одно расхождение**: метка с переводом строки `Extra\nWhiteList`. В JS `.` не матчит `\n`, поэтому `(?=.*Extra)(?=.*WhiteList)` → false. Go-форма `include: [/Extra/i, /WhiteList/i]` → true. Метка из подписки может содержать `\n` только через `%0A` во фрагменте. На реальных данных такого нет. В корпус 0.6 строку внести как известное, обоснованное расхождение (`allowedDiffs`), Go-поведение не менять.

## 7. Сверка плана с реальными данными

| # | Что в данных | План (1.7, 4.2–4.7, 11) | Итог |
|---|---|---|---|
| 1 | tcp+tls **без `sni`** (B) | `sni` не обязателен; `tlsSettings` без `serverName` | ✓ live; Xray берёт SNI из домена адреса |
| 2 | **нет `encryption`** (B) | `""` → `none`, билдер пишет `"encryption":"none"` явно | ✓ |
| 3 | `alpn=h2` на **tcp**+tls (B) | `tlsSettings.alpn=["h2"]` | ✓ `NextProtocol=[h2]` (так же передавал TS) |
| 4 | `path=/` на tcp без `headerType` (B) | 4.3 п.9 / O16 / D28: **молча** игнорировать | ✓ не передаётся. **Но** 11 §4 (вводный абзац) говорит «warning `param_ignored`» для неприменимых параметров — это противоречит D28/O16 и дало бы 50 warnings на каждый sync B. Править формулировку 11 §4 по 4.3 п.9 (§9 п.10) |
| 5 | ML-KEM **`native.0rtt`**, один 1184-байтовый ключ (A) | `validateMLKEM` (4.3 п.3) | ✓ реальный ключ проходит `NewEncapsulationKey768`; conf → `XorMode=0, Seconds=1`; `AddOutbound` OK. В корпусе 0.5 этой комбинации нет (есть `xorpub.1rtt`, `random.0rtt`) — добавить |
| 6 | ML-KEM **поверх tls** (A) | профиль не меняется → `xhttp-tls` (11 §3 #13) | ✓ |
| 7 | `extra`: строки-диапазоны, `headers:{}`, `noGRPCHeader:false`, нулевые `xmux`-поля, `maxConcurrency` + `maxConnections:0` | 4.5: JSON-объект → `json.RawMessage` как есть | ✓ conf принимает (`maxConnections:0` не конфликтует с `maxConcurrency`); все поля дошли |
| 8 | `mode=stream-up` | `xhttpSettings.mode` | ✓ |
| 9 | xhttp `path` — секретоподобная строка (`/`+UUID-форма) | 5.2 `DescribeOutbound` при `includeSecrets=false` убирает `uuid`/`publicKey`/`shortId` и ключи ML-KEM, но **`path` выводит**. У владельца `runtimeState.enabled: true, includeSecrets: false` | ⚠ решение владельца: скрывать ли xhttp `path` (и `host`) при `includeSecrets=false` (§9 п.9) |
| 10 | длинная строка 2174 символа, `encryption` 1610 | тег — `sha8(raw)`; details — первые 40 символов | ✓ |
| 11 | метка без пробела после флага (`🇩🇪DE-1-X`) | `FromLabel` — первые две руны; `InferCity` срезает RI и пробелы | ✓ `de`, город `de-1-x` |
| 12 | метка `<флаг> <Страна>, <Город>` (B) | `InferCity` = до первой `,` | ✓ паритет с TS; город в теге = страна (так было и в TS, не баг плана) |
| 13 | 🇳🇬 вне карты | `xx`, отсекается allowlist | ✓ паритет |
| 14 | суффикс `-X` (A) | фильтры D29 ищут `Extra`/`WhiteList` | ⚠ вопрос владельцу: если `-X` значит «Extra», то обе версии (TS и Go) считают эти серверы обычными, и они попадают в `en`-выходы. Нужен ли фильтр по `-X`, решает владелец |
| 15 | порядок параметров Remnawave | 1.7: порядок первого появления, ключи lowercase | ✓ |

Противоречий с D14–D19, D24–D32 реальные данные не выявили. План принимает 54/54 реальные строки. TS сегодня принимает 50/54 и полностью теряет подписку Remnawave.

## 8. Предложения из HANDOFF на данных

**(a) Тег без хоста.**
- Признаков рандомизации нет. Хосты — фиксированные имена (`xxx-cc-N`, 10 устойчивых форм у B), `*`-производных 10-символьных меток нет, у A `sni` == хост. Но снимок у каждого источника один, а A получен через ветку `BROWSER`. Сравнить два запроса в клиентском формате нельзя.
- Коллизии: `sha8(uuid|port|profile|label)` — 0 дублей и в A, и в B. Дублей по `sha8(raw)` — 0. При этом 7 строк B делят префикс тега с другой строкой (US×5, DE×2). Значит, стабильный хэш должен включать **полную метку**, а не city-slug.
- Паритет: Go-теги по плану 4.2 побайтно равны TS-тегам на 50/50 строк B, то есть при миграции теги не поменяются.

**Рекомендация:** оставить `sha8(raw)` (как в плане и TS) и в 07 предложение пока не записывать. Вернуться к нему, только если два последовательных скачивания в клиентском формате (UA `scd/…`) различаются. Если владелец всё же примет вариант без хоста, использовать `sha8(uuid|port|profile|label)`: на реальных данных коллизий 0, при этом один раз при cutover сменятся все теги (по D21 это допустимо).

**(b) `fetchHeaders`.** Данные нужду **не подтверждают и не опровергают**. A получен через ветку `BROWSER`, которая возвращает страницу до проверки HWID. B выдал собственный sfc владельца (HWID там нет). Найдено ещё одно: если у пользователя Remnawave задан `externalSquadUuid`, fallback `XRAY_BASE64` ищет шаблон типа `XRAY_JSON` (`subscription.service.ts:120-140`). Поэтому клиентский ответ для этого пользователя нужно проверить ещё и на сам формат.

**Рекомендация:** опцию пока не добавлять. Владельцу один раз выполнить `curl -sS -A 'scd/1.0' -D- <ссылка Remnawave>` и посмотреть на ответ:
- тело — base64 с реальными строками и нет заголовков `x-hwid-*` → закрыть пункт;
- заглушки или `x-hwid-not-supported: true` → добавить `fetchHeaders` (задачи 1.6, 4.1, 3.1);
- JSON вместо base64 → отдельный вопрос о формате.

**(c) `cs` / `heartbeatPeriod`.** В реальных данных 0/54: ws-строк нет вообще, в tls-строках нет `cs`.

**Рекомендация:** данными не мотивировано, приоритет низкий. По букве D26 маппинг дешёвый (§10, старый п.1). Можно оставить в плане как пункт полноты D26 или отложить. На миграцию не влияет: без маппинга такие ссылки всё равно принимаются (D28, `param_ignored`).

## 9. Правки плана (решения 07 не меняются; 07 и plan/* в этой задаче не редактировались)

1. **0.5 корпус D26** (`d26-links.txt`/`d26-expect.json`), новые строки (значения — из `docker/fixtures/real-shapes/`, ключ — уже валидный из `vlessenc`):
   - `RS1` — строка 1 `remnawave.txt` как есть: `expect: entry`, `profile: xhttp-tls`, `matrixRow: 3#13`, `warnings: []`, `params: [type,security,encryption,sni,fp,alpn,path,mode,extra]`, note «Remnawave xhttp+tls+ML-KEM native.0rtt, stream-up, extra xmux-строки, порядок параметров Remnawave»;
   - `RS2` — строка 1 `provider2.txt`: `entry`, `tcp-tls`, `matrixRow: 3#1`, `warnings: []`, `params: [type,security,alpn]` (`path` не входит — O16), note «tcp+tls без sni/fp/encryption, path=/ игнорируется молча».
   - Шаг 3 (Verify): добавить проверку `grep -c . docker/fixtures/real-shapes/*.txt` → 4 и 50.
2. **0.2 эталоны TS:** снять `testdata/reference/scd/generate/real-shapes-provider2.{manifest,log}.json` (`--input docker/fixtures/real-shapes/provider2.b64`) и текст ошибки для `remnawave.txt` (`real-shapes-remnawave.error.txt`: `… unsupportedParam=4 …`).
3. **0.3 эталоны sfc:** refresh TS-sfc по обеим фикстурам с тремя output'ами из `config.real-shape.yml` → `testdata/reference/sfc/real-shapes.json` (B: 50/0/0; A: 0/0/0 — «Filtered output is empty.»).
4. **0.6 корпус меток:** добавить все 54 реальные метки (`remnawave.txt`: `🇩🇪DE-1-X`, `🇩🇪DE-2-X`, `🇳🇱NL-1-X`, `🇳🇱NL-2-X`; `provider2.txt`: 50 меток `<флаг> <Страна>, <Город>`, в т.ч. `🇨🇴 Colombia, Bogotá`, `🇺🇸 United States, San-Jose`, `🇳🇬 Nigeria, Lagos`, `🇦🇪 UAE, Fujairah`) и граничные из §6.3: `🇩🇪DE-1-Extra`, `🇳🇱NL-2-X WhiteList`, `🇩🇪 WhiteList Extra`, `extrawhitelist`, `🇷🇺 Москва, Extra, WhiteList`, `Extra\nWhiteList`. Последняя — в `allowedDiffs` теста `TestLabelFilterRealShapeEquivalence` (4.1) с причиной «JS `.` не матчит `\n`».
5. **1.7** `TestParseRealShapes`: 54/54 строки фикстур → `Parse` без skip и без `param_ignored`; у A порядок `Params` = `encryption,type,path,mode,extra,security,sni,fp,alpn`, `path` декодирован (`%2F` → `/`), `extra` — валидный JSON.
6. **4.2** `TestBuildTagRealShapes`: `🇩🇪DE-1-X` → `vless-de-de-1-x-xhttp-tls-…`; `🇺🇸 United States, Miami` → `vless-us-united-states-tcp-tls-…`; `🇳🇬 Nigeria, Lagos` → `vless-xx-nigeria-…`; `🇨🇴 Colombia, Bogotá` → `colombia`.
7. **4.3–4.5** `TestNormalizeRealShapes`: A → `xhttp-tls`, `XHTTPMode=stream-up`, `XHTTPExtra` побайтно = декодированный `extra`, `ALPN=[h2]`, `Fingerprint=chrome`, warnings пусто; B → `tcp-tls`, `SNI=""`, `Encryption=none`, `ALPN=[h2]`, `HeaderType=nil`, **warnings пусто** (нет `param_ignored` за `path`). `TestMLKEMNative0RTTRealShapeKey` — ключ из фикстуры A проходит `validateMLKEM`.
8. **4.6** `TestToProtoRealShapes`: A → `XorMode=0`, `Seconds=1`, `Padding=""`, `splithttp.Config.Xmux` = диапазоны из `extra`, `NoGRPCHeader=false`; B → нет `tcp.Config.HeaderSettings`, `tls.Config.ServerName==""`.
   **4.7** `TestBuildRealShapeProvider2MatchesTS`: теги и `summary` = эталону п.2 (50/50 тегов равны). С Go-формой фильтров `config.real-shape.go.yml`: 25 entries, `filteredByCountry=25`. `TestBuildRealShapeRemnawave`: 4 entries `xhttp-tls` (TS: 0 — намеренное расхождение D26), фильтры оставляют 4.
9. **5.2** (решение владельца, §7 п.9): при `includeSecrets=false` выводить xhttp `path`/`host` как `<redacted>`; тест `TestDescribeOutboundRedactsXHTTPPath`. Если владелец против — зафиксировать в 07, что `path` не секрет.
10. **11 §4, вводный абзац** (документ, не решение): «Игнор … warning `param_ignored`» заменить на «молча (O16, D28); `param_ignored` — только для неизвестных параметров (1.7)», чтобы 4.3 п.9 и матрица не расходились.
11. **3.2** `TestRefreshRealShapes`: фикстуры + три output'а → B: `en-server` 50, остальные «Filtered output is empty.»; A: `en-server` 4 (TS: 0 — намеренное расхождение, в `allowedDiffs` с причиной D27 O7/D28).
12. **5.6** `TestLiveEveryParamReachesXray`: добавить вход `docker/fixtures/real-shapes/remnawave.txt` и `provider2.txt` (54 строки; ожидание — §5 этого документа).
13. **D32 / `docs/CONFIG-MIGRATION.md` «Подписки»:** выходы sfc могут получить строки xhttp/ML-KEM/REALITY+`pqv`, которые TS-sfc молча выбрасывал (§6.2).
14. Вопросы владельцу (в 07 не записаны): `-X` в метках (§7 п.14); редактирование xhttp `path` (п.9); проверка `curl -A scd/1.0` (§8b).

## 10. Remnawave-образные строки на фейковых данных (до получения реальных; сохранено)

Файл (только scratchpad): 16 строк, собраны скриптом по правилам §2. Это формы, которые генератор умеет выдавать, а не реальные строки.

| # | Форма | TS сегодня | Go-план |
|---|---|---|---|
| 1 | tcp+reality+vision, `sid`(16), `pqv`, `spx` | skip `unsupported_param` (pqv, spx) | entry `tcp-reality-vision` |
| 2 | tcp+reality+vision, `sni=`, без `sid` | skip `missing_required_field` (sni) | entry `tcp-reality-vision` |
| 3 | xhttp+reality, `mode=auto`, `extra` | skip `unsupported_param` | entry `xhttp-reality` |
| 4 | xhttp+tls, `alpn`, `cs` | skip `unsupported_param` | entry `xhttp-tls` (+`param_ignored` cs, пока нет маппинга) |
| 5 | ws+tls, `heartbeatPeriod` | skip `unsupported_param` | entry `ws-tls` (+`param_ignored`) |
| 6 | tcp+none+ML-KEM(X25519), `headerType=none` | skip `unsupported_combo` | entry `tcp-mlkem` |
| 7 | xhttp+none+ML-KEM-768 xorpub.1rtt+padding | skip `unsupported_param` | entry `xhttp-mlkem` |
| 8 | UUID vlessRoute (версия 0) | skip «does not contain a valid UUID» | entry `tcp-reality-vision` |
| 9 | дубль remark ` ^~2~^` + `?serverDescription=` | entry, мусор в city | entry, тот же мусор |
| 10 | xhttp+reality + `fm` | skip `unsupported_param` | entry `xhttp-reality` |
| 11 | tcp+tls+vision `headerType=http` | skip `unsupported_value` | entry `tcp-tls` |
| 12 | grpc+reality | skip `unsupported_param` | skip `unsupported_combo` |
| 13 | заглушка `0.0.0.0:1` | skip (nil UUID) | skip `unsupported_combo` |
| 14–16 | trojan / ss / hysteria2 | skip `unsupported_scheme` ×3 | то же |

Живой прогон этих форм: 12/12 `BUILD-OK LIVE-OK listEqual=true`.

Выводы из исходников Remnawave, которые остаются в силе (реальные данные их не отменяют):
1. `cs` (tls → `tlsSettings.cipherSuites`) и `heartbeatPeriod` (ws → `wsSettings.heartbeatPeriod`, uint32) — Xray поддерживает, conf значения не проверяет. По D26 их можно добавить в 11 §4, 4.4/4.5, `paramcheck.Covered` (20 → 22), 5.6. На реальных данных не встречаются (§8c).
2. Корпус 0.5, строки из исходников: (a) tcp+reality+vision в порядке Remnawave `encryption,flow,type,security,sni,fp,pbk,sid,pqv,spx`; (b) reality `sni=` без `sid`; (c) xhttp+reality `mode=auto` + `extra` с `xmux`; (d) xhttp+tls с `cs`; (e) ws+tls с `heartbeatPeriod`; (f) xhttp+none+ML-KEM-768 `xorpub.1rtt`+padding; (g) UUID vlessRoute (версия 0); (h) заглушка `0.0.0.0:1` nil-UUID → skip combo (`stage: scd`, «conf пропустил бы»); (i) `heartbeatPeriod=abc` → skip. Реальные строки — §9 п.1.
3. UA и extended-клиенты (D27 O9): по умолчанию `scd/<version>` получает base64 share-links (кроме случая `externalSquadUuid`, §8b). `fetchUserAgent` вида `Happ/…`/`INCY/…`/`FlClash X/…` добавляет `?serverDescription=` во фрагмент (засоряет label/city); `clash*`/`mihomo`/`stash`/`sfa…` возвращают YAML/JSON. Предупредить в README. Тест 4.7 фиксирует поведение для label с `?serverDescription=`.
4. Заглушки HWID/expired/limited (`0.0.0.0:1`, nil-UUID) → `produced no valid outbound entries`. Ошибку стоит дополнять ответными заголовками `x-hwid-not-supported`/`x-hwid-max-devices-reached`, если они пришли (вместе с §8b).
5. `subscription-userinfo` (трафик/срок) приходит всегда. В плане не используется, кандидат на дашборд (вне scope).

## 11. Как воспроизвести (scratchpad, вне репозитория)

Корень — `scratchpad/realsubs/`. Сырые данные — только в `raw/` (секреты, в репозиторий не копировать).
- `work/inventory.py`, `work/details.py` — инвентарь с маскированием значений (печатают только формы, длины и алфавиты).
- `work/tscheck.mjs` — TS `buildManifest` + `applyManifestFilters` (фильтры scd из `config.real-shape.yml`) + разбор и фильтры TS-sfc; `work/tslabels.mjs` — TS-теги и решения трёх регулярок по меткам. Выход: `ts-raw/` (сырые данные), `ts-fix/` (фикстуры). Плюс `node scd/dist/cli.js generate` (Node 22.22.0) для `remnawave.txt`, `provider2.txt`, `provider2.b64`.
- `realcheck/` — Go-харнесс (модуль на `xray-core v1.260327.1-0.20261010092107-701af60772cd`, `GOTOOLCHAIN=go1.27.2`). Он реализует план 1.7 → 4.2–4.6 и для каждой строки проверяет `conf.Build` → `AddOutbound` → `ListOutbounds` → `proto.Equal` + paramcheck. Запуск:
  - `bin_realcheck run 127.0.0.1:18087 <файл> <ts-tags.json> <источник>`;
  - `bin_realcheck labels <ts-labels.json>` — сверка D29.
  Xray: `bin-xray-26.10.10 run -c live/xcfg.json` (только API + `freedom`). Выход — `live/*.out` (raw) и `live/fix-*.out` (фикстуры), они идентичны.
- `work/sanitize.py` (фикстуры; ключ — `fixgen/vlessenc.out`), `work/leakcheck.py` (токен-проверка), `guard/check.sh` (leak guard координатора).
- `remna/backend` — клон `remnawave/backend` @ `010b365` (ссылки на строки в §1–§3, §8).
