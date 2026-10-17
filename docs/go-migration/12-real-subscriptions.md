# 12. Реальные подписки: форматы, параметры, классификация против матрицы D26

Статус на 2026-10-17: **частично**. Владелец дал две реальные подписки (A — панель Remnawave, есть xhttp; B — провайдер с неизвестным backend, предположительно REALITY). Скачать их из песочницы **не удалось**: egress-прокси сессии отвечает `403` на `CONNECT` к обоим хостам (policy denial, `__agentproxy/status` → `connect_rejected`). Обходить политику не пытались. Поэтому реальные результаты (форматы по UA, инвентарь параметров реальных строк, TS сегодня на реальных данных, санитизированные фикстуры `docker/fixtures/real-shapes/*.txt`) **не получены**; что осталось — §0. Фикстуры не созданы, чтобы не выдавать выдуманные данные за реальные. Что можно было сделать без сети подписок, сделано: разобран исходный код генератора Remnawave (§1–§3), по его правилам собраны ссылки с фейковыми данными и прогнаны через TS `generate` и живой Xray 26.10.10 (§4, воспроизведение — §6). Из этого выведены изменения плана (§5).

Секреты (URL, UUID, хосты, ключи) в этот документ и в репозиторий не попадают.

## 0. Что нужно, чтобы закончить §1–§4

1. Владелец разрешает два хоста подписок в настройках сети окружения (Network access → Allowed domains) **или** кладёт сырые ответы в песочницу вне репозитория.
2. Повторить сбор: по UA `curl` (по умолчанию), `v2rayN/7.0`, `v2rayNG/1.9`, `Happ/1.0`, `clash-verge/2.0`, `clash.meta`, `sing-box/1.11`, `Streisand`, `scd/1.0`, `node`. Записать статус, `content-type`, имена и форму заголовков, формат, число строк.
3. Для share-link-варианта прогнать проверки из §4/§6 (TS `generate` и harness `AddOutbound`/`ListOutbounds`) на реальных строках, затем сделать санитизированные фикстуры и проверить токен-скриптом, что ни одно реальное значение в них не попало.

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

| Параметр / форма | Есть в 11 §4 / плане | Xray 26.10.10 (живой прогон §6) | Go-план сегодня | Нужно |
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
| `?serverDescription=…` после `#` (extended-клиенты) | нет | — | попадёт в label → в city/тег | см. §5 п.4 |
| remark-суффикс ` ^~N~^` | нет | — | идёт в city-slug (`…-testcity-2-…`) | только тест (поведение как у TS) |
| `serviceName`, `mode=gun` (grpc), `mtu`/`tti` (kcp) | 11 §4.1 | — | skip combo | — |
| `ech` | да | — | Remnawave его в ссылки не пишет | — |

## 4. Классификация (Remnawave-образные строки, фейковые данные)

Файл (только scratchpad, не в репозитории): 16 строк, собранных скриптом по правилам §2. Это **не** реальные строки, а формы, которые генератор умеет выдавать.

| # | Форма | TS сегодня | Go-план (ожидание по 11 + 07) |
|---|---|---|---|
| 1 | tcp+reality+vision, `sid`(16), `pqv`, `spx` | skip `unsupported_param` (pqv, spx) | entry `tcp-reality-vision` |
| 2 | tcp+reality+vision, `sni=`, без `sid` | skip `missing_required_field` (sni) | entry `tcp-reality-vision` |
| 3 | xhttp+reality, `mode=auto`, `extra` | skip `unsupported_param` (mode, extra) | entry `xhttp-reality` |
| 4 | xhttp+tls, `alpn`, `cs` | skip `unsupported_param` (mode, cs) | entry `xhttp-tls` + `param_ignored` cs (после §5 п.1 — без warning) |
| 5 | ws+tls, `heartbeatPeriod` | skip `unsupported_param` | entry `ws-tls` + `param_ignored` (после §5 п.1 — без warning) |
| 6 | tcp+none+ML-KEM(X25519), `headerType=none` | skip `unsupported_combo` | entry `tcp-mlkem` |
| 7 | xhttp+none+ML-KEM-768 xorpub.1rtt+padding | skip `unsupported_param` (mode) | entry `xhttp-mlkem` |
| 8 | UUID vlessRoute (версия 0) | skip «does not contain a valid UUID» | entry `tcp-reality-vision` |
| 9 | дубль remark ` ^~2~^` + `?serverDescription=` | **entry**, тег `vless-de-germany-testcity-2-serverdescrip-…` | entry, тот же мусор в city |
| 10 | xhttp+reality + `fm` | skip `unsupported_param` (mode, fm) | entry `xhttp-reality` |
| 11 | tcp+tls+vision `headerType=http` | skip `unsupported_value` | entry `tcp-tls` |
| 12 | grpc+reality | skip `unsupported_param` | skip `unsupported_combo` |
| 13 | заглушка `0.0.0.0:1` | skip (nil UUID) | skip `unsupported_combo` (security=none без ML-KEM) |
| 14–16 | trojan / ss / hysteria2 | skip `unsupported_scheme` ×3 | то же |

TS `generate`: `totalLines 16, parsed 1, skipped 15` (`unsupportedParam 7`, `missingRequiredField 3`, `unsupportedScheme 3`, `unsupportedValue 1`, `unsupportedCombo 1`). Итог: из 13 vless-форм TS принимает 1, Go-план — 11.

Живой прогон (`bin-xray-26.10.10`, JSON в форме билдера 4.6 → `conf.Build` → `AddOutbound` → `ListOutbounds`, без соединений с реальными серверами): 12/12 `BUILD-OK LIVE-OK listEqual=true`, `XRAY ALIVE`. Отдельно проверено: `cipherSuites`, `heartbeatPeriod:30` + `ed:2560`, `xor=1/pad`, `spiderY` у reality без `spx`, `shortId` для `sid` длиной 4/8/16, адрес `0.0.0.0`.

## 5. Последствия для плана (решения не меняются, только список правок)

1. **Матрица 11 §4 и задачи 4.4/4.5/4.6:** добавить `cs` (tls → `tlsSettings.cipherSuites`, строка как есть; conf/API значение не проверяют, неверный набор ломает только dial — как `ech`) и `heartbeatPeriod` (ws → `wsSettings.heartbeatPeriod`, парсинг в uint32 в normalize). Добавить оба в `paramcheck.Covered` (сейчас 20 → 22) и в live-тест 5.6. Это прямое следствие D26 («любой параметр, который Xray поддерживает для выбранных транспортов»). Без правки они уходят в D28 `param_ignored`: ссылка работает, но настройка админа теряется.
2. **Корпус 0.5 (`d26-links.txt`/`d26-expect.json`), новые строки:** (a) Remnawave tcp+reality+vision с порядком параметров `encryption,flow,type,security,sni,fp,pbk,sid,pqv,spx`; (b) reality `sni=` без `sid`; (c) xhttp+reality `mode=auto` + `extra` с `xmux`; (d) xhttp+tls с `cs`; (e) ws+tls с `heartbeatPeriod`; (f) xhttp+none+ML-KEM-768 `xorpub.1rtt`+padding в форме Remnawave; (g) UUID vlessRoute (версия 0); (h) заглушка `0.0.0.0:1` nil-UUID → skip combo (`stage: scd`), явно с комментарием «conf пропустил бы»; (i) `heartbeatPeriod=abc` → skip. Ссылки на реальные фикстуры `docker/fixtures/real-shapes/remnawave.txt` и `provider2.txt` добавить, когда они появятся (§0).
3. **Тег и churn (риск, противоречит ожиданию «теги стабильны», 01 §6.4 / 09 §5):** хэш тега — `sha8(raw)`. У Remnawave строка может меняться **на каждый запрос** (`*`-хосты, списки через `,`, `randomizeHosts` меняет только порядок). Каждый sync тогда даёт новые теги: remove+add всех outbound'ов и перестройку туннелей. TS ведёт себя так же. Владельцу нужно решить, хэшировать ли нормализованный JSON-outbound без случайных частей или оставить как есть. На реальных данных проверить два последовательных скачивания (diff).
4. **UA и extended-клиенты (D27 O9):** по умолчанию `scd/<version>` у Remnawave даёт base64 share-links — это правильно. В README предупредить, что `fetchUserAgent` вида `Happ/…`, `INCY/…`, `FlClash X/…`, `Flowvy/…`, `prizrak-box/…`, `koala-clash/…` включает `?serverDescription=` в фрагменте (засоряет label/city), а `clash*`/`mihomo`/`stash`/`sfa…` дают YAML/JSON вместо share-links. Тест 4.7: label с `?serverDescription=` — фиксированное поведение (без вырезания) или вырезание суффикса. Это решение владельца.
5. **HWID (новое, плана нет):** при включённом HWID-лимите панель без `x-hwid` отдаёт пустой ответ или заглушки → `produced no valid outbound entries`. Нужна опция конфига для произвольных заголовков запроса подписки (например `subscriptions[].fetchHeaders: {x-hwid: …}`) рядом с `fetchUserAgent` (задачи 1.6 `inputsrc.Options`, 4.1, 4.7). Ошибку «нет entries» стоит дополнять ответными заголовками `x-hwid-not-supported`/`x-hwid-max-devices-reached`, если они пришли.
6. **Заголовок `subscription-userinfo`** (трафик и срок) приходит всегда. В плане он не используется; кандидат на дашборд (не в scope, только отметить).
7. **D28 vs `param_ignored`-шум:** у Remnawave на xhttp всегда есть `mode`, на ws может быть `heartbeatPeriod`. После п.1 warnings на типичной подписке Remnawave нулевые. Если п.1 не принят, на каждой ws-строке будет `param_ignored` уровня `debug` — допустимо.
8. Противоречий с D14–D19, D24–D28 в исходниках Remnawave не найдено. Подтверждено: O15 (пустой `sni`, нет `sid`), D18 (vlessRoute-UUID — без D18 эти строки терялись бы), 11 §3 #15 (проверка в normalize обязательна: заглушка `0.0.0.0` проходит conf).

## 6. Как воспроизвести (scratchpad, вне репозитория)

- `realsubs/synthetic/synth.mjs` — эмуляция `serializeLink`/`buildQueryString` Remnawave на фейковых данных (ключи — `plan-update/keys/`, сняты `bin-xray-26.10.10`); `synth.txt`/`synth.b64`; `ts-manifest.json`, `ts-log.json` — `node scd/dist/cli.js generate` (Node 22.22.0).
- `realsubs/remnacheck/` — harness (модуль на `xray-core v1.260327.1-0.20261010092107-701af60772cd`, `GOTOOLCHAIN=go1.27.2`): `cases.json` → `conf.Build` → живой `AddOutbound`/`ListOutbounds` на `127.0.0.1:18085`; вывод — `remnacheck.out`.
- `remna/backend` — клон `remnawave/backend` @ `010b365`.
