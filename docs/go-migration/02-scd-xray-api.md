# 02. scd ↔ Xray gRPC API: инвентаризация и план переноса на Go

Объект исследования: `scd` (TypeScript, ветка `move-to-go`) и Xray-core **v26.3.27** (`ghcr.io/xtls/xray-core:26.3.27`, `docker-compose.yml:3`).
Исходники Xray-core проверены по тегу `v26.3.27` (commit `d2758a023cd7`, 2026-03-27). Ссылки вида `xray:<path>:<line>` относятся к этому дереву.

> TL;DR
> * scd использует **9 unary RPC** двух сервисов (`HandlerService`, `RoutingService`) через самописный HTTP/2 + protobuf-кодер, **новое HTTP/2-соединение на каждый вызов**.
> * В ручном кодере найдены **два реальных расхождения с .proto v26.3.27** (подтверждено декодированием байтов, сгенерированных TS-билдерами, через Go-типы xray-core):
>   1. `TransportConfig.protocol_name` пишется в поле **1** вместо **3** → для `ws+tls` Xray не находит WS-настройки и использует дефолтные (`path=""` → `/`, без `Host`): **путь и Host из подписки игнорируются**.
>   2. TLS `fingerprint` пишется в поле **10** вместо **11** → uTLS-отпечаток для `tcp+tls`/`ws+tls` **игнорируется**.
> * `ListRule` в v26.3.27 возвращает только `{tag, ruleTag}`; TS-декодер дополнительно ищет `inboundTags` (поле 8), которого в ответе нет — в проде всегда `[]`.
> * Рекомендация: **нативные Go-типы xray-core (`github.com/xtls/xray-core v1.260327.0`)**, без `infra/conf`. Двоичный размер ≈ +8 МБ к «голому» gRPC, CGO не нужен.

---

## 1. Используемые gRPC-методы и protobuf-сообщения

### 1.1. Транспорт (как сейчас)

`scd/src/api/xray-handler-client.ts`:

| Аспект | Поведение | Ссылка |
|---|---|---|
| Соединение | `http2.connect(url)` **на каждый вызов**, `client.close()` в `cleanup` | `xray-handler-client.ts:23-49` |
| Адрес | `target.address` (`host:port`), если без схемы — `http://` (h2c, без TLS) | `:24` |
| Заголовки | `:method POST`, `:path`, `content-type: application/grpc`, `te: trailers` | `:26-31` |
| Фрейм | 1 байт compressed=0 + 4 байта длины BE + payload; сжатые фреймы → `Error('Compressed gRPC frames are not supported.')` | `protobuf.ts:411-437` |
| Таймаут | на весь вызов (connect+request+response), `setTimeout`, default **5000 мс**; из конфигурации `target.timeoutMs` (zod default 5000) | `:77-79`, `:128-131`, `config/load-config.ts:99` |
| grpc-status | берётся из headers (trailers-only) или trailers; `grpc-message` декодируется `decodeURIComponent` **только из trailers** | `:81-96` |
| Ответ | берётся **первый** фрейм (`frames[0] ?? empty`) | `:114-118` |

Тексты ошибок (все — `ApiRequestError`, code `API_REQUEST_ERROR`, `errors.ts:17-21`):

* ECONNREFUSED / EPERM (по `error.code` или подстроке сообщения):
  `Unable to connect to Xray API at ${url.host}. Start the Xray service first, for example with "docker compose up -d".` (`:51-64`)
* Таймаут: `Request to Xray API at ${url.host} timed out after ${timeoutMs}ms.` (`:77-79`)
* grpc-status ≠ 0: `grpcMessage || "gRPC call failed with status ${grpcStatus}"` (`:110-112`), затем оборачивается в `ApiRequestError(error.message)` (`:65`).
* Прочие ошибки сокета: `ApiRequestError(error.message)`.

### 1.2. Список RPC

| # | Full method path | Request (как кодируется) | Response (как декодируется) | Где вызывается |
|---|---|---|---|---|
| 1 | `/xray.app.proxyman.command.HandlerService/ListOutbounds` | `ListOutboundsRequest{}` (пустые байты) | `ListOutboundsResponse.outbounds=1` (repeated `core.OutboundHandlerConfig`); из каждого берётся `tag=1` и сырые байты | `xray-handler-client.ts:133-141`, `protobuf.ts:221-241` |
| 2 | `/xray.app.proxyman.command.HandlerService/ListInbounds` | `ListInboundsRequest{}` (пусто → `isOnlyTags=false`, полные конфиги) | `ListInboundsResponse.inbounds=1` (repeated `core.InboundHandlerConfig`) | `:143-151`, `protobuf.ts:243-263` |
| 3 | `/xray.app.proxyman.command.HandlerService/RemoveOutbound` | `RemoveOutboundRequest{tag=1}` | пусто | `:153-160` |
| 4 | `/xray.app.proxyman.command.HandlerService/AddOutbound` | `AddOutboundRequest{outbound=1: OutboundHandlerConfig}` | пусто | `:162-169` |
| 5 | `/xray.app.proxyman.command.HandlerService/RemoveInbound` | `RemoveInboundRequest{tag=1}` | пусто | `:171-178` |
| 6 | `/xray.app.proxyman.command.HandlerService/AddInbound` | `AddInboundRequest{inbound=1: InboundHandlerConfig}` | пусто | `:180-187` |
| 7 | `/xray.app.router.command.RoutingService/ListRule` | `ListRuleRequest{}` | `ListRuleResponse.rules=1` (repeated `ListRuleItem{tag=1, ruleTag=2}`) | `:189-197`, `protobuf.ts:283-308` |
| 8 | `/xray.app.router.command.RoutingService/RemoveRule` | `RemoveRuleRequest{ruleTag=1}` | пусто | `:199-206` |
| 9 | `/xray.app.router.command.RoutingService/AddRule` | `AddRuleRequest{config=1: TypedMessage(router.Config), shouldAppend=2: true}` | пусто | `:208-218` |

Не используются: `AlterInbound/AlterOutbound`, `GetInboundUsers(Count)`, `TestRoute`, `GetBalancerInfo`, `OverrideBalancerTarget`, `SubscribeRoutingStats`, StatsService, LoggerService.

Сверено с `xray:app/proxyman/command/command.proto:21-106` и `xray:app/router/command/command.proto:95-130`. Все 9 методов в v26.3.27 существуют.

### 1.3. Каталог protobuf-сообщений (поля, реально кодируемые/декодируемые scd)

`TypedMessage` (`xray:common/serial/typed_message.proto:10-15`): `type=1` (string, **полное имя proto-сообщения без `type.googleapis.com/`**), `value=2` (bytes). В Go: `serial.ToTypedMessage(msg)` = `{Type: descriptor.FullName(), Value: proto.Marshal(msg)}` (`xray:common/serial/typed_message.go:10-24`).

| Сообщение (proto full name) | Используемые поля (номер, тип) | Источник в TS | .proto в xray v26.3.27 |
|---|---|---|---|
| `xray.core.OutboundHandlerConfig` | `tag=1` string; `sender_settings=2` TypedMessage; `proxy_settings=3` TypedMessage | `build-outbound-grpc.ts:150-161` | `core/config.proto:46-57` |
| `xray.core.InboundHandlerConfig` | `tag=1`; `receiver_settings=2` TM; `proxy_settings=3` TM | `build-inbound-grpc.ts:58-69` | `core/config.proto:35-43` |
| `xray.app.proxyman.SenderConfig` | `stream_settings=2` StreamConfig | `build-outbound-grpc.ts:141-143` | `app/proxyman/config.proto:52-60` |
| `xray.transport.internet.StreamConfig` | `transport_settings=2` repeated TransportConfig; `security_type=3` string; `security_settings=4` repeated TM; `protocol_name=5` string (`tcp`/`websocket`) | `build-outbound-grpc.ts:123-139` | `transport/internet/config.proto:44-65` |
| `xray.transport.internet.TransportConfig` | **TS пишет `1`=“websocket”** (string) и `settings=2` TM. **В v26.3.27: `protocol_name=3`, `settings=2`; поле 1 отсутствует** | `build-outbound-grpc.ts:101-114` | `transport/internet/config.proto:36-42` |
| `xray.transport.internet.websocket.Config` | `host=1`; `path=2` (default `/`); `header=3` map<string,string> (`{"Host": host}`) | `build-outbound-grpc.ts:85-99` | `transport/internet/websocket/config.proto:9-15` |
| `xray.transport.internet.tls.Config` | `allow_insecure=1` bool (кодируется, если не `undefined`, в т.ч. `false`); `server_name=3`; `next_protocol=4` repeated (alpn); **TS пишет fingerprint в `10`, в v26.3.27 `fingerprint=11`** | `build-outbound-grpc.ts:67-74`, декодер `protobuf.ts:360-362` | `transport/internet/tls/config.proto:40-69` |
| `xray.transport.internet.reality.Config` (client) | `Fingerprint=21`; `server_name=22`; `public_key=23` bytes (base64→bytes); `short_id=24` bytes (hex→bytes, **без паддинга до 8**) | `build-outbound-grpc.ts:76-83` | `transport/internet/reality/config.proto:25-28` |
| `xray.proxy.vless.outbound.Config` | `vnext=1` ServerEndpoint (**singular** в v26) | `:63-65` | `proxy/vless/outbound/config.proto:11-12` |
| `xray.common.protocol.ServerEndpoint` | `address=1` IPOrDomain; `port=2` uint32; `user=3` User | `:55-61` | `common/protocol/server_spec.proto:12-15` |
| `xray.common.protocol.User` | `level=1` (=0, не кодируется); `email=2` (= tag outbound’а); `account=3` TM | `:47-53` | `common/protocol/user.proto:12-18` |
| `xray.proxy.vless.Account` | `id=1` uuid; `flow=2`; `encryption=3` (`none`) | `:39-45` | `proxy/vless/account.proto:16-22` |
| `xray.common.net.IPOrDomain` | oneof `ip=1` bytes / `domain=2` string. Outbound: **всегда `domain`**, даже для IP-литерала (`:35-37`). Inbound listen: IPv4 → `ip` (4 байта), иначе `domain` (`build-inbound-grpc.ts:20-27`) | | `common/net/address.proto:11-17` |
| `xray.app.proxyman.ReceiverConfig` | `port_list=1` PortList; `listen=2` IPOrDomain | `build-inbound-grpc.ts:40-45` | `app/proxyman/config.proto:33-42` |
| `xray.common.net.PortList` / `PortRange` | `range=1` repeated; `From=1`, `To=2` (=port) | `:29-38` | `common/net/port.proto:10-20` |
| `xray.proxy.socks.ServerConfig` | `udp_enabled=4` = true (auth_type=0 NO_AUTH) | `:47-51` | `proxy/socks/config.proto:27-33` |
| `xray.app.router.Config` | `rule=2` repeated RoutingRule | `build-routing-grpc.ts:25-27` | `app/router/config.proto:153-170` |
| `xray.app.router.RoutingRule` | `tag=1` (oneof target_tag: outboundTag); `inbound_tag=8` repeated; `rule_tag=19` | `build-routing-grpc.ts:17-23` | `app/router/config.proto:74-118` |
| `xray.app.router.command.ListRuleItem` | `tag=1` (outbound tag правила), `ruleTag=2` | `protobuf.ts:283-308` | `app/router/command/command.proto:109-116` |

TypedMessage type-строки, которые scd пишет/ожидает:
`xray.app.proxyman.SenderConfig`, `xray.proxy.vless.outbound.Config`, `xray.proxy.vless.Account`, `xray.transport.internet.websocket.Config`, `xray.transport.internet.tls.Config`, `xray.transport.internet.reality.Config`, `xray.app.proxyman.ReceiverConfig`, `xray.proxy.socks.ServerConfig`, `xray.app.router.Config`. `StreamConfig.security_type` — та же строка, что и `type` нужного security-сообщения (`xray:transport/internet/config.go:83-90`).

Декодеры для статуса/runtime-state (`current-runtime-state.ts:436-445`, `protobuf.ts:310-409`):
* `decodeGeneratedOutbound` — распознаёт только `SenderConfig` + `vless.outbound.Config`, иначе `undefined` (для `direct`, `blocked` и т.п.). Читает address/port/uuid/flow/encryption, `StreamConfig.protocol_name=5`, WS host/path по TransportConfig **поле 1 == "websocket"**, TLS sni(3)/alpn(4)/**fingerprint(10)**, Reality fp(21)/sni(22)/pbk(23→base64)/sid(24→hex).
* `decodeGeneratedInbound` — только `ReceiverConfig` + `socks.ServerConfig`: listen, portStart/End, udp (поле 4).
* `decodeGeneratedRoutingRule` — `ruleTag = field19 || field2`, `outboundTag = field1`, `inboundTags = field8`. Применяется к **`ListRuleItem`**, в котором полей 8/19 нет ⇒ на реальном Xray `inboundTags` всегда `[]`, ruleTag берётся из поля 2. Тест `runtime-state.test.ts:72-78` подсовывает полный `RoutingRule`, что маскирует это.

### 1.4. Подтверждённые расхождения ручного кодера с v26.3.27

Проверено так: байты, сгенерированные **настоящими** TS-билдерами (Node, `buildOutboundGrpc`), распарсены через `core.OutboundHandlerConfig` + `GetInstance()` + `StreamConfig.GetEffectiveTransportSettings()/GetEffectiveSecuritySettings()` из xray-core:

```
wsTls:   transport[0].protocol_name=""  unknown=0a09776562736f636b6574   ("websocket" в поле 1 → unknown field)
         EFFECTIVE ws: path="" host="" header=map[]                         ← путь /ws и Host потеряны
         EFFECTIVE tls: sni="cdn.example.com" fingerprint="" unknown=520766697265666f78  ← "firefox" в поле 10
tcpTls:  EFFECTIVE tls: sni="example.com" fingerprint="" unknown=52066368726f6d65   ← "chrome" потерян
reality: OK (поля 21–24 совпадают)
```

Механизм: `GetTransportSettingsFor(protocol)` ищет `TransportConfig` с `ProtocolName == "websocket"`, иначе `CreateTransportConfig(protocol)` — дефолтный конфиг (`xray:transport/internet/config.go:54-81`). Для сравнения: `@remnawave/xtls-sdk` (сгенерирован из .proto) пишет `protocolName` тегом 26 (= поле 3) и `fingerprint` тегом 90 (= поле 11) (`xtls-sdk/src/xray-protos/transport/internet/config.ts:322`, `.../tls/config.ts:373`).

Почему это незаметно в статусе: Go-protobuf сохраняет unknown fields и при `ListOutbounds` (`serial.ToTypedMessage(h.senderSettings)`, `xray:app/proxyman/outbound/handler.go:389-396`) отдаёт их обратно — TS-декодер читает те же «неправильные» поля и показывает host/path/fingerprint, как будто они применены.

**Решение для Go-версии (рекомендация):** кодировать по актуальным .proto (поле 3 / поле 11). Формально это изменение поведения, но фактически — исправление: JSON-эквивалент (`build-outbound-json.ts:43-52`, который хэшируется в `manifestHash`) уже описывает намерение «ws path + Host header + fingerprint». Для статуса декодировать **через типы** (корректные поля); outbounds, созданные старым TS-scd, будут показывать пустые ws/fp до первого apply (после рестарта память пуста ⇒ первый sync всё равно делает полный apply, `README.md` «The no-change optimization is memory-only»).

### 1.5. Прочие нюансы семантики v26.3.27 (важно для паритета)

* `ListOutbounds` **пропускает** commander-outbound (`api`) (`xray:app/proxyman/command/command.go:190-206`). `ListInbounds` ничего не фильтрует (`:103-123`); в тестовом Xray API поднят через `api.listen` (`docker/xray/config/01_api.json:4`), поэтому inbound `api` не существует. При legacy-схеме (dokodemo inbound `api`) его нужно класть в `fixedInbounds`, иначе scd его удалит.
* `RemoveOutbound`: неизвестный tag → **успех** (просто `delete` из map, `xray:app/proxyman/outbound/outbound.go:131-145`). `RemoveInbound`: неизвестный tag → `common.ErrNoClue` (ошибка) (`xray:app/proxyman/inbound/inbound.go:73-90`).
* `AddOutbound/AddInbound` с существующим tag → `existing tag found: <tag>` (`outbound.go:103-128`, `inbound.go:39-60`).
* `AddRule(config, shouldAppend)` → `Router.ReloadRules` (`xray:app/router/router.go:112-211`). `shouldAppend=false` **заменяет все правила и балансеры**; scd всегда шлёт `true` ⇒ правило дописывается **в конец** списка. Дубликат `ruleTag` → `duplicate ruleTag <tag>`.
* `RemoveRule(ruleTag)` удаляет **все** правила с этим ruleTag, неизвестный — успех; пустой ruleTag → `empty tag name!` (`router.go:213-231`). ⇒ если в статической конфигурации есть правило без `ruleTag`, `ListRule` вернёт `ruleTag=""`, scd вызовет `RemoveRule("")`, получит ошибку и routing-apply провалится после частичного удаления (fixedRouting не может содержать пустую строку, `load-config.ts:102`). Это поведение TS — переносить как есть или явно пропускать `""` (решение за владельцем).
* `ListRule` → `ListRuleItem{Tag: rule.Tag (outboundTag, пусто для balancerTag), RuleTag}` (`xray:app/router/command/command.go:71-83`, `router.go:234-245`).
* Порядок правил в тестовом Xray: статические `direct` (geoip:ru) и `x-balancer` (geoip:!ru) (`docker/xray/config/04_routing.json`). Если они не в `fixedRouting`, scd их удалит (README-пример: `fixedRouting: []`, `scd/README.md:86-90`); если в — правила scd дописываются **после** них.
* Inbound listen: `IPOrDomain.domain` для listen разрешён только `localhost`, иначе `domain address is not allowed for listening` (`xray:transport/internet/tcp_hub.go:61-66`). TS кодирует как `ip` только IPv4 ⇒ `listen: "::"` или IPv6 падает на `AddInbound`. В Go `net.ParseAddress()` исправил бы это (изменение поведения — зафиксировать).
* Outbound address всегда `domain`, даже `203.0.113.10` (см. пример ниже). Xray дозвонится (резолв IP-литерала тривиален), но формально это отличается от JSON-конфига. Для паритета в Go: `net.DomainAddress(addr)`; «правильнее» — `net.ParseAddress(addr)`.
* `allowInsecure`: через protobuf `AllowInsecure=true` по-прежнему применяется (`xray:transport/internet/tls/config.go:384`), а `infra/conf` после 2026-06-01 **отвергает** `"allowInsecure": true` (`xray:infra/conf/transport_internet.go:709-716`). Важно для выбора подхода (раздел 5).
* `flow=xtls-rprx-vision-udp443` (visionUdp443Override) поддерживается outbound’ом (`xray:proxy/vless/outbound/outbound.go:252`).

### 1.6. Примеры отправляемых сообщений

Ниже — **фактический** вывод: байты из TS-билдеров → `protojson` через типы xray-core (TypedMessage `value` раскрыт вручную для читаемости). Тестовые данные: uuid `7d1b6590-1069-4372-92be-8d0a0ae6eaf5`.

**VLESS tcp+tls** → `AddOutboundRequest{outbound: …}`
```jsonc
{
  "tag": "vless-at-vienna-tcp-tls-abc",
  "senderSettings": { "type": "xray.app.proxyman.SenderConfig", "value": {
    "streamSettings": {
      "protocolName": "tcp",
      "securityType": "xray.transport.internet.tls.Config",
      "securitySettings": [{ "type": "xray.transport.internet.tls.Config", "value": {
        "allowInsecure": false,              // 08 00 — пишется явно, если allowInsecure задан
        "serverName": "example.com",
        "nextProtocol": ["h2", "http/1.1"],
        "<field 10>": "chrome"               // BUG: должно быть fingerprint (11)
      }}]
  }}},
  "proxySettings": { "type": "xray.proxy.vless.outbound.Config", "value": {
    "vnext": { "address": { "domain": "example.com" }, "port": 443,
      "user": { "email": "vless-at-vienna-tcp-tls-abc",
        "account": { "type": "xray.proxy.vless.Account",
                     "value": { "id": "7d1b6590-…", "encryption": "none" } } } } } }
}
```

**VLESS ws+tls**
```jsonc
{
  "tag": "vless-de-berlin-ws-tls-def",
  "senderSettings": { "type": "xray.app.proxyman.SenderConfig", "value": {
    "streamSettings": {
      "protocolName": "websocket",
      "transportSettings": [{
        "<field 1>": "websocket",            // BUG: должно быть protocolName (3)
        "settings": { "type": "xray.transport.internet.websocket.Config", "value": {
          "host": "ws.example.com", "path": "/ws", "header": { "Host": "ws.example.com" } } }
      }],
      "securityType": "xray.transport.internet.tls.Config",
      "securitySettings": [{ "type": "xray.transport.internet.tls.Config",
        "value": { "serverName": "cdn.example.com", "<field 10>": "firefox" } }]
  }}},
  "proxySettings": { "type": "xray.proxy.vless.outbound.Config", "value": {
    "vnext": { "address": { "domain": "cdn.example.com" }, "port": 443,
      "user": { "email": "vless-de-berlin-ws-tls-def",
        "account": { "type": "xray.proxy.vless.Account", "value": { "id": "…", "encryption": "none" } } } } } }
}
```

**VLESS tcp+reality+vision** (с `observatorySubjectSelectorPrefix: x-observe-`)
```jsonc
{
  "tag": "x-observe-vless-nl-amsterdam-tcp-reality-vision-123",
  "senderSettings": { "type": "xray.app.proxyman.SenderConfig", "value": {
    "streamSettings": {
      "protocolName": "tcp",
      "securityType": "xray.transport.internet.reality.Config",
      "securitySettings": [{ "type": "xray.transport.internet.reality.Config", "value": {
        "Fingerprint": "chrome",                     // 21
        "serverName": "www.microsoft.com",           // 22
        "publicKey": "<32 bytes из base64url pbk>",  // 23
        "shortId": "<hex 6ba85179e30d4fc2 → 8 bytes>" // 24 (короче 8 байт, если sid короче)
      }}]
  }}},
  "proxySettings": { "type": "xray.proxy.vless.outbound.Config", "value": {
    "vnext": { "address": { "domain": "203.0.113.10" }, "port": 443,
      "user": { "email": "x-observe-vless-nl-amsterdam-tcp-reality-vision-123",
        "account": { "type": "xray.proxy.vless.Account",
          "value": { "id": "…", "flow": "xtls-rprx-vision", "encryption": "none" } } } } } }
}
```
(base64 в TS: `Buffer.from(v,'base64')` принимает и std, и url-safe алфавит — в Go нужно пробовать `RawURLEncoding`, затем `StdEncoding`/`RawStdEncoding`.)

**SOCKS inbound** → `AddInboundRequest{inbound: …}` (hex: `0a05696e2d7431 12380a20…5265636569766572436f6e666967…1a230a1d…536572766572436f6e66696712022001`)
```jsonc
{
  "tag": "in-<baseTunnelId>",
  "receiverSettings": { "type": "xray.app.proxyman.ReceiverConfig", "value": {
    "portList": { "range": [{ "From": 20000, "To": 20000 }] },
    "listen": { "ip": "fwAAAQ==" }       // 127.0.0.1
  }},
  "proxySettings": { "type": "xray.proxy.socks.ServerConfig", "value": { "udpEnabled": true } }
}
```

**Routing rule** → `AddRuleRequest`
```jsonc
{
  "config": { "type": "xray.app.router.Config", "value": {
    "rule": [{ "tag": "<outboundTagCurrent>", "inboundTag": ["in-<id>"], "ruleTag": "route-<id>" }]
  }},
  "shouldAppend": true
}
```
Теги: `inboundTag = in-${baseTunnelId}`, `routeTag = route-${baseTunnelId}`, порт = `inboundSocks.portRange.start + index` (`topology/build-tunnel-topology.ts:57-71`).

---

## 2. Алгоритмы apply

### 2.1. Оркестрация (`runtime/sync-once.ts`)

1. Порядок applicator’ов фиксирован: **outbound → inbound → routing** (`sync-once.ts:55-58`), фильтруются по `resources.*.enabled`.
2. На каждую подписку: `buildPlan` (общий), затем `preparePlanForTarget` (`:213-216`).
3. **Diff только по хэшу**: план меняется, если `manifestHash` отличается от сохранённого в памяти для `subscriptionId::targetAddress` (`:217-220`). Хэши: outbound — sha256(JSON `jsonOutbound` отсортированных entries) (`outbound-applicator.ts:18-22`); inbound — sha256 `[{inboundTag, listen, port}]` (`inbound-applicator.ts:16-28`); routing — sha256 `[{routeTag, inboundTag, outboundTag}]` (`routing-applicator.ts:16-28`). Если ничего не изменилось — ни одного RPC, лог `skipped_no_changes`.
4. Если изменилось хоть что-то — применяются **все** enabled kinds (не только изменённые!) под per-target mutex `withTargetMutationLock` (`:247`, `run-state.ts:106-128`).
5. Ошибка одного kind (`report.failed>0` или throw) **не останавливает** следующие kinds (`:253-295`); throw превращается в `createFailedApplyReport` c `items[0].id = kind` (`:78-108`, `:281-294`).
6. Только если **все** kinds успешны — `replaceTargetTopology` (generation++, новые хэши, сброс monitor-state туннелей) (`:297-299`, `run-state.ts:65-84`). Иначе хэши не сохраняются → следующий sync повторит apply.
7. Overlap: `memoryState.syncInProgress` → `sync_skipped_overlap` и пустой отчёт (`:474-477`).

### 2.2. Outbounds (`apply/apply-outbounds.ts`) — единственный kind с rollback

1. Pre-check: любой entry.tag ∈ `fixedOutbounds` → `failed=1`, item `Managed outbound tag "<tag>" conflicts with fixedOutbounds on target "<targetAddress>".`, **без RPC** (`:59-70`).
2. `ListOutbounds` — **вне try**: ошибка пробрасывается наверх (станет failed report с id `outbound`) (`:72`).
3. Удаление: для каждого существующего (в порядке ответа Xray), кроме fixed: сохранить backup (сырые байты `OutboundHandlerConfig` из List), `RemoveOutbound(tag)`, `removed++`, item `removed` (`:79-103`). Удаляются **все** не-fixed, включая чужие (scd «владеет» всем нефиксированным; отдельного ownership-state нет).
4. Добавление: entries в порядке сортировки (tag, line, raw — `outbound-applicator.ts:24-33`), `AddOutbound`; статус `replaced`, если tag был удалён на шаге 3, иначе `added` (`:105-124`).
5. Ошибка в шаге 3/4 → rollback best-effort: `RemoveOutbound` для добавленных в **обратном** порядке, затем `AddOutbound(backup)` для всех удалённых (порядок вставки Map), ошибки rollback глотаются; `failed=1`, item id = `currentId || last added || entries[0].tag || 'unknown'`, message = текст ошибки (`:125-148`).
6. Тег для target: `observatorySubjectSelectorPrefix + tag` и `flow` override (`outbound-applicator.ts:35-53`, `targets/override-outbound-for-target.ts`); при наличии топологии entries берутся из `tunnel.outboundInitial` (`:55-69`).

### 2.3. Inbounds (`apply/apply-inbounds.ts`) — без rollback

1. Pre-check `fixedInbounds` → `Managed inbound tag "<tag>" conflicts with fixedInbounds on target "<target>".` (`:54-65`).
2. `ListInbounds` вне try (`:67`).
3. `RemoveInbound` всех не-fixed (`:71-83`), затем `AddInbound` для каждого туннеля (`:85-98`), статус replaced/added по наличию тега в List.
4. Ошибка → `failed=1`, item id = `tunnels[0].inboundTag ?? 'inbound'`, **rollback нет** (`:99-106`).

### 2.4. Routing (`apply/apply-routing.ts`) — без rollback

1. Pre-check `fixedRouting` по `routeTag` → `Managed routing rule "<tag>" conflicts with fixedRouting on target "<target>".` (`:54-65`).
2. `ListRule` вне try (`:67`).
3. `RemoveRule(ruleTag)` всех не-fixed (по `ruleTag`, `:71-83`), затем `AddRule(TypedMessage(router.Config{rule:[...]}), shouldAppend=true)` — **одно правило на вызов** (`:85-98`).
4. Ошибка → item id = `tunnels[0].routeTag ?? 'routing'`, rollback нет (`:99-106`).

`ApplyReport` (все три): `{kind, sourceId, subscriptionId, targetAddress, appliedAt(ISO), durationMs, added, replaced, removed, failed(0|1), deletedIds, appliedIds, items[{id,status,message?}], skipped}`.

### 2.5. Мутации из monitoring (`runtime/monitoring.ts`)

Используют тот же клиент (`createDefaultMutationClient`, `:47-51`) и тот же target-mutex.

* **repairTunnel** (`:260-347`): `RemoveRule(routeTag)` (ошибка игнорируется) → `RemoveOutbound(outboundTagCurrent)` (игнор) → `AddOutbound(outboundWithoutPrefix)` → `AddRule(rule → unprefixed tag)`. Ошибка: monitor `degraded`, `lastError`, лог `tunnel_repair_failed`; без rollback.
* **rejoinTunnelToBalancer** (`:349-420`): `AddOutbound(prefixed)` → [`RemoveRule(route)` (игнор) → `AddRule(→prefixed)` → `RemoveOutbound(unprefixed)` (игнор)]; при ошибке во внутреннем блоке: `RemoveRule(route)` → `AddRule(→current)` → `RemoveOutbound(prefixed)` (все игнор) и rethrow; `lastError = "Tunnel healthy but balancer rejoin failed: <msg>"`. Последовательности RPC зафиксированы тестами `monitoring.test.ts:575-582, 722-728, 814-828`.

### 2.6. Чтение runtime-state (`runtime/current-runtime-state.ts:436-445`)

`ListOutbounds`, `ListInbounds`, `ListRule` **параллельно** (`Promise.all`) одним клиентом (=3 отдельных HTTP/2-соединения). Классификация: `fixed` / `managed-initial` / `managed-fallback` / `unmanaged` (outbounds, `:255-310`), `fixed`/`managed`/`unmanaged` (inbounds, rules). `rawBase64` — base64 сырых байтов элемента List (опция `includeRaw`); секреты (uuid/publicKey/shortId) вырезаются, если `includeSecrets=false` (`:150-160`).

### 2.7. Требования к Go-клиенту для паритета

* Один `*grpc.ClientConn` на target (`grpc.NewClient(addr, insecure)`), на каждый RPC — `context.WithTimeout(target.timeoutMs)`. Это отличается от «соединение на вызов», но наблюдаемое поведение то же; при желании точного паритета можно создавать conn на вызов (дёшево, xray локален). Рекомендация: shared conn + `WithConnectParams`/без `WaitForReady`, чтобы отказ соединения быстро давал `Unavailable`, а не висел до дедлайна.
* Маппинг ошибок (тексты сохраняются, т.к. попадают в отчёты/логи/статус):
  * `codes.Unavailable` + `connection refused`/`EPERM` в сообщении → `Unable to connect to Xray API at <host:port>. Start the Xray service first, for example with "docker compose up -d".`
  * `codes.DeadlineExceeded` → `Request to Xray API at <host:port> timed out after <N>ms.`
  * иной non-OK status → `status.Message()` (если пусто — `gRPC call failed with status <code-number>`).
* Тексты из Xray (`existing tag found: …`, `duplicate ruleTag …`, `empty tag name!`, `common.ErrNoClue`) проходят «как есть».

---

## 3. Соответствие нативным Go-типам xray-core

Модуль: **`github.com/xtls/xray-core v1.260327.0`** (= тег v26.3.27). Важно: `go get …@v26.3.27` **не работает** (`module path must match major version ("…/v26")`); Xray публикует Go-совместимые теги `v1.YYMMDD.N`. Проверено: `go get github.com/xtls/xray-core@d2758a02…` резолвится в `v1.260327.0`. Тот же pin использует, например, PasarGuard/node (`go.mod`).

`go.mod` xray-core: **`go 1.26`**, `google.golang.org/grpc v1.79.3`, `google.golang.org/protobuf v1.36.11`. ⇒ наш модуль тоже должен быть `go 1.26+` (Node-версия сейчас 24.x — это не влияет).

| Сообщение | Go пакет / тип | Конструкция |
|---|---|---|
| HandlerService client/server | `app/proxyman/command`: `HandlerServiceClient`, `NewHandlerServiceClient`, `HandlerServiceServer`, `UnimplementedHandlerServiceServer`, `RegisterHandlerServiceServer`, `AddOutboundRequest`, `RemoveOutboundRequest`, `ListOutboundsRequest/Response`, `AddInboundRequest`, `RemoveInboundRequest`, `ListInboundsRequest{IsOnlyTags}`, `AlterInbound/AlterOutbound*` | `command.NewHandlerServiceClient(conn)` |
| RoutingService | `app/router/command`: `RoutingServiceClient/Server`, `AddRuleRequest{Config, ShouldAppend}`, `RemoveRuleRequest{RuleTag}`, `ListRuleRequest`, `ListRuleResponse{Rules []*ListRuleItem{Tag, RuleTag}}` | |
| OutboundHandlerConfig / InboundHandlerConfig | `core.OutboundHandlerConfig{Tag, SenderSettings, ProxySettings}`, `core.InboundHandlerConfig{Tag, ReceiverSettings, ProxySettings}` | |
| TypedMessage | `common/serial.TypedMessage`, `serial.ToTypedMessage(m)`, `serial.GetMessageType(m)`, `(*TypedMessage).GetInstance()` | |
| SenderConfig / ReceiverConfig | `app/proxyman.SenderConfig{StreamSettings}`, `proxyman.ReceiverConfig{PortList, Listen}` | |
| StreamConfig / TransportConfig | `transport/internet.StreamConfig{ProtocolName, TransportSettings, SecurityType, SecuritySettings}`, `internet.TransportConfig{ProtocolName, Settings}` | |
| WebSocket | `transport/internet/websocket.Config{Host, Path, Header map[string]string}` | |
| TLS | `transport/internet/tls.Config{AllowInsecure, ServerName, NextProtocol, Fingerprint}` | |
| Reality | `transport/internet/reality.Config{Fingerprint, ServerName, PublicKey, ShortId}` | |
| VLESS | `proxy/vless/outbound.Config{Vnext *protocol.ServerEndpoint}`, `proxy/vless.Account{Id, Flow, Encryption}` | |
| ServerEndpoint / User | `common/protocol.ServerEndpoint{Address, Port, User}`, `protocol.User{Level, Email, Account}` | |
| IPOrDomain / Port | `common/net.IPOrDomain` (`net.NewIPOrDomain(net.DomainAddress(s))` / `net.ParseAddress`), `net.PortList{Range: []*net.PortRange{{From, To}}}` (или `net.SinglePortRange(net.Port(p))`) | |
| SOCKS | `proxy/socks.ServerConfig{UdpEnabled: true}` (AuthType `socks.AuthType_NO_AUTH` = 0) | |
| Router | `app/router.Config{Rule: []*router.RoutingRule{{TargetTag: &router.RoutingRule_Tag{Tag: out}, InboundTag: []string{in}, RuleTag: route}}}` | |

Набросок (компилируется против v1.260327.0, проверено в scratch-модуле):

```go
stream := &internet.StreamConfig{ProtocolName: "websocket",
  TransportSettings: []*internet.TransportConfig{{ProtocolName: "websocket",
    Settings: serial.ToTypedMessage(&websocket.Config{Host: h, Path: p, Header: map[string]string{"Host": h}})}},
  SecurityType:     serial.GetMessageType(&tls.Config{}),
  SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(&tls.Config{ServerName: sni, NextProtocol: alpn, Fingerprint: fp})},
}
ob := &core.OutboundHandlerConfig{Tag: tag,
  SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{StreamSettings: stream}),
  ProxySettings: serial.ToTypedMessage(&vlessout.Config{Vnext: &protocol.ServerEndpoint{
    Address: net.NewIPOrDomain(net.DomainAddress(addr)), Port: uint32(port),
    User: &protocol.User{Email: tag, Account: serial.ToTypedMessage(&vless.Account{Id: uuid, Flow: flow, Encryption: "none"})}}})}
_, err := handler.AddOutbound(ctx, &command.AddOutboundRequest{Outbound: ob})
```

Декодирование статуса: `resp.Outbounds[i].SenderSettings.GetInstance()` → `*proxyman.SenderConfig` и т.д. (требует blank-import зарегистрированных типов — они уже импортированы билдерами). Для `rawBase64` — `proto.Marshal(item)` (не байт-в-байт с ответом сервера, но эквивалентно; для детерминизма `proto.MarshalOptions{Deterministic: true}`).

### 3.1. `infra/conf` (JSON → protobuf)

Есть `conf.OutboundDetourConfig.Build() (*core.OutboundHandlerConfig, error)` (`xray:infra/conf/xray.go:235`), `conf.InboundDetourConfig.Build()` (`:137`), `conf.RouterConfig.Build()` (`router.go:99`), `parseFieldRule` (`router.go:531`, ruleTag `:559`). Так работает CLI `xray api ado/adi/adrules` (`xray:main/commands/all/api/rules_add.go:58-91`) и 3x-ui (`internal/xray/api.go` импортирует `infra/conf`).

Можно было бы скармливать туда `buildOutboundJson` (`build-outbound-json.ts`) — логика «JSON = то, что хэшируется». Но:
* **+12.8 МБ** к бинарю относительно нативных типов (см. 3.2), тянет gvisor, wireguard, netlink, sing-shadowsocks;
* более строгая/иная валидация: `allowInsecure:true` → ошибка после 2026-06-01 (`transport_internet.go:709-716`), reality требует 32-байтный pbk и непустой валидный fingerprint, shortId паддится до 8 байт (`:927-951`) — **меняет поведение** относительно текущего протокольного пути;
* семантика зависит от даты/версии Xray (feature removal), т.е. больше version drift.
Вывод: для паритета **не использовать** `infra/conf`; допустимо в тестах как «оракул» для сверки.

### 3.2. Вес зависимостей (замер, linux/amd64, `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`, go1.26.0)

| Вариант | Бинарь | Пакетов в графе | non-std | Модули |
|---|---|---|---|---|
| C: только `grpc` + `protobuf` (≈ сгенерированные protos) | **10.3 МБ** | 307 | 110 | grpc, protobuf, x/net, x/sys, x/text, genproto/rpc |
| A: нативные пакеты xray-core (все 15 из таблицы выше) | **18.6 МБ** | 482 | 263 | + xray-core, utls, xtls/reality, circl, quic-go (apernet), miekg/dns, gorilla/websocket, sing, blake3, brotli, klauspost/*, go-proxyproto, netipx, x/crypto |
| B: `infra/conf` + command-клиенты | **31.5 МБ** | 661 | 432 | + gvisor, wireguard, netlink, sing-shadowsocks, qpack, btree, x/exp, x/time… |

Причина веса A: `.pb.go` лежат в тех же Go-пакетах, что и реализация (например, `transport/internet/reality` тянет uTLS/xtls-reality, `app/router` — geodata/DNS). Все варианты собираются **без CGO**. Сборка A «с холодным кэшем» поверх уже собранного gRPC — ~9 с; в CI с кэшем модулей — незначительно. Side-effect: `init()` регистрирует конфиги/транспорты — безвредно, но нельзя одновременно линковать собственные сгенерированные копии тех же proto-имён (конфликт в `protoregistry`).

---

## 4. `@remnawave/xtls-sdk` и Go-аналоги

`remnawave/xtls-sdk` v0.17.0 (`48c1172`): `nice-grpc` + `ts-proto`-код, сгенерированный из .proto Xray (скрипт `scripts/export-protos.ts`, сейчас `VERSION='26.7.28'`). API: `XtlsApi{handler, router, stats}`; handler — управление **пользователями** inbound’ов через `AlterInbound` (`addVlessUser`, `addTrojanUser`, …, `removeUser`, `getInboundUsers(Count)`), router — только `removeRuleByRuleTag`, stats — `getSysStats`, user/inbound/outbound stats. Методы-обёртки для Add/List Outbound/Inbound/Rule отсутствуют — доступны лишь через «raw client».

Что scd унаследовал/зеркалирует:
* идею работы с Xray через HandlerService/RoutingService по gRPC и TypedMessage с полными proto-именами;
* `RemoveRule` по `ruleTag` (аналог `removeRuleByRuleTag`);
* отказ от user-API (`AlterInbound`) — scd оперирует целыми outbound/inbound/rule;
* при этом **не** сгенерированные proto, а ручной кодер — откуда и ошибки номеров полей (sdk кодирует их корректно).

Go-эквиваленты: отдельного «Go Xray API SDK» уровня xtls-sdk не найдено (поиск по GitHub: у remnawave на Go только `geocheck`, `migrate`; remnawave/node — TypeScript). Де-факто стандарт в Go — **напрямую импортировать пакеты xray-core**: так делают `xray api …` CLI (`main/commands/all/api/*`), MHSanaei/3x-ui (`internal/xray/api.go`: `app/proxyman/command`, `app/router/command`, `common/serial`, `infra/conf`, `proxy/*`), PasarGuard/node (`github.com/xtls/xray-core v1.260327.0`), mohrezfadaei/marzban-node-go.

---

## 5. Рекомендация

| Критерий | Нативные пакеты xray-core | Сгенерированные из .proto (свой `go_package`) | Ручной кодер (порт TS) |
|---|---|---|---|
| Размер бинаря | ~18.6 МБ (+8.3 МБ к gRPC) | ~10.5 МБ | ~10.3 МБ (gRPC всё равно нужен, либо свой h2-клиент ещё меньше) |
| Корректность номеров полей | гарантирована (те же .pb.go, что в сервере) | гарантирована при регенерации | ручная; уже 2 бага |
| Version drift | bump одной строки в go.mod; компилятор ловит переименования | надо вручную вендорить ~20 .proto и регенерировать (protoc + protoc-gen-go в CI) | ручной аудит |
| Время сборки | +~10 с холодной сборки | минимально + шаг генерации | минимально |
| CGO | не нужен | не нужен | не нужен |
| Fake-сервер для тестов | готовые `HandlerServiceServer`/`RoutingServiceServer` | из сгенерированного кода | писать самим |
| Декодирование статуса | `GetInstance()` типизированно | то же (если имена совпадают) | ручной парсер |
| Риски | тяжёлый граф зависимостей, Go 1.26 обязателен, `init()` side-effects | конфликт имён в protoregistry, если когда-то подтянуть xray-core; переписывание `go_package` | ошибки как сейчас |

**Выбор: нативные пакеты xray-core (`v1.260327.0`), без `infra/conf`.** Обоснование: главная цель — точное и корректное управление конкретной версией Xray; одни и те же сгенерированные типы на клиенте и сервере исключают класс ошибок, который уже проявился в TS. +8 МБ для серверного демона в Docker приемлемо (единый бинарь scd+sfc+gtl и так будет ≥10 МБ из-за gRPC/HTTP). Pin версии xray-core в go.mod синхронизировать с образом в `docker-compose.yml` (CI-проверка: тег образа ↔ `v1.YYMMDD` в go.mod).
Запасной вариант, если размер критичен: вендорить .proto из того же тега и генерировать в `internal/xrayproto` — минус 8 МБ ценой шага кодогенерации; ручной кодер не рекомендуется.

Изоляция: весь xray-импорт держать в одном пакете (`internal/scd/xrayapi`) с интерфейсами `OutboundAPI`, `InboundAPI`, `RoutingAPI` (как `OutboundApiClient`/`InboundApiClient`/`RoutingApiClient` в TS), чтобы apply-логика и тесты не зависели от xray-core напрямую и вариант можно было сменить.

---

## 6. Стратегия тестирования в Go

Текущие TS-тесты **не** поднимают gRPC: они подменяют клиент через фабрики `createOutboundApplicator(createClient)` / `createMutationClient` / `createClient` (`service.test.ts:941-1075`, `monitoring.test.ts:76-134`, `runtime-state.test.ts:119-215`) и сравнивают **сырые байты** с `buildOutboundGrpc(...).raw` (`monitoring.test.ts:58-60, 90-93`) или проверяют порядок вызовов (`'removeRule:…'`, `'addRule:route->tag'`). Сам `XrayHandlerClient` (HTTP/2, фрейминг, ошибки) тестами не покрыт.

Предлагаемые уровни:

1. **Unit (как сейчас):** apply/monitoring против Go-интерфейсов (`ListOutbounds(ctx) ([]*core.OutboundHandlerConfig, error)` и т.д.) с recording-fake. Сравнение сообщений — `proto.Equal` (а не байты: `proto.Marshal` не гарантирует детерминизм). Перенести кейсы: удаление всех нефиксированных, conflict с fixed*, rollback (`addCalls[1] == backup`), последовательности repair/rejoin, параллельные List для runtime-state.
2. **Контрактный (in-process gRPC):** `bufconn` + `grpc.NewServer()`, регистрируем fake, встраивающий `command.UnimplementedHandlerServiceServer` / `routercmd.UnimplementedRoutingServiceServer`, со state-машиной, имитирующей **семантику v26.3.27**: map tag→config; `AddOutbound/AddInbound` дубликат → `existing tag found: X`; `RemoveOutbound` неизвестного → ok, `RemoveInbound` неизвестного → error; `ListOutbounds` не возвращает `api`; правила — упорядоченный срез, `AddRule(shouldAppend=true)` append c проверкой дубликата ruleTag, `RemoveRule("")` → `empty tag name!`, `ListRule` → только `{Tag, RuleTag}`. Fake должен вызывать `GetInstance()` на TypedMessage — это ловит неверные type-строки. Инъекция ошибок/задержек per-method — для тестов rollback, `DeadlineExceeded` → текст `timed out after Nms`, закрытого порта → текст `Unable to connect…` (для этого кейса — реальный `net.Listen` на 127.0.0.1:0 и закрытие).
3. **Семантическая проверка билдеров:** golden-тест «билдер → `GetEffectiveTransportSettings()` / `GetEffectiveSecuritySettings()`» (как в разделе 1.4) — гарантирует, что ws path/Host и fingerprint реально применяются. Опционально — сверка с `infra/conf.OutboundDetourConfig.Build()` поверх `buildOutboundJson` как оракулом (только в `_test.go`, чтобы не тянуть infra/conf в бинарь).
4. **Smoke (docker-compose):** реальный `xray-core:26.3.27`; должен проверять, что после sync `ListOutbounds/ListRule` содержат ожидаемое и SOCKS-туннель проходит. Требует существующего `docker/scd/config.yml` (сейчас в репо отсутствует — `ls docker` показывает только `xray/`).

Опционально можно поднять **настоящий** Xray in-process (`core.New` + `app/commander`) в интеграционных тестах — это даёт 100% семантику, но тянет весь xray-core в тестовый бинарь; рекомендую только как отдельный build tag `integration`.

---

## Приложение: методика проверки

* Клоны: `scratchpad/xray-core` (tag v26.3.27), `scratchpad/xtls-sdk` (main `48c1172`).
* Байты TS: Node-скрипт импортировал `scd/src/builders/*.ts` и вывел hex для tcp+tls, ws+tls, reality+vision, SOCKS inbound, rule.
* Go-декодирование: scratch-модуль с `github.com/xtls/xray-core v1.260327.0`, `proto.Unmarshal` в `core.OutboundHandlerConfig`/`core.InboundHandlerConfig`, `GetInstance()`, `protojson`.
* Замер размеров: три scratch-программы (C/A/B) с `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`.
