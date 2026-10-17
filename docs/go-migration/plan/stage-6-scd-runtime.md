# Этап 6. scd: runtime — sync, мониторинг, status server, дашборд, balancer selection, daemon

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development; TDD обязателен. Прочитать «Глобальные ограничения», «Открытые вопросы» и «Review Focus» в `docs/go-migration/plan/README.md`.

**Goal:** перенести runtime scd: память состояния и per-target lock, `scd sync`, мониторинг туннелей с repair/rejoin, balancer monitor с remote push, SOCKS5-пробу, status server (JSON API + дашборд), новую функцию balancer selection (D22) и `scd daemon` с немедленным graceful shutdown.

**Architecture:** `internal/scd/state` (in-memory модель `03` §1 под `sync.RWMutex` + per-target `*sync.Mutex`) ← `internal/scd/syncer` (оркестрация `03` §2) ← `internal/scd/monitor` (тики `03` §4) с `internal/scd/socks` (проба `03` §5) ← `internal/scd/balancer` (D22) ← `internal/scd/status` (снимки, агрегация, runtime-state, `http.Handler`, шаблоны `embed`) ← `internal/scd/daemon` (cron-циклы + сервер + сигналы). Xray — только через `xrayapi.Client` (`Dialer` инжектируется).

**Tech Stack:** stdlib (`net`, `crypto/tls`, `bufio`, `html/template`, `embed`, `net/http`), `testing/synctest`, `github.com/chromedp/chromedp@latest` (только `-tags visual`), `internal/common/*`, пакеты этапов 4–5.

**Spec:** `07` D2, D3, D7, D9, D21, D22, D23; `03` (весь); `10` §4, §6.2–§6.10; `02` §2.5–§2.6; README плана O5, O8, Review Focus 1, 2, 4, 5.

## Контекст этапа

- Временные метки JSON и логов — `t.UTC().Format("2006-01-02T15:04:05.000Z")` (`03` §1).
- Лог-события и их поля — каталог `03` §8.1 (имена `event` и набор полей сохраняются; порядок ключей не контракт, D9). Новые события: `manifest_entry_warning` (`subscriptionId, targetAddress, tag, code, message`), `balancer_selection_changed|failed|recovered`, `balancer_observatory_unavailable` (`10` §6.4).
- Мутации Xray (apply/repair/rejoin) выполняются под `context.WithoutCancel(ctx)` + таймаут вызова — SIGTERM не обрывает последовательность RPC посередине (Review Focus 1, D7).
- Все сортировки — `strings.Compare`/`slices.Sort` (D23).
- Источник фикстур состояния для тестов status — контракт из задачи 0.4 (`testdata/reference/scd/status/fixtures/*.json`).

---

### Task 6.1: Память состояния, per-target lock и `scd sync`

**Read:** `03` §0 п.1–8, §1, §2 (весь), §8.1 (sync-события), §10.7, §10.8 п.1–4; `01` §14.3, §17.3 S18–S20; `02` §2.1; `07` D2, D23.

**Files:**
- Create: `internal/scd/state/state.go`, `state_test.go`
- Create: `internal/scd/syncer/syncer.go`, `report.go`, `syncer_test.go`
- Create: `internal/scd/cli/sync.go`, `sync_test.go`

**Interfaces:**
- Produces (`state`):
  ```go
  type TunnelMonitor struct { State string /* healthy|degraded|repairing|idle */; LastCheckedAt, LastSuccessAt, LastFailureAt *time.Time
      LastStatusCode, LastLatencyMs, LastSuccessStatusCode, LastSuccessLatencyMs *int; LastError string; ConsecutiveFailures int }
  type BalancerMonitor struct { State, RemotePingState string; RemotePingLastReportedStatus, RemotePingLastReportedMsg string; RemotePingLastReportedPingMs *int
      LastCheckedAt, LastSuccessAt, LastFailureAt *time.Time; LastStatusCode, LastLatencyMs, LastSuccessStatusCode, LastSuccessLatencyMs *int; LastError string
      RemotePingLastRunAt, RemotePingLastSuccessAt, RemotePingLastFailureAt *time.Time; RemotePingLastStatusCode *int; RemotePingLastError string; ConsecutiveFailures int }
  type Tunnel struct{ Mapping topology.Tunnel; Monitor TunnelMonitor }
  type Target struct { Hashes map[apply.Kind]string; Generation int; Topology *topology.Topology; Tunnels []*Tunnel /* порядок topology */; Balancer BalancerMonitor }
  type Memory struct{ /* mu sync.RWMutex; targets map[string]*Target; syncing atomic.Bool; locks map[string]*sync.Mutex */ }
  func New() *Memory
  func Key(subscriptionID, targetAddress string) string                 // "<sub>::<addr>"
  func (m *Memory) View(key string, fn func(t *Target)) bool             // read-lock; false если нет
  func (m *Memory) Update(key string, fn func(t *Target)) bool           // write-lock; false если нет
  func (m *Memory) Ensure(key string)                                    // создаёт пустой Target, если его нет
  func (m *Memory) ReplaceTopology(key string, topo topology.Topology, hashes map[apply.Kind]string)  // Generation++, monitor всех туннелей → idle; Balancer не трогается
  func (m *Memory) Keys() []string
  func (m *Memory) LockTarget(key string) (unlock func())               // per-target mutex
  func (m *Memory) TryStartSync() bool; func (m *Memory) EndSync()
  ```
- Produces (`syncer`):
  ```go
  type TargetReport struct { SubscriptionID, TargetAddress, SourceID string; UnchangedKinds []apply.Kind; Failed int; Resources []apply.Report; Skipped []vless.Skip }
      // json: subscriptionId, targetAddress, sourceId, unchangedKinds, failed, resources, skipped
  type Report struct { AppliedAt string; DurationMs int64; Added, Replaced, Removed, Unchanged, Failed int; Targets []TargetReport; Skipped []vless.Skip }
      // json: appliedAt, durationMs, added, replaced, removed, unchanged, failed, targets, skipped
  type Syncer struct { Config config.Loaded; Memory *state.Memory; Log *slog.Logger; Dial xrayapi.Dialer; Now func() time.Time
      LoadInput func(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error) }
  func (s *Syncer) Run(ctx context.Context) (Report, error)
  ```
  Поведение — `03` §2 со следующими решениями: манифест строится **один раз** на подписку (`manifest.FromSubscription`), планы — `apply.PlanForTarget` для каждого включённого kind в порядке `apply.Order`; ошибка `PlanForTarget` (в т.ч. `ErrPortRangeExhausted`) → `FailedReport` для каждого kind этого target, лог `apply_failed`, остальные target'ы продолжаются (D23); изменения — сравнением хэшей; при изменении применяются **все** kinds под `LockTarget` с `context.WithoutCancel(ctx)`; хэши/topology заменяются только если все kinds успешны; overlap → пустой отчёт + warn `sync_skipped_overlap`; порядок `Targets` — сначала failed-загрузки, затем loaded (`03` §0 п.5); `Skipped` в отчёте **не дублируется** по числу applicator'ов (манифест один — отличие от TS, `03` §0 п.6; зафиксировать тестом); `manifest_built` по одному на kind с `managedCount` = `len(plan.ManagedIDs)`; warnings entries → `manifest_entry_warning`. Клиент `Dial(target.address, target.timeoutMs)` создаётся только если есть что применять и закрывается после.
- CLI `scd sync --config`: `config_loaded` лог, `Syncer.Run`, `jsonout.Write(report)`, `report.Failed>0` → `cliutil.ErrReportFailed`. Логгер — `logging.New(os.Stderr, cfg.Logging.Level, cfg.Logging.Format)`.

- [ ] **Step 1: Failing tests (`state_test.go`):** `TestReplaceTopologyResetsMonitorsKeepsBalancer`, `TestLockTargetMutualExclusion`, `TestTryStartSyncOverlap`, `TestUpdateMissingTargetFalse`.
- [ ] **Step 2: Failing tests (`syncer_test.go`, fake Xray через `Dial`, который отдаёт клиентов `xrayfake.Start` по адресу):** `03` §10.7 п.2 (`TestSkipsXrayWhenUnchanged`: второй прогон `unchanged=1`, `ListOutbounds` вызван один раз), п.3 (`TestAppliesSubscriptionsIndependently`), п.4 (`TestLogsFailedApplyItems`: `errorMessages`, `failedItems`), п.5 (`TestOverlapReturnsEmptyReport`), п.6/S18 (`TestOtherSubscriptionsContinueAfterLoadFailure`: targets `[:8080 failed, :8081]`, клиент только для :8081), п.7/S19 (`TestNoXrayCallsWhenNoValidEntries`), п.8/S20 (`TestNoXrayCallsWhenFiltersRemoveAll`); `03` §10.8: `TestPartialFailureKeepsHashesAndMonitors`, `TestFullSuccessResetsMonitorsAndBumpsGeneration`, `TestSyncPortRangeOverflowFailsOnlyThatTarget` (Review Focus 4, D23), `TestFailedLoadsFirstInTargets`, `TestSkippedNotDuplicated`, `TestEntryWarningsLogged` (ws+vision → `manifest_entry_warning` `flow_cleared`), `TestApplyNotCanceledByContext` (cancel ctx во время `Delay` на AddOutbound → apply доходит до конца, хэши сохранены).
- [ ] **Step 3: Failing test (`sync_test.go`):** `TestSyncCommandExitCode` (Xray недоступен → stdout JSON с `failed>0`, код 1).
- [ ] **Step 4: Run** → FAIL. **Step 5: Implement.** **Step 6: Verify** `go test -race ./internal/scd/... && go build -o bin/scd ./cmd/scd && sed 's/xray:8080/127.0.0.1:1/' docker/scd/config.example.yml > docker/scd/smoke.local.yml && ./bin/scd sync --config docker/scd/smoke.local.yml | jq '.failed'; echo "exit=${PIPESTATUS[0]}"; rm docker/scd/smoke.local.yml` (Xray нет → JSON с `failed` по каждому kind, `exit=1`; stdout — чистый JSON (логи в stderr, логи Xray подавлены, 4.5); файл кладётся рядом с примером, чтобы относительный путь фикстуры резолвился, и не коммитится). Адрес `xray:8080` из примера локально не годится: в песочнице имя резолвится через прокси в посторонний HTTP/1.1-сервер (`error reading server preface … HTTP/1.1 header` — проверено), результат недетерминирован.
- [ ] **Step 7: Commit** `feat(scd): port in-memory state and sync orchestration` (body: D2, D23).

---

### Task 6.2: SOCKS5 + HTTP-проба

**Read:** `03` §5 (весь), §10.2; `07` D23 (SOCKS/HTTP-проба); `06` §7 вариант 3.

**Files:**
- Create: `internal/scd/socks/probe.go`, `probe_test.go`

**Interfaces:**
- Produces:
  ```go
  type Request struct{ ProxyHost string; ProxyPort int; URL, Method string; Timeout time.Duration
      RootCAs *x509.CertPool                                                    // тестовый хук; nil в проде
      Dial func(ctx context.Context, network, addr string) (net.Conn, error) } // тестовый хук; nil → (&net.Dialer{}).DialContext
  type Response struct{ StatusCode int; LatencyMs int; BodyBytes int }
  func Do(ctx context.Context, r Request) (Response, error)
  ```
  Протокол `03` §5.1 (greeting `05 01 00`; CONNECT ATYP 01 для IPv4, иначе 03 domain; ответ с пропуском BND по ATYP 01/03/04; HTTPS — `tls.Client` c `ServerName`; запрос `METHOD path HTTP/1.1`, `Host`, `Connection: close`, для POST `Content-Length: 0`; чтение до `\r\n\r\n` или EOF; статус `^HTTP/1\.[01]\s+(\d{3})`). **Один общий дедлайн** `ctx`+`Timeout` на всю пробу (D23): `conn.SetDeadline` от `context.WithTimeout`; текст таймаута по фазе, на которой истёк дедлайн: `SOCKS5 proxy connection timed out after <N>ms.`, `SOCKS5 handshake timed out after <N>ms.`, `SOCKS HTTPS TLS handshake timed out after <N>ms.`, `SOCKS HTTP request timed out after <N>ms.`; прочие тексты: `SOCKS5 proxy does not accept no-auth authentication.`, `SOCKS5 connect failed with code <REP>.`, `Unexpected EOF from SOCKS proxy.`, `Invalid HTTP response from monitored endpoint.`, `Unable to parse HTTP status line: <line>`. Latency — `time.Since` от записи запроса до заголовков, `math.Round` в мс.

- [ ] **Step 1: Failing tests** (фейковый SOCKS-сервер на `127.0.0.1:0` + `httptest`): `03` §10.2 п.1 (`TestResolvesOnHeadersNotClose`), п.2/п.6 (`TestHandshakeTimeout`), п.3 (`TestRequestBytes`: третья запись начинается `GET /health HTTP/1.1`), п.4 (`TestLatencyExcludesHandshake`: handshake с задержкой 300ms, ответ 20ms → latency < 150), п.5 (`TestConnectTimeout` — через хук `Request.Dial`, который блокируется до `ctx.Done()`; listener без `Accept` **не годится**: ядро завершает TCP-handshake в backlog и `Dial` успешен сразу — проверено ревью, — так что истечёт фаза SOCKS-handshake, а не connect), п.7 (`TestTLSHandshakeTimeout`), + `TestGreetingRejected`, `TestConnectFailedCode5`, `TestBindATYPDomainAndIPv6Skipped`, `TestIPv4UsesATYP1DomainUsesATYP3`, `TestPostHasContentLengthZero`, `TestHostHeaderNonDefaultPort`, `TestInvalidStatusLine`, `TestEOFWithoutHeaders`, `TestHTTPSViaSocks` (`httptest.NewTLSServer`, CA в `tls.Config.RootCAs` через тестовый хук `Request.RootCAs`).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/socks/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): port SOCKS5 HTTP probe with single deadline` (body: D23).

---

### Task 6.3: Мониторинг туннелей, repair/rejoin, balancer monitor и remote push

**Read:** `03` §4 (весь), §8.1 (события мониторинга), §10.6 п.1–25; `02` §2.5; `07` D2.

**Files:**
- Create: `internal/scd/monitor/tunnels.go`, `repair.go`, `balancer.go`, `remoteping.go`, `pingurl.go`, `*_test.go` (по файлу)

**Interfaces:**
- Consumes: `state.Memory`, `socks.Do`, `xrayapi.Dialer`, `resources.Rule`, `outbound.ToProto`.
- Produces:
  ```go
  type Deps struct { Memory *state.Memory; Log *slog.Logger; Dial xrayapi.Dialer; Now func() time.Time
      Probe func(ctx context.Context, r socks.Request) (socks.Response, error)
      Direct func(ctx context.Context, url string, timeout time.Duration) (int, error)   // remote ping без SOCKS: GET, тело не читается
      Sleep func(ctx context.Context, d time.Duration) error }
  func TunnelTick(ctx context.Context, d Deps, sub config.SubscriptionConfig) error   // ошибки воркеров → errors.Join, обёрнутая "concurrent task execution failed"
  func Repair(ctx context.Context, d Deps, sub config.SubscriptionConfig, tunnelID string) error
  func Rejoin(ctx context.Context, d Deps, sub config.SubscriptionConfig, tunnelID string) error
  func BalancerTick(ctx context.Context, d Deps, sub config.SubscriptionConfig) error
  func RemotePingURL(raw, status, msg string, pingMs *int) (string, error)
  func ResolveProxyHost(listen, targetAddress string) string   // 0.0.0.0/:: → host из net.SplitHostPort(targetAddress)
  ```
  Алгоритмы `03` §4.1–§4.6 без изменений смысла (retry внутри слота `maxParallel`; repair только после всех попыток; последовательности RPC repair `removeRule→removeOutbound→addOutbound(unprefixed)→addRule(route->unprefixed)` и rejoin `addOutbound(initial)→removeRule→addRule(->initial)→removeOutbound(unprefixed)` с откатом; тексты `Tunnel repaired; awaiting next successful monitor check.`, `Tunnel healthy but balancer rejoin failed: <msg>`, `Expected HTTP <exp>, got <got>.`, `Remote ping returned HTTP <code>.`, `Remote ping timed out after <ms>ms.`). Мутации — под `LockTarget` и `context.WithoutCancel`. Remote ping — горутина с check-and-set `pending` под `Memory.Update`; горутины учитываются в `sync.WaitGroup`, передаваемом через `Deps` (поле `Background *sync.WaitGroup`) для shutdown. `RemotePingURL`: свой упорядоченный разбор query (`set` заменяет на месте первое вхождение, `ping` удаляется и добавляется в конец), кодирование form-urlencoded (`url.QueryEscape`, плюс `~` → `%7E`).

- [ ] **Step 1: Failing tests** — перенос `03` §10.6 п.1–25 один-в-один по названиям (`TestProbesParallelRepairsSerialized`, `TestRespectsMaxParallel`, `TestClearsCurrentOnFailureKeepsLastSuccess`, `TestRetriesBeforeDegraded`, `TestRetriesUnexpectedStatusThenRepairs`, `TestRetryInsideWorkerSlots`, `TestRepairSequence` (Calls fake = `RemoveRule:route-…`, `RemoveOutbound:x-observe-…`, `AddOutbound:<unprefixed>`, `AddRule:route-…-><unprefixed>`), `TestRejoinAfterSuccess`, `TestRejoinFailureKeepsHealthy`, `TestRejoinSequence`, `TestRejoinSkippedWhenBalanced` (Dial не вызван), `TestRejoinRollbackOnAddRuleFailure`, `TestRejoinRollbackFailureDoesNotFailTick`, `TestUnexpectedStatusNoStaleSuccess`, `TestRepairErrorPropagatesAfterProbes`, `TestBalancerTickViaExternalSocks`, `TestBalancerDegradedKeepsLastSuccess`, `TestBalancerRetryThenUpPush`, `TestBalancerDownPushAfterRetries`, `TestRemotePingAsync`, `TestRemotePingFailureKeepsHealth`, `TestRemotePingOverlapSkipped`, `TestRemotePingViaSocks`, `TestDirectPushDoesNotReadBody`, `TestRemotePingURL` (примеры `03` §4.6), + `TestResolveProxyHostIPv6Target`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/monitor/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): port tunnel monitoring, repair/rejoin and balancer monitor` (body: D2).

---

### Task 6.4: Status: снимки, агрегация, remote collector, runtime-state и JSON API

**Read:** `03` §1 (`balanced`), §6.2–§6.5, §7, §10.3, §10.4 п.5–7, 9–12, §10.5, §10.8 (маршруты); `01` §17.2 T4–T5; `07` D3, D23; README плана O8; `testdata/reference/scd/status/**` (фикстуры, `api-*.json`, `runtime-state/*.json`).

**Files:**
- Create: `internal/scd/status/snapshot.go` (`StatusTunnel`, `Snapshot`, `Group`), `aggregate.go` (remote state, collector, `parseRemoteSnapshot`), `runtimestate.go`, `handler.go` (маршруты, JSON), `fixture_test.go` (загрузчик фикстур 0.4 → `state.Memory` + `config.Loaded` + aggregation), `snapshot_test.go`, `aggregate_test.go`, `runtimestate_test.go`, `handler_test.go`

**Interfaces:**
- Produces:
  ```go
  type StatusTunnel struct{ /* поля 03 §6.4 StatusSnapshotTunnel в том же порядке, опциональные — указатели/omitempty; balanced *bool */ }
  func BuildTunnels(m *state.Memory, cfg config.Loaded) []StatusTunnel          // сортировка subscriptionId, targetAddress, displayName (strings.Compare)
  func Balanced(t topology.Tunnel) *bool                                         // 03 §4.4
  type InstanceSnapshot struct{ Version int; InstanceID, CapturedAt string; Subscriptions []GroupedSubscription; Tunnels []StatusTunnel }
  type Aggregator struct{ /* local snapshot + remotes map, mu */ }
  func NewAggregator(remotes []config.RemoteStatusInstanceConfig) *Aggregator
  func (a *Aggregator) Collect(ctx context.Context, r config.RemoteStatusInstanceConfig, client *http.Client, log *slog.Logger, now func() time.Time)   // 03 §7; хранит json.RawMessage snapshot (passthrough) + распарсенную структуру для HTML
  type Deps struct { Config config.Loaded; Memory *state.Memory; Aggregator *Aggregator; Dial xrayapi.Dialer; Now func() time.Time; Hostname func() (string, error); Log *slog.Logger }
  func NewHandler(d Deps) http.Handler   // обёрнут httpserve.Recover
  func BuildRuntimeState(ctx context.Context, d Deps, subscriptionID, targetAddress string) (any, int /*http status*/, error)
  ```
  Маршруты (O8, по пути, query игнорируется, метод не проверяется): `/api/status` → `{"tunnels": [...]}`; `/api/instance-status`; `/api/instances`; `/api/runtime-state` (404 disabled / 400 нет параметров / 404 target / 502 `*xrayapi.Error` / 500 прочее / 200); всё остальное → HTML (задача 6.5; здесь — заглушка `text/html` с пустым телом, тест HTML не проверяет). JSON — `jsonout.Marshal`, `application/json; charset=utf-8`. Каждый запрос пересобирает локальный снимок и сохраняет в агрегатор (`03` §6.2).

- [ ] **Step 1: Failing tests:**
  - `snapshot_test.go`: T4 (displayName/countryIso2/endpoint/key), T5 (`balanced` true → false после repair), `TestGroupSortsByLatencyIdleLast` (`03` §10.5 п.8), `TestGroupSortsSubscriptionsAndTargets` (п.7), `TestConfiguredGroupsIncludeTargetsWithoutTunnels`.
  - `aggregate_test.go`: `03` §10.3 п.1–3; `TestRemoteSnapshotPassthroughKeepsUnknownFields` (Review Focus 5); `TestRemoteSnapshotVersionMustBe1`.
  - `handler_test.go`: для **каждой** фикстуры 0.4 — `/api/status`, `/api/instance-status`, `/api/instances` семантически равны `testdata/reference/scd/status/<name>/api-*.json` (`TestJSONMatchesTSReference`); `03` §10.4 п.5–7, 9–12; `TestQueryIgnoredForRouting` (O8: `/api/status?x=1` → JSON); `TestPanicReturns500` (D23).
  - `runtimestate_test.go`: `03` §10.5 п.1–6 против `xrayfake` (ключи/классификации/diff равны эталонам `runtime-state/*.json`; значения `parsed` ws/fingerprint не сравнивать).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/status/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): port status snapshots, aggregation and JSON API` (body: D3, D23, O8).

---

### Task 6.5: Дашборд (html/template + embed) и visual regression

**Read:** `07` D3; `03` §6.6, §10.4 п.1–4, 8; `scd/src/runtime/status-server.ts:46-476` (разметка, CSS, JS — переносятся как есть); `testdata/reference/scd/status/*/index.html`, `screenshot-*.png`.

**Files:**
- Create: `internal/scd/status/web/dashboard.html.tmpl`, `web/dashboard.css`, `web/dashboard.js` (CSS/JS скопировать из TS дословно; `dashboard.js` — браузерный ассет, не Node-код: проверки «нет Node» задачи 7.3 исключают каталог `internal/scd/status/web/` явно), `internal/scd/status/dashboard.go` (`//go:embed web/*`), `dashboard_test.go`
- Create: `internal/scd/status/visual_test.go` (`//go:build visual`)
- Modify: `internal/scd/status/handler.go` (HTML-маршрут); `.github/workflows/docker.yml` (шаг visual в джобе `go`: `- uses: browser-actions/setup-chrome@v2` с `id: chrome` (без `id` выражение `steps.chrome.outputs.chrome-path` пустое; output `chrome-path` у v2 есть — проверено по `action.yml`) → `SCD_CHROME=${{ steps.chrome.outputs.chrome-path }} go test -tags visual ./internal/scd/status/...`)

**Interfaces:**
- Produces: `func RenderDashboard(w io.Writer, agg AggregatedView) error`; helper-функции шаблона — порты `stateClass`, `formatLatency`, `formatLastCheck`, `formatBalancerStatus`, `formatRemotePushStatus`, `metricClass` (`03` §6.6) как `template.FuncMap`.
- Visual test: chromedp (`go get github.com/chromedp/chromedp@latest` → сейчас **v0.21.0 с новым generic API**, старые примеры `chromedp.Run(ctx, actions...)`/`FullScreenshot(&buf, q)` **не компилируются** — проверено). Использовать: `actx, c1 := chromedp.NewExecAllocator(ctx, append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(path), chromedp.NoSandbox)...)`; `bctx, c2 := chromedp.NewContext(actx)`; действия без результата — `chromedp.Do(bctx, chromedp.EmulateViewport(w, 800), chromedp.Navigate("file://…"), chromedp.Click("#instance-tab-1"))`; скриншот — `png, err := chromedp.Run(bctx, chromedp.FullScreenshot(100))` (quality 100 → PNG, full page; проверено ревью с `headless_shell` из `/opt/pw-browsers` на хосте под root и в контейнере `chromedp/headless-shell`). `ExecPath` = env `SCD_CHROME` (локально `/opt/pw-browsers/chromium_headless_shell-1194/chrome-linux/headless_shell`; без env → `t.Skip`). Для каждой фикстуры 0.4: записать TS-эталон `index.html` и Go-HTML (тот же fixture-loader, что в 6.4, замороженное `Now`) во временный каталог; в **одном** браузере снять full-page скриншоты обоих при ширине 1280 и 390; сравнить: одинаковые размеры, доля отличающихся пикселей ≤ 0.001; для `remote-instances` дополнительно после `chromedp.Click("#instance-tab-1")`. При провале — сохранить `ts.png`, `go.png`, `diff.png` в `t.TempDir()` и вывести путь.

- [ ] **Step 1: Failing tests (`dashboard_test.go`, без браузера):** `03` §10.4 п.1 (`TestDashboardCardsAndClasses`: `balancer-card state-degraded`, `node-card state-healthy`, `Last Check`, `>42 ms<`, `>failed down<`, `metric-balancer-active`, нет `/api/runtime-state`, `<table>`, `Kuma`; balancer-card раньше `nodes-grid`), п.2, п.3, п.4, п.8 (табы, `aria-selected`, `tabindex`, `hidden`, JS-обработчики `ArrowRight|ArrowLeft|Home|End`), `TestDashboardEscapesHostileStrings` (Review Focus 5: фикстура `hostile-strings` → нет `<script>alert`), `TestDashboardTextMatchesTS` (для каждой фикстуры видимый текст (`golang.org/x/net/html`, текстовые узлы `<body>` вне `<script>`/`<style>`, пробелы схлопнуты) совпадает с TS-эталоном).
- [ ] **Step 2: Failing test (`visual_test.go`):** `TestDashboardVisualMatchesTS`.
- [ ] **Step 3: Run** `go test ./internal/scd/status/...` → FAIL; `SCD_CHROME=… go test -tags visual ./internal/scd/status/...` → FAIL.
- [ ] **Step 4: Implement** шаблон (структура `03` §6.6; экранирование — html/template, D3 допускает `&#34;`/`&#39;`).
- [ ] **Step 5: Run** оба прогона → PASS. Если пиксельные расхождения вызваны только сортировкой (D23) — исправлять фикстуру не разрешается; разбор в отчёте задачи.
- [ ] **Step 6: Commit** `feat(scd): port status dashboard with visual regression against TS` (body: D3).

---

### Task 6.6: Balancer selection (D22): цикл, состояние, JSON API, карточка дашборда

**Read:** `10` §4, §6.2–§6.10 (весь дизайн); `07` D22; `docker/xray/config/01_api.json`, `05_observatory.json`.

**Files:**
- Create: `internal/scd/balancer/selection.go` (`ResolveSelection`, сопоставление тегов), `tick.go`, `selection_test.go`, `tick_test.go`
- Modify: `internal/scd/state/state.go` (+ `BalancerSelection` в `Target`, не сбрасывается в `ReplaceTopology`; **создаётся без sync** — `10` §6.3 последний пункт: тик вызывает `Memory.Ensure`)
- Modify: `internal/scd/status/snapshot.go`, `handler.go`, `runtimestate.go`, `web/dashboard.html.tmpl`, `web/dashboard.css` (+ тесты: `balancer_view_test.go`)
- Create: `testdata/reference/scd/status/fixtures/selection-{single,override,pool,none,error,observatory-unavailable,repaired}.json` (расширение контракта 0.4 полем `targets[].balancerSelection` — формат как JSON-ответ `10` §6.6), `testdata/golden/scd/status/selection-*.html` (Go-эталоны, генерируются тестом с флагом `-update`)
- Modify: `docker/xray/config/01_api.json` (+ `"ObservatoryService"`), `docker/scd/config.example.yml` (блок `balancerSelection: {enabled: true, balancerTag: x-balancer, schedule: "*/15 * * * * *"}` с комментарием)

**Interfaces:**
- Produces: `type Mode string` (`override|single|pool|none|unknown`); `func ResolveSelection(info xrayapi.BalancerInfo) (mode Mode, selected string, pool []string)` — `10` §6.2; `func MapTag(tag, prefix string, tunnels []*state.Tunnel) (tunnelID string, managed bool)` — `10` §4 п.2; `type SelectionState struct{…}` — поля `10` §6.4; `func Tick(ctx context.Context, d Deps, sub config.SubscriptionConfig) error` — `GetBalancerInfo` и (если `observatory`) `GetOutboundStatus` параллельно (`errgroup` из `golang.org/x/sync` или две горутины + `sync.WaitGroup`), без target-lock, обновление под `Memory.Update`, логи с подавлением повторов (`10` §6.4). JSON: `GroupedStatusTarget.balancerSelection` (после `balancerMonitor`), поля `StatusTunnel` (`outboundTag`, `balancerSelected`, `balancerCandidate`, `observatory*` — `10` §6.6), `serviceState.balancerSelection` в runtime-state; при выключенной функции ключи отсутствуют (`omitempty`), `version` = 1. Дашборд: карточка `Xray selection · {balancerTag}` после «External SOCKS check», бейджи `Selected`/`Pool`, метрика `Observatory` (`10` §6.7); при отсутствии `balancerSelection` разметка не выводится.

- [ ] **Step 1: Failing tests (`10` §6.9 п.2–7):** `TestResolveSelectionTable` (override × len(principle) 0/1/3 × known), `TestMapTag` (prefixed → туннель; repaired → не выбран, observatory `n/a`; rejoin-окно → `awaiting`; unmanaged `x-observe-static`; устаревшая запись observatory отфильтрована; пустой prefix + точное совпадение), `TestTick*` на `synctest` (`idle→ok`, `ok→error` с сохранением selected и одним логом, повтор той же ошибки без лога, `error→ok` `recovered`, смена выбора `changed` + `selectedSince`, observatory недоступен при рабочем routing, тик не блокируется target-lock'ом (держать `LockTarget` в тесте), тик до первого sync создаёт state), `TestFeatureOffJSONUnchanged` (фикстуры 0.4 → ответы по-прежнему равны TS-эталонам), `TestFeatureOnJSONKeys` (порядок и имена ключей `10` §6.6), `TestParseRemoteAcceptsNewFields`, `TestDashboardSelectionCard` (тексты `Selected/Latency/Since/Observatory`, классы `is-selected`, `state-*`), `TestDashboardSelectionGolden` (Go-эталоны `testdata/golden/...`, `-update` перегенерирует), `TestDashboardFeatureOffNoMarkup`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./... && SCD_CHROME=… go test -tags visual ./internal/scd/status/...` (visual для feature-off фикстур по-прежнему проходит) → PASS; `./bin/scd validate-config --config docker/scd/config.example.yml` → ok без warning'ов.
- [ ] **Step 5: Commit** `feat(scd): show Xray balancer selection on dashboard and status API` (body: D22, D3).

---

### Task 6.7: `scd daemon`: циклы, status server, сигналы

**Read:** `03` §3 (весь), §6.1, §8.1 (daemon-события); `07` D7, D23; README плана Review Focus 1–2.

**Files:**
- Create: `internal/scd/daemon/daemon.go`, `daemon_test.go`
- Create: `internal/scd/cli/daemon.go`

**Interfaces:**
- Consumes: всё выше.
- Produces: `type Daemon struct{ Config config.Loaded; Log *slog.Logger; Dial xrayapi.Dialer; HTTPClient *http.Client; Now func() time.Time }`; `func (d *Daemon) Run(ctx context.Context) error`. Порядок: `runtime.mode != daemon` → ошибка `config at <path> is not in daemon mode`; `config_loaded`; status server (если `enabled && listen != ""`; ошибка bind → вернуть ошибку; лог `status_server_started`); циклы `cronloop.RunLogged`: sync (`daemon_tick`; события `daemon_tick_skipped_overrun`/`sync_failed` «Daemon sync failed.»), для каждой подписки (включая disabled — как TS) monitor/balancerMonitor/balancerSelection при `enabled && schedule` (события `03` §3.1 и `10` §6.4), remote instances; все циклы — горутины под `sync.WaitGroup`; `ctx.Done()` → циклы выходят сразу (D7), активные задачи и фоновые remote-ping'и дожидаются, затем `httpserve.Run` делает `Shutdown` (таймаут 10 s). CLI `scd daemon --config` создаёт ctx через `signal.NotifyContext(…, os.Interrupt, syscall.SIGTERM)`.

- [ ] **Step 1: Failing tests (`synctest` + fake Xray):** `TestDaemonRejectsNonDaemonMode`, `TestDaemonFirstSyncImmediate`, `TestDaemonStartsWhenXrayDown` (Review Focus 2: Dial отдаёт адрес закрытого порта → status server отвечает 200 на `/api/instance-status`, следующий тик снова пытается), `TestDaemonShutdownWaitsForInFlightApply` (Review Focus 1: `Delay` на AddOutbound, cancel посреди → `Run` возвращается только после завершения apply, состояние fake полное), `TestDaemonStopsImmediatelyBetweenTicks` (D7: расписание `0 0 1 1 *`, cancel → возврат без ожидания слота), `TestDaemonStartsLoopsForDisabledSubscriptionMonitors` (как TS), `TestDaemonBindErrorFails`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.**
- [ ] **Step 4: Verify:**
  ```bash
  go test -race ./... && golangci-lint run && go build -o bin/scd ./cmd/scd
  sed 's/0.0.0.0:8080/127.0.0.1:19090/; s/xray:8080/127.0.0.1:1/' docker/scd/config.example.yml > docker/scd/smoke.local.yml
  (./bin/scd daemon --config docker/scd/smoke.local.yml & p=$!; sleep 2; curl -s http://127.0.0.1:19090/api/instance-status | jq .version; kill -TERM $p; wait $p; echo exit=$?)
  rm docker/scd/smoke.local.yml
  ```
  Expected: `1`, `exit=0` менее чем через секунду после TERM. (Файл кладётся рядом с примером, чтобы относительный путь фикстуры резолвился; он не коммитится.)
- [ ] **Step 5: Commit** `feat(scd): add scd daemon with immediate graceful shutdown` (body: D7, D23).
