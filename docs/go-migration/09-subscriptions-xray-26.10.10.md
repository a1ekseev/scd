# 09. Адаптация VLESS-подписок под Xray v26.10.10

Дополняет `01-scd-domain.md` §5–§6, §11–§12, `02-scd-xray-api.md` и `08-xray-26.10.10.md`. Документ отвечает на требование владельца: «Все подписки должны адаптироваться под новую версию xray — это касается не только данного поля, а вообще всего!» Здесь только исследование: **объём работ не решён**, все пункты «нужно решение» вынесены в §8.

Ссылки: `new:<path>:<line>` — дерево Xray-core **v26.10.10** (`701af60772cd`, scratchpad `xray-core-26.10.10/`); `old:` — v26.3.27; `ts:` — `scd/src/…`; `x3ui:` — 3x-ui `608ef4e4` (2026-10-14), `internal/sub/…`; `v2rayN:` — `ServiceLib/Handler/Fmt/…` (master, 2026-10). Стандарт share-ссылок — XTLS/Xray-core discussion #716 (последние правки: `pcs`/`vcn` 2026-02-02, `mtu`/`tti` 2026-03-27).

Проверочные программы (scratchpad, модуль `x2610/` на псевдоверсии 26.10.10):
* `x2610/subs/main.go` — канонический JSON v26 → `infra/conf.OutboundDetourConfig.Build()` → печать proto, сравнение с ручной сборкой (`proto.Equal`), 13 негативных кейсов;
* `x2610/spider/main.go` — воспроизведение падения REALITY-клиента (§1.4).

> **TL;DR**
> 1. **Критично, затрагивает и текущий TS-scd.** REALITY-outbound без `spider_y` роняет **весь процесс Xray** (`panic: index out of range`), как только сервер отвечает «настоящим» сертификатом (неверный `pbk`, смена ключа, MITM, редирект DPI). Это `new:transport/internet/reality/reality.go:257,263,292,298`; в v26.3.27 код тот же. TS шлёт только поля 21–24, поэтому уязвим. Проверено: `x2610/spider` → `panic … reality.go:263`, exit 2. Go-билдер **обязан** ставить `SpiderX="/"` и `SpiderY=[10]int64{}`, как это делает `infra/conf` (`new:infra/conf/transport_security.go:214-242`).
> 2. **Строгий белый список параметров отбрасывает современные ссылки.** 3x-ui **всегда** добавляет `spx` в REALITY-ссылки (`x3ui:service.go:2010`, `deriveSpiderX` `:2025`). TS-парсер отвечает на это `unsupported_param` (`ts:subscription/parse-subscription-line.ts:4-18,82`), и вся строка пропускается. Та же судьба у `pqv`, `pcs`, `vcn`, `ech`, `fm`, `x_padding_bytes` и др.
> 3. Подмены `allowInsecure` в v26.10.10: `pinnedPeerCertSha256` (share-параметр **`pcs`**, proto `tls.Config.pinned_peer_cert_sha256=22`) и `verifyPeerCertByName` (**`vcn`**, `=17`). Оба поля есть с v26.3.27 и уже генерируются 3x-ui и v2rayN. Сам `allowInsecure=1` 3x-ui всё ещё выдаёт для external-host эндпоинтов (`x3ui:host_sub.go:322-323`), v2rayN — больше нет.
> 4. Номера полей не менялись. Поменялось то, **что делает `infra/conf`** до proto: lowercase+валидация `fp`, base64url-32 для `pbk`, hex ≤16 для `sid` с дополнением до 8 байт, разбор `spx`, вынос `?ed=` из ws-path, `Host` из `headers` → `host`, проверка `flow`, трансформация `encryption=mlkem768x25519plus.*`. Через gRPC всё это **не происходит**: scd должен либо повторить это сам (вариант A), либо вызывать `infra/conf` (вариант B, +12.2 МБ к бинарю). См. §3.4.
> 5. Хэши (`manifestHash`) живут только в памяти, поэтому любое изменение формы outbound’ов стоит один полный apply после рестарта, а рестарт при миграции и так его даёт. Теги (`sha8(raw)`) не меняются. Но ставшие валидными строки (например, с `spx`) **добавляют туннели и сдвигают порты** (§5).

---

## 1. Параметр за параметром: TS → v26.10.10

Принятые TS параметры: `type, path, security, alpn, encryption, fp, sni, flow, host, allowinsecure, headertype, pbk, sid` (`ts:subscription/parse-subscription-line.ts:4-18`). Всё остальное, **включая `spx`**, отклоняется кодом `unsupported_param`.

Обозначения статуса: **=** без изменений; **Δsem** — семантика или валидация изменилась либо скрыта в `infra/conf`; **✗** — удалено; **dep** — deprecated (только warning).

### 1.1 Таблица соответствия

| URI-параметр (TS) | Что делает TS | v26.10.10: proto (номер поля) | v26.10.10: `infra/conf` (JSON-ключ) | Статус | Рекомендуемое поведение Go |
|---|---|---|---|---|---|
| userinfo (UUID) | regex: версия 1–5, вариант 8–b (`ts:…/parse-subscription-line.ts:40-42`) | `vless.Account.id=1` (`new:proxy/vless/account.proto:18`) | `id`; `uuid.ParseString`, затем канонизация `u.String()` (`new:infra/conf/vless.go:323-327`). Принимает **любой** 32–36-символьный hex-UUID (в т.ч. v7) и строки 1–30 символов (маппинг в UUIDv5, `new:common/uuid/uuid.go:67-83`) | = (Xray мягче TS) | Оставить regex TS ради паритета. **Вопрос владельцу:** разрешать UUIDv7 (`[1-8]` в версии)? Проверено: Xray принимает `0190f5b8-1c3a-7c2e-…` (neg[12] в `x2610/subs`). В proto писать `uuid.String()` (lowercase) |
| host:port | `address` (IPv6 в скобках), `port` | `ServerEndpoint.address=1` (IPOrDomain), `port=2` | `Address.Build()` → IP или domain | **Δ (баг TS)** | `net.NewIPOrDomain(net.ParseAddress(address))` (`new:common/net/address.go:79-96`, скобки снимаются в `:82-85`). TS всегда шлёт `domain` (02 §1). Для `[2001:db8::1]` это domain **со скобками** (`IPOrDomain.AsAddress` → `DomainAddress`, `:195-196`): по коду dial к нему невозможен (живьём не проверялось). Conf даёт `ip` (проверено: `198.51.100.10` → `ip: xjNkCg==`) |
| `security` | `tls` / `reality`, иначе `unsupported_combo` | `StreamConfig.security_type=3`, `security_settings=4` | `""`/`none`/`tls`/`reality`; `xtls` → removed-error (`new:infra/conf/transport_internet.go:91-123`) | = | Без изменений. `none` по-прежнему не поддерживать: conf сам запрещает VLESS без TLS/REALITY/encryption на публичный адрес (`new:infra/conf/xray.go:242-263`, проверено neg[11]) |
| `type` | `tcp` / `ws` (lowercase), по умолчанию `tcp` | `StreamConfig.protocol_name=5` и `TransportConfig.protocol_name=3` | `raw`/`tcp`→`tcp`; `ws`/`websocket`→`websocket` **+ deprecation-warning** «migrate to XHTTP» (`new:…/transport_internet.go:16-46`, ws `:27-29`) | = / **dep (ws)** | Принять `raw` как алиас `tcp` (v2rayN знает `raw`, `v2rayN:BaseFmt.cs:100`; стандарт #716 оставляет `tcp`). В proto: `ProtocolName:"tcp"` / `"websocket"`. ws — оставить, warning в лог не нужен (это warning конфигов Xray) |
| `sni` | `serverName` | TLS `server_name=3` (`new:transport/internet/tls/config.proto:47`); REALITY `server_name=22` (`new:…/reality/config.proto:26`) | `serverName` | = | Без изменений. Пустой SNI в TLS → dial с `ServerName=адрес` (`new:transport/internet/tls/config.go:489-499`). REALITY без SNI → адрес (`new:…/reality/reality.go:154-156`). #716: «省略时复用 remote-host» — совпадает |
| `fp` | как есть, без lowercase | TLS `fingerprint=11` (`new:…/tls/config.proto:69`); REALITY `Fingerprint=21` | **lowercase** + проверка `tls.GetFingerprint` (`new:infra/conf/transport_security.go:355-358` TLS; `:180-186` REALITY, плюс запрет `unsafe`/`hellogolang`) | **Δsem** | `fp = strings.ToLower(fp)`; `tls.GetFingerprint(fp)==nil` → skip `unsupported_value`. Для REALITY ещё и `unsafe`/`hellogolang` → skip. Причина: через proto неизвестное имя (например `Chrome`) в TLS даёт **Go crypto/tls без uTLS** (`new:transport/internet/tcp/dialer.go:73-83`), а в REALITY — ошибку dial «failed to get fingerprint» (`new:…/reality/reality.go:158-161`). Набор имён: `new:transport/internet/tls/tls.go:237-311`. Против 26.3.27 **ничего не удалено**, добавлены `hellofirefox_148`, `hellochrome_133`, `hellosafari_26_3`. Пустой `fp` = `HelloChrome_Auto` (`:221-224`), а #716 даёт тот же дефолт «chrome» |
| `alpn` | split `,` + trim + без пустых | TLS `next_protocol=4` | `alpn` (StringList); единственная проверка — `fromMitm` (`new:…/transport_security.go:337-346`) | = | Без изменений. Пустой список → рантайм `["h2","http/1.1"]` (`new:…/tls/config.go:426-428`), для ws → `["http/1.1"]` (`WithNextProto`, `:508-514`; `new:…/websocket/dialer.go:73`). **Риск:** ws+`alpn=h2` ломает ws-handshake. Рекомендация — warning (не skip). Для REALITY alpn игнорируется (полей нет) — как в TS |
| `pbk` | обязателен для REALITY; `Buffer.from(…,'base64')` (лениво: std/url, с паддингом и без, мусор отбрасывается) (`ts:builders/build-outbound-grpc.ts:14-19`) | REALITY `public_key=23` | `password` (алиас `publicKey`, `password` приоритетнее) → `base64.RawURLEncoding`, **ровно 32 байта**, иначе ошибка (`new:…/transport_security.go:190-198`) | **Δsem** | Декодировать base64url без паддинга. Рекомендуется толерантно: убрать `=`, заменить `+/`→`-_`. Длина ≠32 → skip `unsupported_value` (`REALITY pbk must be 32-byte base64url`). Сейчас TS отправляет мусор, и outbound «молча» не работает. В JSON — ключ `password` (#716: «pbk … 对应 `password`») или `publicKey` (оба валидны) |
| `sid` | обязателен (непустой); `Buffer.from(…,'hex')`, обрезается на первом невалидном символе | REALITY `short_id=24` | hex, `len≤16`, чётная длина; результат **дополняется нулями до 8 байт** (`new:…/transport_security.go:202-208`) | **Δsem** | Валидировать: `^[0-9a-fA-F]{0,16}$` и чётная длина, иначе skip `unsupported_value`. Байты — `make([]byte,8)` + `hex.Decode` (как conf). На проводе это отличается от TS (1–8 байт), но рантайм эквивалентен: `copy(SessionId[8:], ShortId)` (`new:…/reality/reality.go:173`). **Пустой `sid` легален** (#716 «此项可能为空字符串»; conf даёт 8 нулей), а TS его отвергает (`ts:normalize/normalize-vless.ts:122`). Рекомендуется адаптировать: разрешить пустой |
| `spx` | **не принимается → строка skip** | REALITY `spider_x=26`, `spider_y=27` (repeated int64, ровно 10) | `spiderX`: пусто → `/`; должен начинаться с `/`; из query вынимаются `p,c,t,i,r` (`a` или `a-b`) в `SpiderY[0..9]`, остаток — в `SpiderX` (`new:…/transport_security.go:214-242`) | **обяз.** | Принимать `spx`. Повторить conf-разбор 1:1 (код 25 строк). При отсутствии `spx`: `SpiderX="/"`, `SpiderY=make([]int64,10)` — **обязательно**, см. §1.4. `spx` не с `/` → skip `unsupported_value` |
| `pqv` | не принимается → skip | REALITY `mldsa65_verify=25` | `mldsa65Verify`: base64url, ровно 1952 байта (`new:…/transport_security.go:209-213`) | новое (с 25.7) | Принимать. Декодировать, проверять длину, иначе skip. Пустая строка — поле не задавать. Без поддержки `pqv` нельзя просто игнорировать параметр: это ослабит проверку сервера. Значит либо поддержка, либо skip |
| `flow` | as-is, регистр важен; для tcp-tls сохраняется; **для ws-tls тоже сохраняется** | `vless.Account.flow=2` | допустимы только `""`, `xtls-rprx-vision`, `xtls-rprx-vision-udp443`, иначе ошибка (`new:infra/conf/vless.go:329-334`) | **Δsem** | Проверять множество значений, иначе skip `unsupported_value`: через proto Xray принимает любой flow при `AddOutbound` (08 §3, `xtls-rprx-bogus` OK), но ломается на трафике. **ws+tls+vision**: conf принимает (neg[6]), но рантайм отвечает «XTLS only supports TLS and REALITY directly for now» (`new:proxy/vless/outbound/outbound.go:272-283`). 3x-ui выдаёт flow только для tcp+tls/reality (`x3ui:service.go:1205-1212`). Рекомендация: для ws отбрасывать flow с warning или skip — **решение владельца**. `visionUdp443Override` (`ts:targets/override-outbound-for-target.ts`) по-прежнему валиден (`new:…/outbound.go:249-253`) |
| `encryption` | `""`/`none`, иначе skip | `vless.Account.encryption=3`, **`xorMode=4`, `seconds=5`, `padding=6`** (`new:proxy/vless/account.proto:22-25`) | `none` **обязателен явно** (пустое → ошибка «please add/set "encryption":"none"», neg[7]); `mlkem768x25519plus.<native\|xorpub\|random>.<1rtt\|0rtt>.[padding…].<keys…>` разбирается и **перекладывается** в `xorMode/seconds/padding` с отрезанием префикса (`new:infra/conf/vless.go:336-378`) | **Δsem / новое** | В proto всегда `Encryption:"none"` (как TS). `mlkem768x25519plus.*` — опциональное расширение (§4). Если его поддерживать вручную, нужно точно повторить трансформацию `:336-378`: рантайм ждёт **уже разобранную** строку (`new:proxy/vless/outbound/outbound.go:86-98`). Передать raw-строку из URI нельзя |
| `path` (ws) | обязателен для ws (непустой); в JSON `path ?? "/"` | `websocket.Config.path=2`, **`ed=5`** | `?ed=N` вынимается из path в `ed`, остаток path сохраняется (`new:infra/conf/transport_method.go:628-638`) | **Δsem** | Повторить: `url.Parse(path)` → если `ed` непуст, `Ed=atoi`, удалить `ed` из query. Проверено: `/stream/updates/case-a?ed=2048` → `path:"/stream/updates/case-a", ed:2048`. TS шлёт `?ed=2048` в path буквально: на сервер уходит путь с query, а early data не включается. Требование «path обязателен» оставить (паритет). Для tcp path игнорировать, как TS |
| `host` (ws) | `wsSettings.headers.Host`; в proto TS — **и** `host=1`, **и** `header["Host"]` | `websocket.Config.host=1` | `headers.Host` → **deprecated**, переносится в `host` (`new:…/transport_method.go:639-648`) | **Δsem** | Писать только `Host` (поле 1), `Header` не заполнять. Рантайм всё равно перетирает `Host` из map значением `wsSettings.Host` (`new:transport/internet/websocket/dialer.go:134-142`). JSON-форма: `"wsSettings":{"path":…, "host":…}` |
| `headerType` | `""`/`none`, иначе skip | `tcp.Config.header_settings` | `none`/`http` (`new:…/transport_method.go:236-264`) | = | Без изменений (`http` — опциональное расширение, §4) |
| `allowInsecure` | `0/1/true/false`, иначе skip; кладётся в TLS поле 1 | **✗** поле 1 удалено (`new:…/tls/config.proto:41`) | `true` → `PrintRemovedFeatureError(... "pinnedPeerCertSha256"(pcs) and "verifyPeerCertByName"(vcn))` (`new:…/transport_security.go:362-364`, neg[2]) | **✗** | См. §2 |
| — (TS ставит) `User.level=1` | 0 | `User.level=1`, `email=2` | — | = | `Level:0`, `Email:tag`, как TS (`ts:builders/build-outbound-grpc.ts:47-53`) |

### 1.2 Поля, которые TS не шлёт, а v26.10.10 ожидает или подставляет через conf

| Поле | Значение из conf | Нужно ли в Go |
|---|---|---|
| `reality.Config.spider_x/spider_y` | `"/"` и 10 нулей (или из `spx`) | **Да, обязательно** (§1.4) |
| `reality.Config.short_id` | 8 байт | Да (см. `sid`) |
| `tls.Config.use_system_ca=23` | `false` | Нет (встроенный Mozilla-bundle, 08 §2.2). Решение по CA — 08 §8.3 |
| `websocket.Config.ed=5` | из `?ed=` | Да |
| `websocket.Config.heartbeatPeriod=6` | 0 | Нет |
| `SenderConfig.target_strategy` | `AS_IS` (=0) | Нет (нулевое значение) |

### 1.3 Профильная логика (`determineProfile`, `ts:normalize/normalize-vless.ts:53-68`)

* `tcp+tls`, `ws+tls`, `tcp+reality+vision` в v26.10.10 валидны.
* REALITY допускается conf только на `tcp`, `xhttp`(splithttp) и `grpc` (`new:infra/conf/transport_internet.go:105-108`, neg[5]). Профиль `tcp+reality` **без** vision Xray тоже принимает; TS его отвергает (`unsupported_combo`). Расширять или нет — §4.
* `tcp+tls+flow=vision` (TS сохраняет flow) работает только при TLS 1.3 у сервера (`new:proxy/vless/outbound/outbound.go:356-366`). Это не изменилось.

### 1.4 Найдено: падение Xray при REALITY без `spider_y` (касается текущего TS-scd)

* Код: при `!uConn.Verified` (сервер прислал не REALITY-сертификат) клиент запускает «паука», который индексирует `config.SpiderY[0..9]` (`new:transport/internet/reality/reality.go:208-298`). Индексация идёт без проверки длины: `:257`, `:263`, `:287`, `:292` в горутине, `:298` в самом dial. `recover()` в этих путях нет (единственные в `proxy/`, `app/`, `transport/` — `app/dns/cache_controller.go:179`, `common/reflect/marshal.go:132`, `proxy/vless/outbound/outbound.go:164`).
* `infra/conf` всегда ставит `SpiderY = make([]int64, 10)` (`new:infra/conf/transport_security.go:220`). Значит, JSON-конфиги Xray не страдают, а **gRPC-клиенты, собирающие `reality.Config` вручную, страдают**.
* Воспроизведение (`x2610/spider/main.go`): httptest TLS-сервер, CA которого доверен через `SSL_CERT_FILE`, `reality.UClient` со случайным `PublicKey`:
  ```
  [Error] transport/internet/reality: REALITY: received real certificate (potential MITM or redirection)
  panic: runtime error: index out of range [0] with length 0
  …/transport/internet/reality/reality.go:263   exit=2
  ```
  С `SpiderX="/"`, `SpiderY=make([]int64,10)` получаем штатную ошибку `REALITY: processed invalid connection`, exit 0.
* Код spider в v26.3.27 тот же (diff `reality.go` — только CloseNotify и уровни логов). Значит, **текущий прод TS-scd уязвим**: достаточно одного outbound’а с устаревшим `pbk`, на который попадёт трафик (в том числе probe мониторинга), чтобы упал весь Xray со всеми туннелями.
* Go: обязательная часть билдера REALITY. Для TS-ветки — кандидат на hotfix до миграции (решение владельца; код в этом исследовании не менялся).

---

## 2. `allowInsecure` и его замена

### 2.1 Что есть в v26.10.10

| Механизм | proto | JSON | share-параметр (#716 §4.4.4) | Семантика (`new:transport/internet/tls/config.go`) |
|---|---|---|---|---|
| ~~allowInsecure~~ | ~~1~~ (удалено, `config.proto:41`) | `allowInsecure:true` → removed-error | ~~`allowInsecure`~~ («没有过 `allowInsecure`，且 Xray-core 已移除该配置项，请使用以上两项代替») | — |
| pinnedPeerCertSha256 | `repeated bytes pinned_peer_cert_sha256 = 22` (`config.proto:88`) | строка, через `,`; hex, `:` допускаются (формат OpenSSL); каждый ровно 32 байта (`new:infra/conf/transport_security.go:365-381`) | **`pcs`** — «TLS CA/leaf 证书 SHA-256 指纹», запятые разделяют значения | `InsecureSkipVerify=true` (`:401-405`), затем `verifyPeerCert`: совпал leaf → OK; совпал CA → CA становится единственным корнем, и проверяется цепочка по `ServerName` (пустой SNI → ошибка «Pinning CA needs a valid ServerName», `:334-351`) (`:284-354`) |
| verifyPeerCertByName | `repeated string verify_peer_cert_by_name = 17` (`config.proto:78`) | строка, через `,`, trim (`:382-390`) | **`vcn`** — «TLS 用于验证 leaf 证书的 name» | `InsecureSkipVerify=true` (`:396-400`). Цепочка проверяется по root CAs (или pinned CA), а имя — по **любому** из `vcn` вместо SNI (`:314-332`). Это вариант «SNI-фронтинг с честной проверкой» |
| useSystemCA | `use_system_ca = 23` | `useSystemCA` | нет | Корни — встроенный bundle или системные (08 §2.2) |

Оба поля (17, 22) существуют и в v26.3.27 (`old:transport/internet/tls/config.proto:78,88`), поэтому их поддержка не зависит от целевой версии.

### 2.2 Что делают клиенты и панели

* **v2rayN** разбирает `pcs`, `vcn`, `ech`, `pqv`, `spx`, `fm` (`v2rayN:BaseFmt.cs:201-213`) и генерирует их (`:37-90`). `allowInsecure` не читает и не пишет (grep пуст).
* **3x-ui** генерирует `pcs` (`strings.Join(pins, ",")`), `vcn`, `ech`, `fp` (`x3ui:service.go:1848-1863`). **Но** для external-host эндпоинтов с флагом «allowInsecure» всё ещё пишет `allowInsecure=1` (`x3ui:host_sub.go:316-325`), а для tuic — `allow_insecure=1` (`x3ui:service.go:910-911`). Значит, `allowInsecure=1` в подписках будет встречаться ещё долго.

### 2.3 Рекомендуемое поведение Go

1. Добавить в белый список `pcs`, `vcn` и маппить их так же, как conf (`:365-390`): `pcs` — split `,`, trim, удалить `:`, `hex.DecodeString`, длина 32, иначе skip `unsupported_value` (`pcs must be comma-separated SHA-256 hex`); `vcn` — split `,`, trim, пустые элементы пропускать.
2. `allowInsecure`: как и раньше, разбирать с той же валидацией (`ts:normalize/normalize-vless.ts:135-146`, тексты ошибок не менять). **В proto не передавать.**
   * `allowInsecure=1` **и** есть `pcs`/`vcn` → проверку задают они, warning не нужен.
   * `allowInsecure=1` без `pcs`/`vcn` → warning `allowInsecure is not supported by Xray ≥ v26.6.22; outbound will verify certificates (use pcs/vcn)`. Outbound **создаётся**: если сертификат валиден, он работает.
   * Альтернатива (решение владельца): skip `unsupported_value`, как делает conf. Минус — потеря строк, которые с валидным сертификатом работали бы.
3. Из JSON-представления `allowInsecure` **убрать**. `tlsSettings.allowInsecure:true` — невалидный для v26 конфиг (removed-error). Значение сохранить в `normalized.allowInsecure` (отчёты и generate) и, если нужно, в отдельном диагностическом поле.
4. Автоматически «эмулировать» insecure (например, pin сертификата, полученного при первом подключении) **не рекомендуется**: это TOFU, и Xray сознательно убрал такую возможность.

---

## 3. Есть ли «эталонный» парсер ссылок и форма, которую стоит зеркалить

### 3.1 Xray-core

* В v26.10.10 **нет парсера `vless://`**: `grep -rn "vless://" --include=*.go` пуст. Стандарт #716 описан только в discussion, код Xray его не реализует.
* Канонический маппинг «JSON → proto» — `infra/conf`: `OutboundDetourConfig.Build()` (`new:infra/conf/xray.go:266-…`) → `StreamConfig.Build()` (`new:…/transport_internet.go:72-331`) → `TLSConfig/REALITYConfig.Build()` (`new:…/transport_security.go:54-410`), `WebSocketConfig/SplitHTTPConfig/GRPCConfig/TCPConfig.Build()` (`new:…/transport_method.go`), `VLessOutboundConfig.Build()` (`new:infra/conf/vless.go:261-389`). Зеркалить нужно именно его: share-параметры #716 определены как «соответствует ключу X конфиг-файла».
* JSON-ключ транспорта в v26 переименован `network` → `method`. `network` по-прежнему принимается (`new:…/transport_internet.go:80-82`, коммит `fb548f54`), поэтому в нашем JSON можно оставить `network`.

### 3.2 Стандарт #716: параметры и дефолты (сжато)

| Параметр | Назначение / JSON-ключ | Дефолт / пустое |
|---|---|---|
| `type` | `tcp,kcp,ws,http,grpc,httpupgrade,xhttp` («tcp 一时半会儿改不成 raw») | — |
| `encryption` | VLESS: `none` или `mlkem768x25519...` (ред. 2026-01-18) | `none`; пустое недопустимо |
| `flow` | `xtls-rprx-vision` и т.п. (ред. 2026-01-18) | может быть пустым |
| `security` | `none,tls,reality` | `none` |
| `path`, `host` | ws/httpupgrade/xhttp | path → `/` |
| `serviceName`, `authority`, `mode` (grpc) | `mode`: `gun` / `multi` / `guna` | `gun` |
| `mode`, `extra` (xhttp) | `extra` — JSON целиком, url-encoded | — |
| `seed`, `headerType`, `mtu`, `tti` (kcp) | `mtu`/`tti` добавлены 2026-03-27 | — |
| `fm` | `finalmask` — JSON целиком (добавлен 2026-01-31) | — |
| `fp` | отпечаток uTLS | `chrome`; для REALITY обязателен |
| `sni` | | адрес |
| `alpn` | через `,` | решает ядро |
| `ech` | `echConfigList` (2025-07-26) | может быть пустым |
| `pcs`, `vcn` | §2 (2026-01-18 / 02-02) | могут быть пустыми |
| `pbk` | REALITY `password` | обязателен |
| `sid` | | **может быть пустым** |
| `pqv` | REALITY `mldsa65Verify` (2025-07-22) | может быть пустым |
| `spx` | REALITY `spiderX` | может быть пустым |

Сам стандарт предупреждает, что breaking-изменения вероятны. `headerType=http` для tcp формально не стандартизован, но 3x-ui его генерирует (`x3ui:service.go:1730-1747`).

### 3.3 Отличия популярных генераторов, важные для нас

* 3x-ui: `spx` **всегда** у REALITY (`deriveSpiderX` → `/` + 15 hex, `x3ui:service.go:2025-2035`); `sid` берёт случайный из `shortIds`; `x_padding_bytes` + `extra` у xhttp (`:2660-2679`; `x_padding_bytes` нестандартный); `mode=multi` у grpc (`:1758-1760`); `encryption` всегда выставлен (`:1198-1203`).
* v2rayN: `headerType=none` всегда для raw (`BaseFmt.cs:105`); `encryption=none` всегда (`VLESSFmt.cs:51-52`). Пустые значения не пишет.

### 3.4 Архитектурный выбор для Go-билдера (нужно решение)

| | **A. Ручная сборка proto (текущий D2) + свой порт conf-нормализации** | **B. Канонический JSON → `infra/conf` → proto** |
|---|---|---|
| Как | normalizer повторяет: lowercase/валидацию `fp`, `pbk`, `sid`, `spx`→SpiderY, `?ed=`, flow-set, (mlkem); билдер собирает `tls/reality/websocket.Config` | Строим `jsonOutbound` в форме v26 (тот, что хэшируется), затем `json.Unmarshal` → `conf.OutboundDetourConfig.Build()` → `AddOutbound` |
| Совпадение с Xray | Нужно поддерживать вручную. Тест-оракул: `proto.Equal(ручной, conf.Build(json))` (в `x2610/subs` для ws-tls уже `true`) | Совпадает по построению, включая будущие правки Xray |
| Новые фичи (xhttp `extra`, `fm`, mlkem, grpc) | Каждую нужно переносить (xhttp — ~200 строк дефолтов, `transport_method.go:316-519`) | Бесплатно: JSON-ключи 1:1 с #716 |
| Размер бинаря (linux/amd64, `-s -w`) | 22.5 МБ (08 §4) | **34.7 МБ** (+12.2; 145 пакетов xray-core против 80). Замер: `x2610/bin_subs_stripped` |
| Побочные эффекты | нет | conf пишет warning’и в логгер Xray (ws deprecated и т.п.). Тексты ошибок — Xray’евские (`infra/conf: …`), нужен маппинг в наши `reasonCode`. API `infra/conf` нестабилен (08 §8.1) |
| Валидация на этапе normalize | своя | можно вызвать `Build()` прямо в normalize: ошибка → skip `unsupported_value` с текстом Xray в `details` |

Рекомендация исследования: **A для трёх текущих профилей + conf как тестовый оракул** (golden-тест «каждая фикстура: ручной proto == conf.Build(jsonOutbound)»). **B**, если владелец включит в scope xhttp, finalmask или VLESS-encryption: тогда ручной порт дороже 12 МБ.

---

## 4. Что ещё может прийти в подписках (сейчас отбрасывается) — флаги для владельца

«Обязательно» — без этого реально встречающиеся ссылки на уже поддерживаемых профилях либо теряются, либо работают неверно или опасно. «Расширение» — новый профиль или фича, scope решает владелец.

| # | Параметр(ы) | Где в Xray | Класс | Комментарий |
|---|---|---|---|---|
| 1 | `spx` (REALITY) | `reality.spider_x/y` | **Обязательно** | 3x-ui выдаёт всегда. Плюс обязательные дефолты SpiderY (§1.4) |
| 2 | `pqv` (REALITY) | `reality.mldsa65_verify=25` | **Обязательно** (при наличии: поддержать или skip) | 3x-ui выдаёт, если на сервере `mldsa65Seed`. Простое bytes-поле |
| 3 | `pcs`, `vcn` | `tls.pinned_peer_cert_sha256=22`, `verify_peer_cert_by_name=17` | **Обязательно** | Замена `allowInsecure` (§2) |
| 4 | `allowInsecure` | — | **Обязательно** (решить политику) | §2.3 |
| 5 | `fp` lowercase/валидация, `pbk`/`sid` строгий формат, пустой `sid` | §1.1 | **Обязательно** | Иначе «тихо мёртвые» outbound’ы |
| 6 | `?ed=` в ws-path, ws `host` в поле 1 | §1.1 | **Обязательно** | Сейчас early data теряется, и path уходит с query |
| 7 | flow на ws | §1.1 | **Обязательно** (решить: drop или skip) | Иначе outbound падает на каждом соединении |
| 8 | `type=raw` | алиас tcp | Желательно | Дёшево |
| 9 | `ech` | `tls.ech_config_list=19` (string) + `ech_socket_settings=21` | Расширение; **если не поддерживать — skip, а не игнор** (игнор раскроет SNI) | conf: `new:…/transport_security.go:399`; логика `ApplyECH` (`new:…/tls/config.go:476-481`). Значение может быть base64 ECHConfigList или DNS-запросом — нужно отдельное исследование |
| 10 | `type=xhttp`: `path`, `host`, `mode` (`auto/packet-up/stream-up/stream-one`), `extra` (JSON), 3x-ui `x_padding_bytes` | `splithttp.Config` (`new:transport/internet/splithttp/config.proto:25-…`), conf `transport_method.go:266-519` | Расширение (новые профили `xhttp-tls`, `xhttp-reality`) | Xray сам советует уходить с ws на XHTTP (deprecation-warning). Вручную — много дефолтов (xmux 3/600-900/1800-3000, padding и др.), поэтому лучше вариант B |
| 11 | `type=grpc`: `serviceName`, `authority`, `mode=multi` | `grpc.Config` (`new:transport/internet/grpc/config.proto:6-15`) | Расширение | Помечен deprecated (`transport_internet.go:24-26`); REALITY допускается |
| 12 | `type=httpupgrade`: `path`, `host` | `httpupgrade.Config` | Расширение | deprecated; `headers.Host` запрещён (`transport_method.go:681-685`) |
| 13 | `encryption=mlkem768x25519plus.*` (VLESS Encryption) | `Account.encryption/xorMode/seconds/padding` | Расширение | Нужна трансформация conf (`vless.go:336-378`). С ней vision допустим на любом транспорте (`x3ui:service.go:1205-1212`) |
| 14 | `fm` (finalmask JSON) | `StreamConfig.tcpmasks=11/udpmasks=10/quic_params=12` | Расширение | Структура сложная, многоуровневая. Реально только через conf (вариант B). Неподдержанный `fm` — **skip** (без маски соединение не установится) |
| 15 | `headerType=http` (tcp) + `host`/`path` | `tcp.Config.header_settings` (http.Config) | Расширение | Редко; не стандартизован |
| 16 | `type=kcp` (`seed`, `headerType`, `mtu`, `tti`) | mkcp | Расширение, маловероятно | — |
| 17 | `security=reality` без vision, `security=tls`+ws+`flow` | профили | Расширение | Xray разрешает tcp+reality без flow |
| 18 | UUIDv7 и не-UUID id | `uuid.ParseString` | Расширение | §1.1 |
| 19 | Другие схемы (`hysteria2://`, `trojan://`, `ss://`, `vmess://`) | — | Вне VLESS | Не рассматривались |

Общий принцип для белого списка (рекомендация). Неизвестные параметры, которые влияют на **безопасность или установку соединения** (`ech`, `fm`, `pqv`, `pcs`, `vcn`, `extra`, `encryption`), — skip с `unsupported_param`, как сейчас, пока не поддержаны. Чисто информационные или безопасные (`spx`, `headerType=none`, `mode` для неподдерживаемого `type` — всё равно skip по combo) — принимать. Ослаблять текущее правило «неизвестный → skip» без разбора **не рекомендуется**.

---

## 5. Хэши, теги, стабильность манифеста

* **Теги** `vless-<cc>-<city>-<profile>-<sha8(raw)>` (`ts:normalize/normalize-vless.ts:70-80`) от формы outbound’а не зависят. Если не переименовывать профили, у существующих строк теги после миграции **те же**. Новые профили (xhttp и др.) получат новые имена, старые не затронуты.
* **`manifestHash`** outbound = `sha256(JSON(entries.map(jsonOutbound)))` (02 §4, `ts:apply/outbound-applicator.ts:18-22`) хранится **только в памяти** (`03-scd-runtime.md` §RS). Go-процесс стартует с пустым состоянием, и первый sync всё равно делает полный apply: удаление сгенерированных и повторный Add. Поэтому смена формы `jsonOutbound` (убрать `allowInsecure`, `headers.Host`→`host`, lowercase `fp`, добавить `spiderX`/`password`/`pcs`…) стоит **ровно один полный apply, который и так произошёл бы**. Приемлемо, дополнительных действий не нужно. Remote push / `/api/runtime-state` отдают только теги, не `jsonOutbound` (03 §API), поэтому JSON-API-контракт (D3) не затронут.
* **Где форма видна снаружи:** `scd generate` печатает `normalized` и `jsonOutbound`. Golden-файлы этапа 0 (00 §План) для этих полей **разойдутся** намеренно. В golden-сравнении эти поля нужно исключить или зафиксировать новую форму как ожидаемую.
* **Побочный эффект адаптации:** строки, которые раньше отбрасывались (`spx`, пустой `sid`, `pcs`…), станут entries. Топология сортируется по тегу, порт = `portRange.start + index` (01 §12.2), поэтому **новые туннели сдвигают порты всех туннелей с бо́льшим тегом**, и `portRange` может переполниться (`… can allocate only N ports`). Это обычное поведение при изменении подписки, но после миграции оно случится разом. Рекомендация: предупредить в release notes и проверить ёмкость `portRange` на реальных подписках.
* **Обратное:** строки, которые раньше проходили, а теперь будут skip (невалидный `pbk`/`sid`/`fp`, ws+vision при выборе skip), исчезнут. Их outbound’ы и так были нерабочими или опасными.

---

## 6. Эталонные Go-структуры для v26.10.10

Все три примера получены через `infra/conf.OutboundDetourConfig.Build()` из JSON в форме v26 (`x2610/subs/main.go`). Ручная сборка ws-tls совпала с conf: `proto.Equal == true`. Импорты: `core "github.com/xtls/xray-core/core"`, `app/proxyman`, `common/net`, `common/protocol`, `common/serial`, `proxy/vless`, `proxy/vless/outbound`, `transport/internet`, `transport/internet/{tls,reality,websocket}`.

Общая часть (одинакова для всех профилей):
```go
func vlessHandler(tag, address string, port uint32, uuidStr, flow string, ss *internet.StreamConfig) *core.OutboundHandlerConfig {
	return &core.OutboundHandlerConfig{
		Tag:            tag,
		SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{StreamSettings: ss}),
		ProxySettings: serial.ToTypedMessage(&outbound.Config{Vnext: &protocol.ServerEndpoint{
			Address: net.NewIPOrDomain(net.ParseAddress(address)), // IP → ip, "[v6]" → ip, иначе domain
			Port:    port,
			User: &protocol.User{
				Level: 0,
				Email: tag, // как TS (build-outbound-grpc.ts:50)
				Account: serial.ToTypedMessage(&vless.Account{
					Id:         uuidStr,          // canonical lowercase
					Flow:       flow,             // "", xtls-rprx-vision, xtls-rprx-vision-udp443
					Encryption: "none",
				}),
			},
		}}),
	}
}
```

### 6.1 tcp+tls
Строка фикстуры: `vless://<uuid>@at1.example.test:8443?type=tcp&path=%2F&security=tls&alpn=h2` (`path` для tcp игнорируется; `sni`/`fp` отсутствуют).
```go
tlsCfg := &tls.Config{
	ServerName:   "",            // sni отсутствует → dial подставит адрес
	NextProtocol: []string{"h2"},
	Fingerprint:  "",            // = HelloChrome_Auto; при fp=… — lowercase, проверено GetFingerprint
	// PinnedPeerCertSha256: из pcs, VerifyPeerCertByName: из vcn (если есть)
	// AllowInsecure: поля нет (compile error)
}
ss := &internet.StreamConfig{
	ProtocolName:     "tcp",
	SecurityType:     serial.GetMessageType(tlsCfg), // "xray.transport.internet.tls.Config"
	SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(tlsCfg)},
	// TransportSettings: nil (conf не создаёт tcp.Config без headerType)
}
```
Вывод conf для того же входа с `sni=at1.example.test&fp=chrome`: `tls {serverName:"at1.example.test", nextProtocol:["h2"], fingerprint:"chrome"}`, `streamSettings.protocolName:"tcp"`, без transportSettings.

JSON (`jsonOutbound`, форма v26):
```json
{"tag":"…","protocol":"vless",
 "settings":{"vnext":[{"address":"at1.example.test","port":8443,"users":[{"id":"…","encryption":"none"}]}]},
 "streamSettings":{"network":"tcp","security":"tls","tlsSettings":{"alpn":["h2"]}}}
```

### 6.2 ws+tls
Строка: `…@198.51.100.10:443?encryption=none&type=ws&fp=chrome&sni=cdn.example.test&flow=&host=cdn-1.example.test&path=%2Fstream%2Fupdates%2Fcase-a%3Fed%3D2048&security=tls&allowInsecure=1` (`?ed=2048` добавлен для иллюстрации).
```go
wsCfg := &websocket.Config{
	Host: "cdn-1.example.test",     // поле 1; Header["Host"] НЕ ставить
	Path: "/stream/updates/case-a", // ?ed вынут
	Ed:   2048,
}
tlsCfg := &tls.Config{ServerName: "cdn.example.test", Fingerprint: "chrome"} // allowInsecure=1 → только warning
ss := &internet.StreamConfig{
	ProtocolName: "websocket",
	TransportSettings: []*internet.TransportConfig{{
		ProtocolName: "websocket",
		Settings:     serial.ToTypedMessage(wsCfg),
	}},
	SecurityType:     serial.GetMessageType(tlsCfg),
	SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(tlsCfg)},
}
// flow: "" (непустой flow на ws → drop/skip, §1.1)
```
Вывод conf (проверено): `transport websocket {host:"cdn-1.example.test", path:"/stream/updates/case-a", ed:2048}`, `tls {serverName:"cdn.example.test", fingerprint:"chrome"}`, `vnext.address.ip = 198.51.100.10`. В лог при этом пишется `[Warning] … WebSocket transport … is deprecated … migrate to XHTTP`.

JSON: `"streamSettings":{"network":"ws","security":"tls","tlsSettings":{"serverName":"cdn.example.test","fingerprint":"chrome"},"wsSettings":{"path":"/stream/updates/case-a?ed=2048","host":"cdn-1.example.test"}}`. В JSON path хранится как в ссылке: conf сам вынимает `ed`.

### 6.3 tcp+reality+vision
Строка: `…@203.0.113.20:443?security=reality&encryption=none&fp=chrome&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=io.example.test&pbk=CMkW1axrhEXoiJ6anMz9XEjlfqlAtEZya7L0b5ZPMyw&sid=abe4a59b9f2407e3`.
```go
pbk, _ := base64.RawURLEncoding.DecodeString("CMkW1axrhEXoiJ6anMz9XEjlfqlAtEZya7L0b5ZPMyw") // len==32 обязательно
sid := make([]byte, 8)
_, _ = hex.Decode(sid, []byte("abe4a59b9f2407e3")) // len≤16, чётная; короче → дополнено нулями
rCfg := &reality.Config{
	Fingerprint: "chrome",           // lowercase; не unsafe/hellogolang; GetFingerprint != nil
	ServerName:  "io.example.test",
	PublicKey:   pbk,
	ShortId:     sid,
	SpiderX:     "/",                // ОБЯЗАТЕЛЬНО (или из spx)
	SpiderY:     make([]int64, 10),  // ОБЯЗАТЕЛЬНО, иначе panic в Xray (§1.4)
	// Mldsa65Verify: из pqv (1952 байта), если есть
}
ss := &internet.StreamConfig{
	ProtocolName:     "tcp",
	SecurityType:     serial.GetMessageType(rCfg), // "xray.transport.internet.reality.Config"
	SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(rCfg)},
}
// flow: "xtls-rprx-vision" (или "-udp443" при visionUdp443Override)
```
Вывод conf (проверено): `{Fingerprint:"chrome", serverName:"io.example.test", publicKey:<32B>, shortId:"q+Slm58kB+M=" (8B), spiderX:"/", spiderY:[0×10]}`, account `{id, flow:"xtls-rprx-vision", encryption:"none"}`.

`spx` (пример `/abc?p=10-20&c=2`) разбирается как в conf: `SpiderY[0..1]=10,20` (p), `[2..3]=2,2` (c), остальные 0, `SpiderX="/abc"` (query без p/c/t/i/r, через `q.Encode()`). Портировать 1:1 по `new:infra/conf/transport_security.go:214-242`.

JSON: `"realitySettings":{"serverName":"io.example.test","fingerprint":"chrome","password":"CMkW…Myw","shortId":"abe4a59b9f2407e3","spiderX":"/"}` (+ `"mldsa65Verify"` при `pqv`).

---

## 7. Итоговые изменения normalizer/builder (Go) против TS

1. Белый список параметров: + `spx`, `pqv`, `pcs`, `vcn`. Плюс по решению §4: `ech`, `fm`, `extra`, `mode`, `serviceName`, `authority`, `seed`, `mtu`, `tti`, `x_padding_bytes`. Сообщение `unsupported_param` и порядок ключей — без изменений (01 §5.3).
2. `type`: принять `raw` ≡ `tcp`.
3. `fp`: lowercase + `tls.GetFingerprint`; REALITY: ещё запрет `unsafe`/`hellogolang`.
4. `pbk`: base64url → 32 байта; `sid`: hex ≤16, чётная, допускается пустой; `pqv`: base64url → 1952 байта; ошибки → `unsupported_value`.
5. REALITY: всегда `SpiderX`/`SpiderY` (из `spx` или дефолт).
6. `flow`: множество `{"", xtls-rprx-vision, xtls-rprx-vision-udp443}`. Для ws — drop или skip (решение).
7. ws: `Host` только в поле 1; `?ed=` → `Ed`.
8. TLS: `pcs`/`vcn` → поля 22/17; `allowInsecure` — разбирать, не передавать, warning (§2.3).
9. Адрес: `net.ParseAddress` (IP → `ip`, IPv6 без скобок).
10. `jsonOutbound` — в форме, валидной для v26 `infra/conf` (без `allowInsecure`, `wsSettings.host`, `realitySettings.spiderX`, lowercase `fp`). Тест-оракул: `conf.Build(jsonOutbound)` proto-равен ручному proto.
11. Тесты: фикстуры `test/fixtures/vpn-sample.txt` + ссылки 3x-ui/v2rayN с `spx`/`pqv`/`pcs`/`vcn`/`ed`/`allowInsecure=1`/пустым `sid`/`fp=Chrome`/битым `pbk`; негативные кейсы из `x2610/subs` (neg[0..12]); регресс-тест на `SpiderY` (`len==10`) для каждого REALITY-outbound.

## 8. Решения для владельца

1. **Hotfix TS-scd по §1.4** (падение Xray от REALITY без spider_y) до завершения миграции — да/нет.
2. Scope расширений §4 (#9–#18): какие профили и транспорты добавлять (xhttp, grpc, VLESS-encryption, finalmask, ech, reality без vision).
3. Архитектура билдера: A (ручной proto + оракул) или B (`infra/conf`, +12.2 МБ) (§3.4). При заметном расширении scope рекомендуется B.
4. Политика `allowInsecure=1` без `pcs`/`vcn`: warning и создать outbound (рекомендуется) или skip (§2.3).
5. `flow` на ws: drop с warning (рекомендуется: так делает 3x-ui) или skip.
6. Пустой `sid` разрешить (рекомендуется, по стандарту) — это отличие от TS.
7. UUIDv7 / не-UUID id: оставить строгий regex TS (рекомендуется для паритета) или следовать Xray.
8. Принять одноразовый сдвиг портов туннелей после миграции (§5) и описать его в release notes.
