# Этап 5. scd: Xray gRPC API и apply

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development; TDD обязателен. Прочитать «Глобальные ограничения» в `docs/go-migration/plan/README.md`. Это ядро сервиса (D2): поведение переносится строго.

**Goal:** клиент Xray API на типах xray-core v26.10.10 за интерфейсом `xrayapi.Client`, in-process fake Xray на bufconn с семантикой v26.10.10, построение inbound/routing-ресурсов, алгоритмы apply (outbound с rollback, inbound, routing), applicator'ы с хэшами планов, read-only RPC балансера/observatory и live-тесты против настоящего Xray 26.10.10.

**Architecture:** `internal/scd/xrayapi` — единственный пакет, который ходит в gRPC (`grpc.NewClient`, один `*grpc.ClientConn` на target, `context.WithTimeout(timeoutMs)` на вызов, маппинг ошибок). `internal/scd/xrayapi/xrayfake` — fake-сервер (используется тестами всех вышестоящих пакетов). `internal/scd/resources` — inbound/routing protobuf и декодирование для runtime-state. `internal/scd/apply` — алгоритмы `02` §2 и applicator'ы. `internal/scd/livetest` — тесты под build tag `xraylive`.

**Tech Stack:** `google.golang.org/grpc` (+ `credentials/insecure`, `test/bufconn`, `status`, `codes`), xray-core (`app/proxyman/command`, `app/router/command`, `app/router`, `app/observatory/command`, `app/observatory`, `core`, `common/serial`, `infra/conf`).

**Spec:** `07` D2, D4, D5, D19, D22; `02` §1.2, §1.5, §2, §2.7, §3, §6; `08` §2–§3, §7; `10` §2, §3, §6.1, §6.9; `09` §1.4; README плана O5.

## Контекст этапа

- Тексты ошибок API-слоя — дословно из `02` §2.7 и `10` §6.1 (D2 «те же тексты»; это исключение из D9).
- Порядок `ListOutbounds`/`ListInbounds` у Xray недетерминирован (`08` §3): fake **перемешивает** ответ, тесты не полагаются на порядок, rollback восстанавливает по сохранённому списку.
- `ListRule` возвращает только `{Tag, RuleTag}`; `inboundTags` в runtime-state всегда `[]` (как у TS на реальном Xray).
- Ревью перепроверило на живом Xray 26.10.10 (релизный бинарь в Docker, `docker/xray/config` + `ObservatoryService`): тексты `existing tag found: <tag>` (outbound/inbound), `common: not enough information for making a decision`, `app/router: duplicate ruleTag <tag>`, `app/router: empty tag name!`, `app/router: cannot find tag`, успешные `RemoveOutbound`/`RemoveRule` неизвестного, `ListRule` = `{direct,direct}`, `{"",x-balancer}`; `GetOutboundStatus` отвечает; отсутствующий сервис на bufconn → `codes.Unimplemented` `unknown service xray.app.router.command.RoutingService`; закрытый порт → `codes.Unavailable` с `connect: connection refused`. Все outbound'ы плана (tcp-tls c pcs/vcn, ws-tls с `?ed=`, reality с пустым sid и `spiderX`, xhttp с `extra`, ML-KEM) после `AddOutbound` возвращаются `ListOutbounds` и `proto.Equal` отправленным. `testing/synctest` + bufconn-gRPC работают вместе (включая `-race`).

---

### Task 5.1: `xrayapi.Client` (9 RPC), маппинг ошибок и fake Xray на bufconn

**Read:** `02` §1.2, §1.5, §2.7, §6 п.1–2; `08` §3 (таблица живого прогона — тексты ошибок v26.10.10), §7; `07` D2, D4.

**Files:**
- Create: `internal/scd/xrayapi/client.go`, `errors.go`, `client_test.go`
- Create: `internal/scd/xrayapi/xrayfake/fake.go`, `handler.go`, `routing.go`, `fake_test.go`

**Interfaces:**
- Produces (`xrayapi`):
  ```go
  type Rule struct{ Tag, RuleTag string }
  type OutboundAPI interface {
      ListOutbounds(ctx context.Context) ([]*core.OutboundHandlerConfig, error)
      AddOutbound(ctx context.Context, h *core.OutboundHandlerConfig) error
      RemoveOutbound(ctx context.Context, tag string) error }
  type InboundAPI interface {
      ListInbounds(ctx context.Context) ([]*core.InboundHandlerConfig, error)
      AddInbound(ctx context.Context, h *core.InboundHandlerConfig) error
      RemoveInbound(ctx context.Context, tag string) error }
  type RoutingAPI interface {
      ListRules(ctx context.Context) ([]Rule, error)
      AddRule(ctx context.Context, r *router.RoutingRule) error   // AddRuleRequest{Config: ToTypedMessage(&router.Config{Rule: [r]}), ShouldAppend: true}
      RemoveRule(ctx context.Context, ruleTag string) error }
  type Client interface { OutboundAPI; InboundAPI; RoutingAPI; BalancerAPI; ObservatoryAPI; Close() error }  // BalancerAPI/ObservatoryAPI — задача 5.5 (здесь объявить пустыми интерфейсами-заглушками и дополнить там)
  type Dialer func(address string, timeout time.Duration) (Client, error)
  func Dial(address string, timeout time.Duration) (Client, error)       // grpc.NewClient(passthrough:///addr, insecure); без WaitForReady
  func DialWith(address string, timeout time.Duration, opts ...grpc.DialOption) (Client, error)  // для bufconn
  type Error struct{ Message string }   // аналог ApiRequestError; Error() = Message
  ```
  Маппинг (`errors.go`, каждая ошибка RPC → `*Error`): `codes.Unavailable` с `connection refused`/`operation not permitted` в тексте → `Unable to connect to Xray API at <address>. Start the Xray service first, for example with "docker compose up -d".`; `codes.DeadlineExceeded` → `Request to Xray API at <address> timed out after <N>ms.`; иначе `status.Message()` или `gRPC call failed with status <int(code)>`.
- Produces (`xrayfake`):
  ```go
  type Server struct{ /* state под mutex */ }
  type Option func(*Server)
  func WithStaticOutbounds(tags ...string) Option   // freedom-outbound'ы, как direct/blocked
  func WithStaticInbounds(tags ...string) Option
  func WithStaticRules(rules ...xrayapi.Rule) Option
  func WithoutRoutingService() Option; func WithoutObservatoryService() Option   // сервис не регистрируется → Unimplemented
  func Start(t testing.TB, opts ...Option) (*Server, xrayapi.Client)          // bufconn; Cleanup закрывает
  func (s *Server) Address() string                                            // строка для текстов ошибок, напр. "bufconn"
  func (s *Server) OutboundTags() []string; InboundTags() []string; Rules() []xrayapi.Rule  // отсортированные копии
  func (s *Server) Outbound(tag string) *core.OutboundHandlerConfig
  func (s *Server) Calls() []string        // "AddOutbound:<tag>", "RemoveRule:<ruleTag>", "AddRule:<ruleTag>-><outboundTag>", …
  func (s *Server) FailNext(method string, err error)   // одноразовая инъекция (status.Error)
  func (s *Server) Delay(method string, d time.Duration)
  ```
  Семантика v26.10.10: дубль `AddOutbound` → `codes.Unknown` `app/proxyman/outbound: existing tag found: <tag>`; `RemoveOutbound` неизвестного → ok; дубль `AddInbound` → `app/proxyman/inbound: existing tag found: <tag>`; `RemoveInbound` неизвестного → `common: not enough information for making a decision`; `AddRule` дописывает в конец, дубль непустого ruleTag → `app/router: duplicate ruleTag <tag>` (состояние не меняется); `RemoveRule("")` → `app/router: empty tag name!`; неизвестный → ok; удаляет все с этим ruleTag; `ListRule` → `{Tag, RuleTag}`; List* ответы перемешаны; на каждый Add вызывается `GetInstance()` у всех TypedMessage (неверный type → `codes.Unknown`).

- [ ] **Step 1: Failing tests (`fake_test.go`)** — по одному на каждое правило семантики выше (`TestFakeDuplicateOutbound`, `TestFakeRemoveUnknownOutboundOK`, `TestFakeRemoveUnknownInboundFails`, `TestFakeAddRuleAppendsAndRejectsDuplicate`, `TestFakeRemoveEmptyRuleTag`, `TestFakeListRuleShape`, `TestFakeRejectsUnknownTypedMessage`, `TestFakeListOrderShuffled` (100 вызовов дают ≥2 разных порядка при ≥3 элементах)).
- [ ] **Step 2: Failing tests (`client_test.go`):** `TestClientRoundTrip` (Add/List/Remove каждого вида через bufconn), `TestErrorConnectionRefused` (реальный `net.Listen` → Close → `Dial` → `ListOutbounds` → текст `Unable to connect to Xray API at 127.0.0.1:<port>. Start the Xray service first, for example with "docker compose up -d".`), `TestErrorTimeout` (fake `Delay` 200ms, timeout 50ms → `Request to Xray API at … timed out after 50ms.`), `TestErrorPassesXrayMessage` (`existing tag found: x`), `TestErrorEmptyMessage` (`status.Error(codes.Internal,"")` → `gRPC call failed with status 13`).
- [ ] **Step 3: Run** `go test ./internal/scd/xrayapi/...` → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/scd/... && golangci-lint run` → PASS.
- [ ] **Step 6: Commit** `feat(scd): add Xray API client and in-process fake Xray v26.10.10` (body: D2, D4).

---

### Task 5.2: Ресурсы inbound/routing и декодирование для runtime-state

**Read:** `02` §1.3 (SOCKS, Router), §1.5 (listen domain), §1.6 (примеры SOCKS inbound и rule), §2.6; `03` §6.4 (CurrentRuntimeStateSnapshot: `parsed`), §10.5; `testdata/reference/scd/status/runtime-state/*.json` (форма `parsed`); `scd/src/api/protobuf.ts:310-409` (ключи `parsed`, пока файл существует); `07` D19.

**Files:**
- Create: `internal/scd/resources/inbound.go`, `routing.go`, `describe.go`, `resources_test.go`

**Interfaces:**
- Consumes: `topology.Tunnel`.
- Produces:
  - `func Inbound(t topology.Tunnel) (*core.InboundHandlerConfig, error)` — через `conf.InboundDetourConfig` из JSON `{"tag":"in-…","listen":<listen>,"port":<port>,"protocol":"socks","settings":{"auth":"noauth","udp":true}}` (`listen` IP → `ip`; `::` → ip; домен кроме `localhost` Xray отвергнет на Add — это его ошибка).
  - `func Rule(t topology.Tunnel, outboundTag string) *router.RoutingRule` — `{TargetTag: &router.RoutingRule_Tag{Tag: outboundTag}, InboundTag: []string{t.InboundTag}, RuleTag: t.RouteTag}`.
  - `type ParsedOutbound map`-эквивалент со стабильными ключами: `func DescribeOutbound(h *core.OutboundHandlerConfig, includeSecrets bool) (map[string]any, bool)` — только VLESS-outbound'ы (`SenderConfig`+`vless/outbound.Config`), ключи как в эталоне `runtime-state` (`protocol`, `address`, `port`, `uuid`, `flow`, `encryption`, `network`, `security`, `sni`, `alpn`, `fingerprint`, `host`, `path`, `publicKey`, `shortId` + аддитивно `xhttpMode`); без секретов — без `uuid`, `publicKey`, `shortId`. Значения читаются **корректными** полями (ws из `TransportConfig.ProtocolName=="websocket"`, fingerprint — поле 11; D6).
  - `func DescribeInbound(h *core.InboundHandlerConfig) (map[string]any, bool)` (`protocol: "socks"`, `listen`, `portStart`, `portEnd`, `udp`) и `func DescribeRule(r xrayapi.Rule) map[string]any` (`ruleTag`, `outboundTag`, `inboundTags: []`).
  - `func RawBase64(m proto.Message) string` — `proto.MarshalOptions{Deterministic: true}` + `base64.StdEncoding`.

- [ ] **Step 1: Failing tests:** `TestInboundSocksShape` (GetInstance: `ReceiverConfig.PortList.Range[0].From==20000`, `Listen.GetIp()` = 127.0.0.1; `socks.ServerConfig.UdpEnabled`), `TestInboundListenAllIPv6` (`::` → 16-байтный ip), `TestRuleShape`, `TestDescribeOutboundKeysMatchReference` (множество ключей `parsed` для ws/reality = эталонному из `runtime-state/raw-and-secrets.json` ∪ {`xhttpMode`}), `TestDescribeRedactsSecrets`, `TestDescribeNonVlessReturnsFalse` (freedom).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): build inbound and routing resources and describe runtime objects` (body: D6, D19).

---

### Task 5.3: Apply outbounds (полная замена, fixed, rollback)

**Read:** `02` §2.2 (алгоритм дословно), §2.4 (`ApplyReport`); `03` §2.4 (порядок ключей отчёта); `01` §17.1 P23–P24; `07` D2.

**Files:**
- Create: `internal/scd/apply/report.go`, `outbounds.go`, `outbounds_test.go`

**Interfaces:**
- Consumes: `xrayapi.OutboundAPI`, `outbound.Outbound`, `outbound.ToProto`.
- Produces:
  ```go
  type Kind string  // KindOutbound="outbound", KindInbound="inbound", KindRouting="routing"; var Order = []Kind{KindOutbound, KindInbound, KindRouting}
  type Item struct{ ID string `json:"id"`; Status string `json:"status"`; Message string `json:"message,omitempty"` }  // removed|added|replaced|failed
  type Report struct { Kind Kind `json:"kind"`; SourceID string `json:"sourceId"`; SubscriptionID string `json:"subscriptionId"`; TargetAddress string `json:"targetAddress"`
      AppliedAt string `json:"appliedAt"`; DurationMs int64 `json:"durationMs"`; Added, Replaced, Removed, Failed int /* json: added, replaced, removed, failed */
      DeletedIDs []string `json:"deletedIds"`; AppliedIDs []string `json:"appliedIds"`; Items []Item `json:"items"`; Skipped []vless.Skip `json:"skipped"` }
  type Meta struct{ SourceID, SubscriptionID, TargetAddress string; Now func() time.Time }
  func FailedReport(kind Kind, m Meta, skipped []vless.Skip, err error) Report   // items[0].id = kind, durationMs 0
  func Outbounds(ctx context.Context, c xrayapi.OutboundAPI, fixed []string, entries []outbound.Outbound, m Meta) (Report, error)
  ```
  Алгоритм `02` §2.2: pre-check fixed (`Managed outbound tag "<tag>" conflicts with fixedOutbounds on target "<target>".`, без RPC); `ListOutbounds` — ошибка возвращается как `error` (вызывающий превращает в FailedReport); удалить все не-fixed с backup; добавить entries (порядок — как переданы; сортирует applicator), статус `replaced`/`added`; при ошибке — rollback best-effort: Remove добавленных в обратном порядке, Add backup'ов в порядке удаления, ошибки rollback игнорируются; `failed=1`, item id = текущий || последний добавленный || entries[0].tag || `unknown`.

- [ ] **Step 1: Failing tests** (fake из 5.1): `TestOutboundsReplacesExisting` (P23: items removed→replaced; Calls: Remove до Add), `TestOutboundsKeepsFixed`, `TestOutboundsDeletesForeignNonFixed`, `TestOutboundsFixedConflictNoRPC` (Calls пусты), `TestOutboundsRollbackOnAddFailure` (P24: `FailNext("AddOutbound")` на втором add → последний item failed, финальный вызов — re-add backup, состояние Xray = исходное (по тегам и `proto.Equal`)), `TestOutboundsRollbackErrorsIgnored`, `TestOutboundsListErrorPropagates`, `TestReportJSONKeyNames` (ключи как `03` §2.4).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/apply/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): port outbound apply with fixed tags and rollback` (body: D2).

---

### Task 5.4: Apply inbounds/routing и applicator'ы (планы, хэши, подготовка под target)

**Read:** `02` §2.1, §2.3, §2.4; `01` §12 (последний абзац), §17.3 S16–S17; `03` §2.1–§2.2 (что applicator отдаёт sync'у); README плана O5.

**Files:**
- Create: `internal/scd/apply/inbounds.go`, `routing.go`, `plan.go`, `inbounds_test.go`, `routing_test.go`, `plan_test.go`

**Interfaces:**
- Consumes: `resources.Inbound`, `resources.Rule`, `topology.Build`, `outbound.*`, `manifest.Manifest`, `config.*`.
- Produces:
  ```go
  func Inbounds(ctx context.Context, c xrayapi.InboundAPI, fixed []string, tunnels []topology.Tunnel, m Meta) (Report, error)   // без rollback; conflict text: Managed inbound tag "<tag>" conflicts with fixedInbounds on target "<target>".
  func Routing(ctx context.Context, c xrayapi.RoutingAPI, fixed []string, tunnels []topology.Tunnel, m Meta) (Report, error)   // RemoveRule всех не-fixed ruleTag (включая "" — O5), затем AddRule по одному; conflict: Managed routing rule "<tag>" conflicts with fixedRouting on target "<target>".
  type Plan struct { Kind Kind; SourceID string; ManifestHash string; ManagedIDs []string; Outbounds []outbound.Outbound; Topology *topology.Topology; Skipped []vless.Skip; Summary *manifest.Summary }
  func PlanForTarget(kind Kind, m manifest.Manifest, sourceID string, t config.SubscriptionTargetConfig) (Plan, error)
      // outbound: при inboundSocks — из topology (tunnel.OutboundInitial), иначе ForTarget(prefix+tag); entries сортируются по tag; hash = outbound.Hash
      // inbound: hash sha256(JSON [{inboundTag, listen, port}]); routing: [{routeTag, inboundTag, outboundTag(=initial)}]
      // ошибка topology (ErrPortRangeExhausted) — возвращается (sync валит только этот target, D23)
  func Apply(ctx context.Context, c xrayapi.Client, p Plan, t config.SubscriptionTargetConfig, m Meta) Report   // диспетчер по Kind; ошибка → FailedReport
  ```

- [ ] **Step 1: Failing tests:** inbounds — замена всех не-fixed, conflict без RPC, ошибка Add → failed без rollback, item id = первый inboundTag; routing — порядок `RemoveRule`→`AddRule`, conflict, ошибка; `TestRoutingRemovesEmptyRuleTagLikeTS` (O5: статическое правило без ruleTag не в fixed → `RemoveRule("")` → report failed с `app/router: empty tag name!`); plan — S16 (переставленные строки → одинаковые hash и ManagedIDs, entries отсортированы), S17 (prefix меняет hash, теги начинаются с `x-observe-`), `TestPlanTopologyOverflowError`, `TestPlanRoutingUsesInitialTag`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): port inbound/routing apply and resource plans` (body: D2, D23, O5).

---

### Task 5.5: Read-only RPC балансера и observatory (D22)

**Read:** `10` §2.1–§2.4, §3.1, §5.2 (тексты ошибок), §6.1, §6.9 (fake-описание, кейс 1); `07` D22.

**Files:**
- Create: `internal/scd/xrayapi/balancer.go`, `balancer_test.go`
- Create: `internal/scd/xrayapi/xrayfake/balancer.go`, `observatory.go`; Modify: `xrayfake/fake.go`

**Interfaces:**
- Produces (`xrayapi`) — дословно `10` §6.1: `BalancerAPI.GetBalancerInfo(ctx, tag) (BalancerInfo, error)`, `ObservatoryAPI.GetOutboundStatus(ctx) ([]OutboundObservation, error)`, `BalancerInfo{OverrideTarget string; PrincipleTargets []string; PrincipleKnown bool}`, `OutboundObservation{Tag; Alive; DelayMs *int64; LastError string; LastSeenAt, LastTryAt *time.Time; HealthPing *HealthPing}`, `HealthPing{All, Fail int64; Average, Deviation, Min, Max time.Duration}`. Нормализация: `[""]`→`[]`; `principle_target==nil` → `PrincipleKnown=false`; `delay 99999999` или `!alive` → `DelayMs=nil`; `0` секунд → nil; результат observatory отсортирован по `Tag`. Ошибки: `type ServiceDisabledError struct{ Service string }` (текст `Xray API service <S> is not enabled (add it to api.services).`) при `Unimplemented` + `unknown service`; `type BalancerNotFoundError struct{ Tag string }` (`Xray balancer "<tag>" not found.`) при `cannot find tag`; прочее — общий маппинг 5.1.
- Produces (`xrayfake`): `func WithBalancer(tag string, selectors []string, strategy string) Option` (`leastPing|leastLoad|random|roundRobin`); `func (s *Server) SetObservation(obs ...*observatory.OutboundStatus)`; `func (s *Server) SetOverride(balancer, target string)`; семантика `10` §6.9 (кандидаты — outbound'ы fake с префиксом, `sort.Strings`; leastPing — min delay среди alive, иначе `[""]`; leastLoad expected 0 — то же с tie-break по тегу, иначе `[]`; random/rr — все кандидаты; неизвестный тег → `codes.Unknown` `app/router: cannot find tag`; observatory-ответ перемешан).

- [ ] **Step 1: Failing tests (кейс 1 из `10` §6.9):** `TestBalancerInfoNormalizesEmptyTag`, `TestBalancerInfoNilPrinciple`, `TestBalancerInfoOverride`, `TestBalancerNotFound`, `TestRoutingServiceDisabled`, `TestObservatoryDisabled`, `TestObservationDecoding` (99999999 → nil, Unix seconds → time, ns → Duration, сортировка по тегу), `TestFakeStrategies` (leastPing/leastLoad/random/roundRobin на 4 кандидатах с одним мёртвым).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): add read-only balancer and observatory API calls` (body: D22).

---

### Task 5.6: Live-тесты против Xray 26.10.10 (`-tags xraylive`)

**Read:** `07` D2, D6, D13, D19, D22; `09` §1.4 (воспроизведение паники), §7 п.11; `10` §5.1 (стенд); `08` §3, §5; scratchpad-артефакты (если есть): `/tmp/claude-0/-home-user-scd/b738d151-5a61-5b3f-9a86-51bb9c74ebc7/scratchpad/bin-xray-26.10.10`, `.../x2610/live/main.go`, `.../x2610/spider/main.go`, `.../balcfg/main/`.

**Files:**
- Create: `internal/scd/livetest/harness_test.go` (`//go:build xraylive`), `params_test.go`, `apply_test.go`, `reality_test.go`, `balancer_test.go`, `parity_test.go`
- Create: `internal/scd/livetest/testdata/xray/{00_log.json,01_api.json,03_inbound.json,04_routing.json,05_observatory.json,06_outbound.json}` — как `docker/xray/config`, но: API `127.0.0.1:{{API_PORT}}`, services `HandlerService, RoutingService, ObservatoryService`; routing **без geoip** (правило `{"ruleTag":"x-balancer","inboundTag":["socks-proxy"],"balancerTag":"x-balancer"}`); inbound `socks-proxy` на `127.0.0.1:{{SOCKS_PORT}}`; observatory `probeURL: http://127.0.0.1:{{PROBE_PORT}}/generate_204`, `probeInterval: 2s`
- Modify: `.github/workflows/docker.yml` — в джобу `go` шаг: скачать `https://github.com/XTLS/Xray-core/releases/download/v26.10.10/Xray-linux-64.zip` и `…/Xray-linux-64.zip.dgst`, сверить `sha256sum` с строкой `SHA2-256=` из `.dgst`, распаковать в `$RUNNER_TEMP/xray`, `SCD_XRAY_BIN=$RUNNER_TEMP/xray/xray go test -tags xraylive -race -count=1 ./internal/scd/livetest/...` (ассет существует, хотя релиз — pre-release; проверено ревью: zip 22 168 131 байт, sha256 `72a5fde8…e5d9` совпадает с `.dgst`, внутри `xray`, `geoip.dat`, `geosite.dat`; `xray version` → `Xray 26.10.10 … 701af60 (go1.27.2 linux/amd64)`)

**Interfaces:**
- Consumes: `xrayapi.Dial`, `outbound.ToProto`, `manifest.Build`, `apply.*`, `resources.*`.
- Produces (только тестовые): `func startXray(t *testing.T) (client xrayapi.Client, socksAddr string)` — читает `SCD_XRAY_BIN` (нет → `t.Skip("SCD_XRAY_BIN not set")`), подставляет свободные порты в шаблоны, запускает `xray run -confdir <tmp>`, ждёт готовности `ListOutbounds` (до 10 s), в `Cleanup` убивает процесс и проверяет, что он не упал раньше (exit code / паника в stderr → `t.Errorf`).

- [ ] **Step 1: Тесты (пишутся сразу как падающие при наличии бинаря):**
  - `TestLiveEveryParamReachesXray` (D6, D19): строки `docker/fixtures/subscription.txt` + из `testdata/reference/scd/corpus/vless-corpus.txt` с `spx`, `pcs`, `vcn`, пустым `sid`, `?ed=`, `type=xhttp` (+`extra`), mlkem; для каждого принятого entry: `AddOutbound(ToProto)` → `ListOutbounds` → найти по тегу → `proto.Equal` для `SenderSettings.GetInstance()` и `ProxySettings.GetInstance()` с отправленным.
  - `TestLiveRealityDoesNotCrashXray` (D13): reality-outbound (через `outbound.ToProto`, `serverName: "example.com"` — этот SAN есть в сертификате `httptest`) со случайным `pbk` на адрес `httptest.NewTLSServer` (`127.0.0.1:<port>`); `SSL_CERT_FILE` процесса Xray указывает на PEM сертификата этого сервера (REALITY проверяет «настоящий» сертификат системным пулом — `transport/internet/reality/reality.go:129-135`, `SSL_CERT_FILE` действует; встроенный CA-bundle Xray относится только к обычному TLS); routing-правило `in-x → outbound`; SOCKS-inbound через `resources.Inbound`; HTTP-запрос через SOCKS (`internal/scd/socks` ещё нет — использовать `golang.org/x/net/proxy` только в тесте; запрос сам по себе завершится ошибкой — это ожидаемо). Ассерты: (1) stderr Xray содержит `REALITY: received real certificate` — путь «spider» реально пройден; (2) через 2 s процесс Xray жив. **Воспроизводимость подтверждена ревью** на релизном бинаре 26.10.10: с outbound'ом из `conf` (SpiderY из 10 элементов) — лог `received real certificate`, Xray жив; с тем же outbound'ом, у которого вручную обнулены `SpiderX/SpiderY`, — `panic: runtime error: index out of range [8] with length 0 … reality.go:298`, `exit status 2` (скрипт: scratchpad ревью `review/xexp/spider/main.go`). Поэтому `t.Skip` здесь допустим только при отсутствии `SCD_XRAY_BIN`.
  - `TestLiveApplyReplacesManagedKeepsFixed` (D2): полный `apply.Apply` outbound→inbound→routing дважды; fixed (`direct`, `blocked`, `socks-proxy`, ruleTag `x-balancer`) на месте; теги совпадают с планом.
  - `TestLiveFakeParity`: один сценарий (дубль Add outbound/inbound, дубль ruleTag, `RemoveRule("")`, Remove неизвестного inbound) против live и против `xrayfake` → одинаковые тексты ошибок.
  - `TestLiveBalancerAndObservatory` (D22, `10` §6.9 п.8): `GetBalancerInfo("x-balancer")` → `PrincipleKnown`; `GetOutboundStatus` без ошибки; `GetBalancerInfo("nope")` → `BalancerNotFoundError`.
- [ ] **Step 2: Run без бинаря** `go test -tags xraylive ./internal/scd/livetest/...` → все `SKIP`. **С бинарём** (`SCD_XRAY_BIN=<path> go test -tags xraylive -race -count=1 ./internal/scd/livetest/...`) → на этом шаге FAIL допустим только из-за отсутствующих хелперов; доведя хелперы — PASS. Бинарь: релизный архив (команды — README плана, «Локальный образ Xray»; скачивание из песочницы работает — проверено) или `scratchpad/bin-xray-26.10.10`. Корпусная mlkem-строка со случайным ключом в live-тест не попадает (её отсекает normalize, 4.4); если какой-то принятый entry падает на `AddOutbound` — это баг normalize, а не повод исключить строку.
- [ ] **Step 3: CI-шаг** в джобе `go` (см. Files).
- [ ] **Step 4: Verify** `go vet -tags xraylive ./... && golangci-lint run && go test -race ./...`.
- [ ] **Step 5: Commit** `test(scd): add live Xray 26.10.10 integration tests` (body: D2, D6, D13, D19, D22).
