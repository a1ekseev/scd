# 11. Матрица VLESS-outbound'ов Xray v26.10.10 в scope D26 и маппинг параметров ссылки

Источник истины для задач нормализатора/билдера (plan stage-4) и live-тестов (stage-5). Дополняет `09-subscriptions-xray-26.10.10.md`; где расходится с 09 §1, §4, §7 — действует этот документ (он основан на D26 и на живом прогоне). Решения не меняет: D26 (scope + максимальная совместимость), D15 (`allowInsecure`), D16 (flow), D17 (`sid`), D18 (id), D19 (JSON → `infra/conf` → API), D25 (текст ошибки ключа ML-KEM).

Ссылки `new:<path>:<line>` — дерево Xray-core v26.10.10 (`701af60772cda123492f86e383e1fdba066614a9`, scratchpad `xray-core-26.10.10/`); `x3ui:` — 3x-ui `608ef4e` (2026-10-14) `internal/sub/service.go`; `v2rayN:` — `ServiceLib/Handler/Fmt/BaseFmt.cs`. Стандарт share-ссылок #716 из песочницы недоступен (`github.com/.../discussions/716` → 403, GraphQL закрыт), поэтому используется его сводка из 09 §3.2 и исходники генераторов.

## 1. Scope (D26)

| Ось | В scope | Вне scope → skip |
|---|---|---|
| `type` | `tcp`, `raw` (алиас), `ws`, `xhttp` | `grpc`, `httpupgrade`, `kcp`, `http`/`h2`/`h3`/`quic` (последние Xray удалил), всё прочее |
| `security` | `tls`, `reality`; `none`/пусто **только** вместе с ML-KEM | `none` без ML-KEM; `xtls` и неизвестные значения |
| `encryption` | `none` (или пусто), `mlkem768x25519plus.*` | прочие значения |
| `flow` | `""`, `xtls-rprx-vision`, `xtls-rprx-vision-udp443` | прочие значения |

Вне scope, но Xray v26.10.10 это умеет (проверено живьём, на будущее): `grpc`+tls, `grpc`+reality, `httpupgrade`+tls, `kcp`+ML-KEM — `conf.Build` OK, `AddOutbound` OK, `ListOutbounds` равен отправленному. grpc/httpupgrade/ws Xray помечает deprecated (`new:infra/conf/transport_internet.go:24-32`).

## 2. Как проверялось

1. Ключи — из релизного бинаря того же коммита (`scratchpad/bin-xray-26.10.10`, `Xray 26.10.10 … 701af60`): `xray vlessenc` (две клиентские строки: X25519-аутентификация, 32 байта; ML-KEM-768, 1184 байта), `xray x25519` (pbk), `xray mldsa65` (`Verify` → pqv, 1952 байта), `xray tls ech --serverName ech.example.test` (ECHConfigList). Каталог: `scratchpad/plan-update/keys/`.
2. Программа `scratchpad/plan-update/matrix/main.go` (модуль на `github.com/xtls/xray-core v1.260327.1-0.20261010092107-701af60772cd`, `GOTOOLCHAIN=go1.27.2`): для каждого кейса строит JSON-outbound в форме конфига v26 → `conf.OutboundDetourConfig.Build()` → `HandlerService.AddOutbound` в **живой** Xray (`bin-xray-26.10.10 run -c plan-update/xcfg/config.json`, API `127.0.0.1:18085`) → `ListOutbounds` → `proto.Equal` для `SenderSettings` и `ProxySettings` → печать разобранного proto → `RemoveOutbound`. Логгер Xray перехвачен (`xlog.RegisterHandler`), deprecation-warning'и печатаются в отчёт.
3. Запуск: `cd plan-update/matrix && GOTOOLCHAIN=go1.27.2 go build -o bin_matrix . && ./bin_matrix 127.0.0.1:18085 ../keys > ../matrix.out`. Итог (`plan-update/matrix.out`): **95 кейсов**: 69 `BUILD-OK LIVE-OK listEqual=true` (ни одного `listEqual=false`), 25 `BUILD-ERR` (включая OOS `http` → removed-error), 1 `BUILD-OK LIVE-ADD-ERR`; в конце `XRAY ALIVE after all cases`. Процесс Xray после прогона остановлен.

## 3. Матрица transport × security × encryption × flow

✓ — `conf.Build` OK, `AddOutbound` OK, `ListOutbounds` == отправленному. ✗ — ошибка (текст ниже). «рантайм ✗» — Xray принимает, но каждое соединение падает.

| # | network | security | encryption | flow | Xray v26.10.10 | Поведение scd | profile |
|---|---|---|---|---|---|---|---|
| 1 | tcp/raw | tls | none | — | ✓ | принять | `tcp-tls` |
| 2 | tcp | tls | none | vision / vision-udp443 | ✓ (vision требует TLS 1.3 у сервера) | принять | `tcp-tls` |
| 3 | tcp | reality | none | — | ✓ | **принять** (было O1 combo) | `tcp-reality` |
| 4 | tcp | reality | none | vision | ✓ | принять | `tcp-reality-vision` |
| 5 | ws | tls | none | — | ✓ (+warning deprecated) | принять | `ws-tls` |
| 6 | ws | tls | none | vision | ✓ conf, **рантайм ✗** «XTLS only supports TLS and REALITY directly for now.» (`new:proxy/vless/outbound/outbound.go:282`) | flow сброс + warning `flow_cleared` (D16) | `ws-tls` |
| 7 | ws | reality | * | * | ✗ «REALITY only supports RAW, XHTTP and gRPC for now.» (`new:infra/conf/transport_internet.go:107`) | skip `unsupported_combo` | — |
| 8 | xhttp | tls | none | — | ✓ | принять | `xhttp-tls` |
| 9 | xhttp | reality | none | — | ✓ | принять | `xhttp-reality` |
| 10 | xhttp | tls/reality | none | vision | ✓ conf, **рантайм ✗** (как #6: conn не `*tls.Conn`/`*reality.UConn`) | flow сброс + `flow_cleared` | `xhttp-tls` / `xhttp-reality` |
| 11 | tcp/ws/xhttp | none | ML-KEM | — | ✓ (в т.ч. публичный IP `1.1.1.1`) | принять | `tcp-mlkem` / `ws-mlkem` / `xhttp-mlkem` |
| 12 | tcp/ws/xhttp | none | ML-KEM | vision | ✓; vision внутри VLESS Encryption допустим на любом транспорте (`outbound.go:264-268`, 3x-ui `vlessFlowAllowed`) | принять, flow сохраняется | `<net>-mlkem` |
| 13 | tcp/xhttp | tls/reality | ML-KEM | ± vision | ✓ | принять, flow сохраняется | как без ML-KEM; reality+vision → `<net>-reality-vision` |
| 14 | ws | tls | ML-KEM | vision | ✓ | принять, flow сохраняется | `ws-tls` |
| 15 | любой | none | none | * | публичный адрес ✗ «vless without TLS or other encryption is prohibited unless the server address is a private IP or domain» (`new:infra/conf/xray.go:231-253,343`); **приватный** IP/домен (`10.1.2.3`, `*.test`, TEST-NET `198.51.100.0/24`) — ✓ | skip `unsupported_combo` (вне scope D26; проверять в normalize явно, т.к. фикстуры с `.test` conf пропустит) | — |
| 16 | — | xtls | — | — | ✗ removed-error (`transport_internet.go:119`) | skip combo | — |

**Профили и теги.** Профиль = `<network>-<security>` (`network` ∈ `tcp|ws|xhttp`, `raw` → `tcp`; `security` ∈ `tls|reality|mlkem`, где `mlkem` — это `security=none` + ML-KEM) + суффикс `-vision` **только** для `reality` с сохранённым vision-flow. Отсюда три старых профиля TS (`tcp-tls` вкл. tls+vision, `ws-tls`, `tcp-reality-vision`) и их теги `vless-<cc>-<city>-<profile>-<sha8(raw)>` не меняются; новые: `tcp-reality`, `xhttp-tls`, `xhttp-reality`, `xhttp-reality-vision` (только с ML-KEM, см. #13/#10), `tcp-mlkem`, `ws-mlkem`, `xhttp-mlkem`. ML-KEM поверх tls/reality профиль не меняет (тег уникален за счёт `sha8(raw)`).

## 4. Маппинг параметров ссылки → JSON (`streamSettings`/`settings`) → proto

> **Поправка (D28/O16, D33; 2026-10-17):** неприменимые к transport/security параметры («Игнор» ниже) отбрасываются **молча**, без warning; `param_ignored` (уровень `debug`) — только для неизвестных параметров (план 1.7). Плюс по D33 в маппинг добавлены `cs` → `tlsSettings.cipherSuites` и ws `heartbeatPeriod` → `wsSettings.heartbeatPeriod` (см. `12-real-subscriptions.md` §3).

«Игнор» — параметр не относится к выбранным transport/security; Xray такие ключи тоже не использует. Поведение scd: не передавать, warning `param_ignored` «<key> is not used with <network>+<security>; ignored.». Значения из ссылки — после `url.QueryUnescape`.

| Параметр | Где действует | JSON-ключ (форма v26) | Что делает conf / proto | Проверено (кейс `matrix.out`) | Ошибка Xray → skip |
|---|---|---|---|---|---|
| `type` | все | `streamSettings.network` = `tcp` \| `ws` \| `xhttp` (`raw` → пишем `tcp`) | `TransportProtocol.Build`: `raw/tcp→tcp`, `ws→websocket`, `xhttp→splithttp` (`transport_internet.go:16-46`) | `raw-alias+tls`, `ws+tls`, `xhttp+tls` | OOS → combo (до conf) |
| `security` | все | `streamSettings.security` = `tls` \| `reality` \| `none` | `""/none` — без security; `tls`, `reality` (`:91-123`) | матрица §3 | combo (до conf) |
| `encryption` | все | `users[0].encryption`: `none` или строка ML-KEM как в ссылке | conf отрезает `mlkem768x25519plus.<mode>.<rtt>.`, сегменты <20 символов → `Account.Padding`, `XorMode` native/xorpub/random = 0/1/2, `Seconds` 1 для 0rtt (`vless.go:336-378`) | `mlkem xorpub.1rtt` (xor=1 sec=0), `mlkem random.0rtt` (xor=2 sec=1), `mlkem padding segs` (pad=`100-111-1111.75-0-111.50-0-3333`), `mlkem two keys` | `None`, пусто в JSON, `foo` mode, `2rtt` → ✗ conf; **ключ правильной длины, но не ключ** (1184 случайных байта) → conf ✓, `AddOutbound` ✗ `proxy/vless/outbound: failed to use encryption > mlkem: invalid polynomial encoding` — ловить в normalize (D25: «Invalid VLESS encryption key.») |
| `flow` | все | `users[0].flow` | `""`, `xtls-rprx-vision`, `…-udp443`, иначе ✗ (`vless.go:329-334`) | `tcp+tls+vision-udp443`, `flow bogus` ✗ | ✗ → skip; рантайм-несовместимость → сброс (§3 #6/#10) |
| userinfo id | все | `users[0].id` | `uuid.ParseString` (≤30 символов → UUIDv5; 32–36 hex), канонизация (`vless.go:323-327`) | `id non-uuid string` (`my-user` → `db93355e…`) | ✗ → skip (O4) |
| `sni` | tls, reality | `tlsSettings.serverName` / `realitySettings.serverName` | как есть; пустой → dial по адресу | `tls sni+alpn+fp`, `reality sni empty` ✓ | — (REALITY без `sni` Xray принимает → **не** требовать) |
| `fp` | tls, reality | `…Settings.fingerprint` (пишем lowercase) | TLS: lowercase, `GetFingerprint` != nil **или** `unsafe` (`transport_security.go:355-358`); REALITY: + запрет `unsafe`/`hellogolang` (`:180-186`); пусто = HelloChrome_Auto | `fp=Chrome`→`chrome`, `randomized`, TLS `unsafe` ✓; REALITY без `fp` ✓ | TLS `bogus`, REALITY `unsafe` ✗ |
| `alpn` | tls | `tlsSettings.alpn` (split `,`, trim, без пустых) | `NextProtocol`; >1 элемента с `fromMitm` ✗ (`:337-346`) | `h2,http/1.1`, `h3` (xhttp) ✓ | reality/none → игнор |
| `pcs` | tls | `tlsSettings.pinnedPeerCertSha256` (строка как в ссылке) | split `,`, trim, удалить `:`, hex, ровно 32 байта (`:365-381`) | `tls pcs (colon hex)` ✓ (два значения) | `zz`, `abcd` ✗; reality → игнор |
| `vcn` | tls | `tlsSettings.verifyPeerCertByName` (строка) | split `,`, trim, без пустых (`:382-390`) | `tls vcn` ✓ | reality → игнор |
| `ech` | tls | `tlsSettings.echConfigList` (строка как есть) | передаётся без проверки (`:399`); на dial: base64 ECHConfigList **или** DNS-форма `name+https://doh` / URL (`tls/ech.go:51-79`); ошибка разбора только логируется, соединение заведомо падает (`tls/config.go:476-480`) | base64 из `xray tls ech`, DNS-форма, `!!!notbase64` — все ✓ | conf/API не отвергают ничего → принимать как есть; reality → игнор |
| `pbk` | reality | `realitySettings.password` | base64url **без** паддинга, ровно 32 байта (`:190-198`) | ✓; `publicKey`-ключ тоже ✓ | std-base64 с `=` ✗ conf → normalize заранее приводит (`=` убрать, `+/`→`-_`); пусто ✗ `empty "password"` |
| `sid` | reality | `realitySettings.shortId` | hex ≤16 символов → 8 байт с дополнением нулями (`:199-208`) | пусто → 8 нулей, `ab` → `qwAAAAAAAAA=` ✓ | `abc` (нечётная), 18 символов ✗ |
| `spx` | reality | `realitySettings.spiderX` | должен начинаться с `/`; `p,c,t,i,r` → `SpiderY[0..9]`; без `spx` → `"/"` и 10 нулей (`:214-242`) | `/abc?p=10-20&c=2&q=1` → `spiderX:"/abc?q=1"`, `spiderY:[10,20,2,2,0…]` ✓ | `abc` ✗ |
| `pqv` | reality | `realitySettings.mldsa65Verify` | base64url, ровно 1952 байта (`:209-213`) | `Verify` из `xray mldsa65` ✓ | `AAAA` ✗; tls → игнор |
| `headerType` | tcp | `none`/пусто → `tcpSettings` не пишем; `http` → `tcpSettings.header = {"type":"http","request":{"path":[…],"headers":{"Host":[…]}}}` | `tcpHeaderLoader` `none|http` (`transport_method.go:236-264`); `request.path` (по умолчанию `/`), заданные `headers` **заменяют** набор заголовков Xray по умолчанию (`:61-140`) | `tcp headerType=http host+path` (+tls, +reality, +ML-KEM) ✓ | `bogus` ✗; ws/xhttp → игнор (v2rayN пишет `headerType=none` только для raw) |
| `host` | ws, xhttp, tcp+http | `wsSettings.host` / `xhttpSettings.host` / `header.request.headers.Host` (split `,`) | ws: поле `Host`, `headers.Host` deprecated (`:627-659`); xhttp: `Host`; `headers` с `host` ✗ (`:336-340`) | ✓ | tcp без http → игнор |
| `path` | ws, xhttp, tcp+http | `wsSettings.path` (как в ссылке, `?ed=` вынимает conf) / `xhttpSettings.path` / `header.request.path` (split `,`) | ws: `?ed=N` → `Ed` (`:629-638`); пусто → Xray по умолчанию | `/ws?ed=2048` → `path:"/ws", ed:2048`; ws без path ✓ | — (ws без `path` принимать: Xray принимает) |
| `mode` | xhttp | `xhttpSettings.mode` | `""`→`auto`, `auto|packet-up|stream-up|stream-one`, иначе ✗ (`:316-334`) | все 4 ✓ | `bogus` ✗ |
| `extra` | xhttp | `xhttpSettings.extra` (JSON-объект целиком) | json → `SplitHTTPConfig`, внешние `host/path/mode` перекрывают (`:317-327`); неизвестные ключи игнорируются; `downloadSettings` (свой `StreamConfig`) поддержан | `xPaddingBytes`, `xmux`, `headers`, `noGRPCHeader`, `downloadSettings`, неизвестный ключ ✓ | `xPaddingPlacement:"nowhere"`, `headers.Host` ✗; не JSON/не объект → skip в normalize (иначе невалидный JSON-outbound) |
| `fm` | все в scope | `streamSettings.finalmask` (JSON-объект целиком: `tcp[]`, `udp[]`, `quicParams`) | маски: tcp — `header-custom`, `fragment`, `sudoku`, `xmc`; udp — `header-custom`, `mkcp-legacy`, `noise`, `salamander`, `sudoku`, `xdns`, `xicmp`, `realm`, `udphop` (`transport_finalmask.go:72-89,1106-1156`); `quicParams` — проверки `transport_internet.go:229-327` | `fragment` (tcp+tls, reality+vision), `header-custom` (ws), `sudoku`, `noise`+`quicParams` (xhttp h3) ✓ | неизвестный тип ✗; не JSON/не объект → skip в normalize |
| `allowInsecure` | tls | **не передаётся** | `allowInsecure:true` ✗ removed-error (`:362-364`) | `tls allowInsecure` ✗ | политика D15 |

### 4.1 Параметры, которые выдают генераторы, но которых нет в Xray

| Параметр | Кто выдаёт | Смысл | Поведение по умолчанию в плане |
|---|---|---|---|
| `support-x25519mlkem768=true` | 3x-ui **в каждой** REALITY-VLESS-ссылке (`x3ui:service.go:1243`) | флаг для клиентов; в Xray и в #716 его нет | принимать и игнорировать (`param_ignored` без skip) — **нужно подтверждение владельца**: по букве D26 «неизвестный → skip» это убило бы все свежие REALITY-ссылки 3x-ui |
| `x_padding_bytes` | 3x-ui для xhttp (`x3ui:service.go:2660-2679`) | дубль `extra.xPaddingBytes` для sing-box-клиентов | так же: игнорировать (значение уже есть в `extra`) — подтверждение владельца |
| `serviceName`, `authority` (grpc), `seed`, `mtu`, `tti` (kcp) | v2rayN/3x-ui для своих транспортов | параметры транспортов вне scope | в белом списке (стандарт #716): ссылка с `type=grpc|kcp` → `unsupported_combo`; на транспорте из scope → `param_ignored` |

## 5. Что из этого следует для нормализатора/билдера

1. Нормализатор проверяет только то, что **conf не может** или проверяет неудобно: scope (§1), сочетания (§3: ws+reality, none без ML-KEM), сброс vision (§3 #6/#10), D15, валидность ключей ML-KEM (единственный случай «conf ✓, live ✗»), JSON-форму `extra`/`fm`, толерантную нормализацию `pbk`, lowercase `fp`, сплит списков.
2. Всё остальное валидирует `conf.Build()` в манифесте: ошибка → skip `unsupported_value` «Xray rejected the outbound configuration.» с текстом Xray в `details` (D19, D26). Так «skip только то, что Xray отвергает» выполняется по построению, без дублирования проверок.
3. Тесты: unit-оракул — `ToProto` (conf.Build) для каждой строки корпуса; live — `AddOutbound` → `ListOutbounds` → `proto.Equal` + адресная проверка поля (по одному кейсу на каждую строку таблицы §4 и каждую ✓-строку §3).
