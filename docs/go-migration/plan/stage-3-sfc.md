# Этап 3. sfc → `scd sfc`

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development; TDD обязателен. Прочитать «Глобальные ограничения» в `docs/go-migration/plan/README.md`.

**Goal:** перенести sfc (фильтр подписок + публичная раздача base64) в `internal/sfc` и `scd sfc serve|refresh|validate-config|print-config`.

**Architecture:** `internal/sfc/config` → `internal/sfc/refresh` (загрузка через `common/inputsrc`, разбор `common/vless`, фильтр `common/regexlit`, state outputs) + `internal/sfc/cache` (дисковый `.sfc-cache`) → `internal/sfc/server` (`http.Handler`) → `internal/sfc/cli`. Расписание — `common/cron` + `common/cronloop`.

**Tech Stack:** stdlib, `internal/common/*`.

**Spec:** `04-sfc.md` (весь), `07` D7, D8, D9, D10, D23, D24, D26, D27 (O7, O9, O10); README плана O11, O12, O13.

## Контекст этапа

Принятые отличия от TS (зафиксировать тестами, перечислить в README в 7.3):
- regex — RE2 (D8); `refreshSchedule` — классический Linux cron (D24, `common/cron`): пустой, `L/W/#/H/?`, dow `7`, `@every`, никогда не срабатывающие — ошибка конфига;
- строки подписки отбираются тем же `common/vless.Parse` с **общим** белым списком параметров (D27 O7, задача 1.7): ссылки с `spx`, `pqv`, `pcs`, `vcn`, `ech`, `fm`, `extra`, `support-x25519mlkem768` и т.п., которые TS-sfc выбрасывал как `unsupported_param`, теперь попадают в выход; неизвестные параметры игнорируются (D28) — ссылка попадает в выход. sfc применяет только `Parse` (как TS: «тот же парсер, что в scd», 04 §1 п.2) — проверка scope транспорта/шифрования (`normalize`, `internal/scd`) в sfc не выполняется: sfc раздаёт ссылки клиентам, а не в Xray;
- User-Agent загрузки — `subscriptions[].fetchUserAgent`, по умолчанию `scd/<version>` (D27 O9; в TS — `node`);
- логи — stderr, `logging.format` `json|pretty` (D27 O10);
- base64 — stdlib (невалидный base64 в формате `base64` → ошибка загрузки → fallback на кэш);
- `#100%` в label → строка пропускается, подписка не падает (O11);
- сортировка отчёта — `strings.Compare` по `id` (D23);
- env: как в TS — после YAML, только строковые значения, отсутствующая → `""` (`configload.EnvStringsLenient`);
- `serve`: начальный refresh до старта HTTP, затем cron-цикл **без** немедленного повторного refresh (первый тик cron — по расписанию; см. Task 3.4) — устраняет двойной refresh (04 §12 п.1; README плана O12);
- shutdown немедленный (D7);
- путь запроса — `r.URL.EscapedPath()` + удаление dot-сегментов; ключ — `pathRoute + "/" + escapeID(id)`, где `escapeID(id) = strings.ReplaceAll(url.QueryEscape(id), "+", "%20")` — Go-эквивалент `encodeURIComponent` из TS (`sfc/src/runtime/output-path.ts`); отличается только на `!'()*` (Go экранирует). `url.PathEscape` **не** подходит: он оставляет `+&=:@$` (для id `x+y` даст `x+y`, а TS и клиенты используют `x%2By`) — проверено ревью. Тот же `escapeID` — для имени файла кэша (3.2).

---

### Task 3.1: Конфиг sfc и `validate-config` / `print-config`

> **Поправка D29/D30 (обязательна):** фильтр меток — `labelIncludeRegex` (строка или список, AND) + новый `labelExcludeRegex` (строка или список, OR); общий пакет `internal/common/labelfilter` (`type Filter struct{Include, Exclude []*regexp.Regexp}`; `func Parse(node) (Filter, error)`; `func (f Filter) Match(label string) bool`), компиляция элементов через `regexlit` (D8); lookahead → ошибка с подсказкой `use labelIncludeRegex/labelExcludeRegex lists instead of lookahead`. Тесты: `TestLabelFilterRealShapeEquivalence` — для корпуса меток `testdata/reference/common/label-filter.json` (этап 0.6) решения Go-формы фильтров из D29 совпадают с решениями TS-регулярок; `TestRealShapeConfigGoFormValid` — Go-форма `docker/<svc>/config.real-shape.yml` проходит `validate-config`; `TestRealShapeConfigTSFormRejectedWithHint`. Здесь — `outputs[].{labelIncludeRegex,labelExcludeRegex}` (хотя бы один обязателен); Go-форма — `docker/sfc/config.real-shape.go.yml`.

**Read:** `04` §2 (CLI, формат validate), §3 (весь), §11 тесты 13–16; `docker/sfc/config.example.yml`; `testdata/reference/sfc/print-config-docker-example.json`.

**Files:**
- Create: `internal/sfc/config/config.go`, `config_test.go`
- Modify: `internal/sfc/cli/cli.go`, create `internal/sfc/cli/cli_test.go`

**Interfaces:**
- Consumes: `configload.Load(path, configload.EnvStringsLenient, os.LookupEnv)`, `configload.ResolveInput`, `schema`, `cron.Parse`, `listenaddr.Parse`, `regexlit.Compile`.
- Produces:
  ```go
  type Config struct {
      Subscriptions []Subscription `json:"subscriptions"`
      Runtime struct{ RefreshSchedule string `json:"refreshSchedule"` } `json:"runtime"`
      Server  struct{ Listen string `json:"listen"` } `json:"server"`
      Logging struct{ Level string `json:"level"`; Format string `json:"format"` } `json:"logging"`
  }
  type Subscription struct { ID, Input string; Enabled bool; Format string; FetchTimeoutMs int; FetchUserAgent string; PathRoute string; Outputs []Output }
      // json: id, input, enabled, format, fetchTimeoutMs, fetchUserAgent (omitempty), pathRoute, outputs
  type Output struct { ID string `json:"id"`; Enabled bool `json:"enabled"`; Name string `json:"name,omitempty"`; LabelIncludeRegex string `json:"labelIncludeRegex"`; UserAgent []string `json:"userAgent,omitempty"`; ProfileTitle string `json:"profileTitle,omitempty"`; ProfileUpdateInterval int `json:"profileUpdateInterval,omitempty"` }
      // каждое поле — с явным json-именем (тег `json:",omitempty"` без имени дал бы ключ `Name`, и TestPrintConfigDockerExample упал бы)
  type Loaded struct { Path, Dir string; Config Config }
  func Load(path string) (Loaded, error)
  ```
- Правила (04 §3.2): неизвестные ключи игнорируются (не strict); trim строк; `enabled` default true; `format` auto; `fetchTimeoutMs` 5000 (>0); `fetchUserAgent` — необязательная непустая строка (trim); `pathRoute` начинается с `/`, ≠ `/`, не оканчивается `/`; `outputs` ≥1; `labelIncludeRegex` обязателен и компилируется `regexlit`; `userAgent` элементы непустые, массив ≥1; `profileUpdateInterval` 1..24; app-level: хотя бы одна enabled подписка; хотя бы один enabled output в enabled подписках; уникальность subscription id и output id (глобально, включая disabled); `refreshSchedule` — `cron.Parse`; `server.listen` — `listenaddr.Parse`; `logging` default `{info, json}`, `format` ∈ `json|pretty`. `input` резолвится от каталога конфига.
- CLI: `validate-config` → `jsonout.Write({"configPath": abs, "valid": true})`; `print-config` → `jsonout.Write(cfg)`.

- [ ] **Step 1: Failing tests** (`04` §11 п.13–16 + новые):
  - `TestRejectsBrokenRegexAndDuplicateOutput` (п.13: оба сообщения в ошибке — проверки не обрываются на regex)
  - `TestPathRouteRules` (п.14: `/profiles` ок; отсутствие → `subscriptions.0.pathRoute: required`; `profiles/` → путь содержит `pathRoute`)
  - `TestUserAgentAndIntervalBounds` (п.15), `TestProfileFieldsPreserved` (п.16)
  - `TestEnvMissingBecomesEmpty`, `TestEnvInNumberFieldIsTypeError` (`fetchTimeoutMs: ${TM}` → `expected integer`)
  - `TestRejectsLookaheadRegex` (D8), `TestRejectsEmptyRefreshSchedule` (D23), `TestRejectsNonLinuxCron` (`0 0 ? * *` → путь `runtime.refreshSchedule`, D24)
  - `TestFetchUserAgentOptional` (отсутствует → нет ключа в print-config; `"  "` → issue `subscriptions.0.fetchUserAgent`), `TestLoggingFormatEnum` (`pretty` ок, `text` → issue)
  - `TestEmptyNameDropped` (`name: "  "` → отсутствует в print-config)
  - `cli_test.go`: `TestPrintConfigDockerExample` — семантически равно эталону (поле `input` сравнивать после замены абсолютного префикса репозитория).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Verify** `go test -race ./internal/sfc/... && go build -o bin/scd ./cmd/scd && ./bin/scd sfc validate-config --config docker/sfc/config.example.yml`.
- [ ] **Step 5: Commit** `feat(sfc): port sfc config and validate/print commands` (body: D8, D9, D23).

---

### Task 3.2: Refresh, состояние outputs и дисковый кэш источника

**Read:** `04` §2.1 (формат отчёта), §4.1–§4.7, §6, §7.1, §11 тесты 17–24, §12; `testdata/reference/sfc/refresh-docker-example.json`, `testdata/reference/sfc/outputs/*`.

**Files:**
- Create: `internal/sfc/cache/cache.go`, `cache_test.go`
- Create: `internal/sfc/refresh/refresh.go`, `state.go`, `refresh_test.go`

**Interfaces:**
- Consumes: `config.Loaded`, `inputsrc.Load`, `subscription.ScanLines`, `vless.Parse`, `regexlit.Compile`.
- Produces (`cache`): `func Dir(configPath string) string` (`<dir(config)>/.sfc-cache`); `func Path(configPath, subscriptionID string) string` (`<Dir>/<escapeID(id)>.subscription`, `escapeID` — из «Контекста этапа» (QueryEscape + `+`→`%20`, как `encodeURIComponent` в TS `source-cache.ts`); итог для `../main/slash id` — `..%2Fmain%2Fslash%20id.subscription`, проверено ревью; совпадение с TS-именами важно, т.к. volume `.sfc-cache` переживает миграцию); `func Write(configPath, id, content string) (string, error)` — `MkdirAll`, temp-файл через `os.CreateTemp(dir, base+".*.tmp")`, `Rename`, при ошибке удалить temp; `func Read(configPath, id string) (path, content string, err error)`.
- Produces (`refresh`):
  ```go
  type OutputState struct { ID, SubscriptionID, PathRoute, Name, ProfileTitle string; UserAgent []string; ProfileUpdateInterval int
      LastGoodBase64, LastGoodPlain string; LastGoodLineCount int; LastRefreshAt, LastSuccessAt, LastFailureAt time.Time; LastError string }
  type State struct { /* mu sync.RWMutex; outputs map[id]*OutputState (атомарная замена указателя); byPath map[string]string (path → id) */ }
  func NewState(cfg config.Config) *State           // регистрирует ВСЕ outputs (включая disabled → 502)
  func (s *State) Lookup(path string) (*OutputState, bool)   // snapshot-копия
  type OutputReport struct { ID string `json:"id"`; SubscriptionID string `json:"subscriptionId"`; Name string `json:"name,omitempty"`; MatchedLines int `json:"matchedLines"`; OK bool `json:"ok"`; UsedCachedValue bool `json:"usedCachedValue"`; UsedSourceCache bool `json:"usedSourceCache,omitempty"`; Error string `json:"error,omitempty"` }
  type Report struct { RefreshedAt string `json:"refreshedAt"`; SourceCount int `json:"sourceCount"`; OutputCount int `json:"outputCount"`; Successful int `json:"successful"`; Failed int `json:"failed"`; Outputs []OutputReport `json:"outputs"` }
  type Refresher struct { Cfg config.Loaded; State *State; Log *slog.Logger; Now func() time.Time; Input func(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error) }
  func (r *Refresher) Run(ctx context.Context) Report
  ```
- Алгоритм — `04` §4.6 (последовательно по enabled подпискам; кэш пишется до разбора; при ошибке загрузки — fallback на кэш с `usedSourceCache`; иначе failure каждого enabled output с сохранением lastGood; пустой фильтр → `Filtered output is empty.`); base64 выхода — `base64.StdEncoding` от `strings.Join(raws, "\n")`; outputs отчёта сортируются `strings.Compare` по id; `RefreshedAt` — `Now().UTC().Format("2006-01-02T15:04:05.000Z")`. Логи — события `04` §7.1 (`subscription_source_cache_written`, `…_write_failed`, `subscription_refreshed`, `subscription_source_cache_used`, `subscription_source_cache_missed`, `subscription_refresh_failed`) с теми же полями.

- [ ] **Step 1: Failing tests (`cache`):** `TestPathEscapesID` (п.22), `TestWriteReadRoundTrip`, `TestWriteRenameFailureRemovesTemp` (п.23: на месте файла каталог → ошибка, в каталоге нет `*.tmp`).
- [ ] **Step 2: Failing tests (`refresh`)** — `Input` подменяется функцией-фейком, `Now` — фиксированным временем:
  - `TestRefreshPlainRemote` (п.17), `TestRefreshBase64Auto` (п.18), `TestRefreshWritesCacheAndFallsBack` (п.19), `TestRefreshNoCacheFails` (п.20; лог `subscription_source_cache_missed` с `cachePath`, `cacheError` ~ `no such file`), `TestRefreshEmptyFilterKeepsLastGood` (п.21), `TestRefreshReplacesStateAtomically` (п.24), `TestRefreshMalformedLabelSkipsLineOnly` (O11).
  - `TestRefreshUsesSharedParamWhitelist` (D27 O7): источник из трёх строк — REALITY-ссылка 3x-ui с `spx&pqv&support-x25519mlkem768=true`, xhttp-ссылка с `extra&fm`, ссылка с `foo=1` → в выходе ровно первые две.
  - `TestRefreshPassesUserAgent`: фейковый `Input` видит `o.UserAgent` из `fetchUserAgent` (и `""` без ключа).
  - `TestRefreshDockerExampleMatchesReference`: конфиг `docker/sfc/config.example.yml` (реальный файл фикстуры) → `Report` (кроме `refreshedAt`) и `LastGoodBase64` каждого output равны `testdata/reference/sfc/*`.
  - `TestReportSortedByID` (`["Z1","_a","a","b"]` → порядок `strings.Compare`).
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/sfc/...` → PASS.
- [ ] **Step 6: Commit** `feat(sfc): port refresh pipeline, output state and source cache`.

---

### Task 3.3: HTTP-обработчик sfc

**Read:** `04` §5 (весь), §11 тесты 5–12; `testdata/reference/sfc/server-cases.json`.

**Files:**
- Create: `internal/sfc/server/server.go`, `server_test.go`

**Interfaces:**
- Consumes: `refresh.State.Lookup`.
- Produces: `func New(st *refresh.State) http.Handler`. Маршруты: `/healthz` → 200 `ok\n`; найден output с `LastGoodBase64` → 200 тело base64; найден без него → 502 `output is not available yet\n`; иначе 404 `not found\n`; всё `text/plain; charset=utf-8`; метод не проверяется. Профильные заголовки: если UA непуст, у output есть `UserAgent` и `strings.HasPrefix(ua, p)` для какого-то p → `profile-title: base64:<StdEncoding(title)>` (если title задан) и `profile-update-interval: <n>` (если задан). Имена заголовков записывать через `w.Header()["profile-title"] = …` (нижний регистр, как в TS).

- [ ] **Step 1: Failing tests** (п.5–12 + эталон): `TestHealthz`, `TestUnknownIDUnderRoute404`, `TestNoPayload502`, `TestPayloadNoUA`, `TestProfileHeadersOnUAPrefix` (`ClashMeta/1.0` → `base64:QXZhaWxhYmxlIFByb2ZpbGU=`, `6`), `TestNoHeadersOnUAMismatch`, `TestNoHeadersWithoutUserAgentConfig`, `TestOtherRoute404`, `TestDisabledOutput502`, `TestServerCasesReference` (каждый кейс `server-cases.json`: статус, тело, заголовки профиля).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/sfc/...` → PASS.
- [ ] **Step 5: Commit** `feat(sfc): port public output HTTP handler`.

---

### Task 3.4: `scd sfc serve` и `scd sfc refresh`

**Read:** `04` §2 (CLI), §4.8, §4.9, §7.1 (`config_loaded`, `server_started`, `refresh_tick*`, `refresh_failed`), §11 тесты 1–4; `07` D7; `internal/common/cronloop` (1.4).

**Files:**
- Create: `internal/sfc/cli/serve.go`, `refresh.go`, `serve_test.go`
- Modify: `internal/sfc/cli/cli.go`

**Interfaces:**
- Consumes: `config.Load`, `refresh.Refresher`, `server.New`, `cron.Parse`, `cronloop.RunLogged`, `httpserve`, `logging`.
- Produces: `func RunServe(ctx context.Context, cfgPath string, stderr io.Writer) error`: config → logger → `config_loaded` → начальный `Refresher.Run` → `Listen`+`server_started` (`listen`) → параллельно `httpserve.Run` и cron-цикл, где **первый немедленный тик cronloop пропускается** (обёртка задачи игнорирует самый первый вызов), тик логирует `refresh_tick` и вызывает `Run`; overrun → `refresh_tick_skipped_overrun`; ошибка → `refresh_failed`. По `ctx.Done()` всё останавливается сразу (D7), активный refresh дожидается. `refresh` команда: один `Refresher.Run`, `jsonout.Write(report)`, `report.Failed>0` → `cliutil.ErrReportFailed` (код 1).

- [ ] **Step 1: Failing tests:**
  - `TestServeInitialRefreshThenListen` (HTTP отвечает 200 на `/s/test-all` сразу после `server_started`; ровно один `Refresher.Run` до первого слота cron)
  - `TestServeStopsImmediately` (cancel → возврат < 1s при расписании `0 0 1 1 *`)
  - `TestRefreshCommandExitCode` (failed>0 → `ErrReportFailed`, stdout — JSON отчёта)
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Verify:**
  ```bash
  go test -race ./... && golangci-lint run && go build -o bin/scd ./cmd/scd
  ./bin/scd sfc refresh --config docker/sfc/config.example.yml | jq '.successful'   # 3
  ```
  (если `.sfc-cache` создался в `docker/sfc/` — он git-ignored, удалить).
- [ ] **Step 5: Commit** `feat(sfc): add scd sfc serve and refresh commands` (body: D7, D10).
