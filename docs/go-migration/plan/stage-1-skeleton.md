# Этап 1. Каркас модуля и `internal/common`

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. TDD (superpowers:test-driven-development) обязателен: тест → RED → код → GREEN → commit. Перед задачей прочитать «Глобальные ограничения» и «Соглашения» в `docs/go-migration/plan/README.md`.

**Goal:** Go-модуль `github.com/a1ekseev/scd` с бинарём `bin/scd`, деревом cobra-команд, CI-джобой Go (параллельно существующим Node-джобам) и общими пакетами, которые нужны всем трём сервисам.

**Architecture:** `cmd/scd/main.go` → `internal/cli.Execute`. Общие пакеты маленькие и независимые; каждый — отдельная задача с тестами. Только то, что действительно одинаково в scd и sfc (и gtl), уходит в `internal/common` (D10).

**Tech Stack:** Go 1.27.2, `github.com/spf13/cobra@latest`, `go.yaml.in/yaml/v3@latest`, `github.com/robfig/cron/v3@v3.0.1`, stdlib.

**Spec:** `07` D1, D7, D8, D9, D10, D18, D23, D24, D26, D27, D28, D33; `11-xray-vless-matrix.md` §4, §4.1 (задача 1.7); `12-real-subscriptions.md` §2, §3, §9 п.5 (задачи 1.6, 1.7); `06` §2–§6, §10–§12 (с поправками D9/D10); `01` §2–§5; `03` §3; `04` §3–§4.

## Контекст этапа

- Пакеты сервисов пока пустые-заглушки: `internal/scd/cli`, `internal/sfc/cli`, `internal/gtl/cli` возвращают команды без логики (наполняются в этапах 2–6).
- Тексты ошибок — Go-стиль (строчная буква, без точки в конце), кроме мест, где план указывает текст явно.
- Все общие пакеты не логируют сами и не читают глобальные переменные окружения, кроме `configload` (env-интерполяция).

---

### Task 1.1: Модуль, корневая команда, архитектурный тест, lint, CI-джоба Go

> **Поправка D32:** создать скелет `docs/CONFIG-MIGRATION.md` (разделы из D32, пока пустые) — последующие задачи дописывают его.

**Read:** `07` D1, D10; `06` §2 (cobra), §11 (CI, `.golangci.yml`), §12; `.github/workflows/docker.yml`; `.gitignore`.

**Files:**
- Create: `go.mod` (`module github.com/a1ekseev/scd`, `go 1.27.0`, `toolchain go1.27.2`), `go.sum`
- Create: `cmd/scd/main.go` — `func main() { buildinfo.Version = version; os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr)) }`, переменная `var version = "dev"` (ldflags `-X main.version=…`)
- Create: `internal/common/buildinfo/buildinfo.go`, `buildinfo_test.go` — `var Version = "dev"`; `func UserAgent() string` → `"scd/" + Version` (D27 O9: дефолтный User-Agent загрузки подписок; общий пакет, т.к. сервисы не могут импортировать `internal/cli`)
- Create: `internal/cli/root.go`, `internal/cli/root_test.go`
- Create: `internal/common/cliutil/cliutil.go` (`var ErrReportFailed = errors.New("report has failed items")` — sentinel «отчёт напечатан, но есть failed»; сервисные пакеты не могут импортировать `internal/cli`)
- Create: `internal/scd/cli/cli.go` (`func Commands() []*cobra.Command` → пока пусто), `internal/sfc/cli/cli.go` (`func New() *cobra.Command` → `Use: "sfc"`, Short `Filter upstream subscriptions and serve them by public URL.`), `internal/gtl/cli/cli.go` (`func New() *cobra.Command` → `Use: "gtl"`, Short `Gen Test Load`)
- Create: `internal/archtest/imports_test.go`
- Create: `.golangci.yml`
- Modify: `.gitignore` — добавить `/bin/`
- Modify: `.github/workflows/docker.yml` — новая джоба `go` (условие как у `verify`), Node-шаги не трогать

**Interfaces:**
- Produces: `package cli` — `func NewRoot() *cobra.Command` (Use `scd`, Short `Generate and apply Xray resources from subscriptions.`, `SilenceUsage: true`, `SilenceErrors: true`, `Version: buildinfo.Version` (флаг `--version` только у корня; у `scd sfc`/`scd gtl` своего нет — D27 O10), шаблон версии `{{.Version}}\n`, completion отключён); `func Execute(args []string, stdout, stderr io.Writer) int` — 0 при успехе, 1 при любой ошибке (печатает `err.Error()+"\n"` в stderr). Если ошибка `errors.Is(err, cliutil.ErrReportFailed)` — `Execute` возвращает 1, ничего не печатая.
- Produces: корень монтирует `scdcli.Commands()...`, `sfccli.New()`, `gtlcli.New()`.

- [ ] **Step 1: Failing tests** в `internal/cli/root_test.go`:
  - `TestHelpListsServiceCommands`: `Execute([]string{"--help"}, &out, &errb)` == 0; `out` содержит `sfc` и `gtl`.
  - `TestUnknownCommandFails`: `Execute([]string{"nope"}, …)` == 1; `errb` содержит `unknown command "nope"`.
  - `TestVersionFlag`: `buildinfo.Version="1.2.3"`; `Execute([]string{"--version"})` == 0, `out.String()=="1.2.3\n"`; `Execute([]string{"gtl","--version"})` == 1 (`unknown flag: --version`).
  - `internal/common/buildinfo` `TestUserAgent`: `Version="1.2.3"` → `scd/1.2.3`; по умолчанию `scd/dev`.
  - `TestReportFailedExitCode`: команда-заглушка, возвращающая `cliutil.ErrReportFailed`, даёт код 1 и пустой stderr.
  - `internal/archtest/imports_test.go` `TestServicesDoNotImportEachOther`: обходит `internal/scd`, `internal/sfc`, `internal/gtl` (`go/parser` с `parser.ImportsOnly`), ни один файл не импортирует `github.com/a1ekseev/scd/internal/<другой сервис>`; `internal/common/...` не импортирует ни один сервис.
- [ ] **Step 2: Run** `go test ./...` → FAIL (нет пакетов/функций).
- [ ] **Step 3: Реализация** `go.mod` (`go get github.com/spf13/cobra@latest`), `root.go`, заглушки, `main.go`.
- [ ] **Step 4: `.golangci.yml`** (v2): `version: "2"`; `linters.default: standard`; `linters.enable: [errorlint, gocritic, revive, bodyclose, contextcheck, noctx, modernize, usestdlibvars, perfsprint, misspell, unconvert]`; `linters.exclusions.presets: [std-error-handling]`; `formatters.enable: [gofumpt, goimports]`; `formatters.settings.goimports.local-prefixes: [github.com/a1ekseev/scd]`; `run.build-tags: [xraylive, visual]`. Проверка: `golangci-lint config verify` → без ошибок (ревью проверило ровно этот набор на v2.15.0). Все пакеты задачи — с `// Package … ` комментарием, экспортируемые идентификаторы — с doc-комментариями (revive `package-comments`/`exported`, см. «Lint» в README плана).
- [ ] **Step 5: CI-джоба `go`** в `.github/workflows/docker.yml`: `actions/checkout@v7` → `actions/setup-go@v7` (`go-version-file: go.mod`, `cache: true`) → `go mod verify` → `go vet ./...` → `golangci/golangci-lint-action@v9` (`version: v2.15.0`) → `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` → `go test -race -count=1 ./...` → `go build -o bin/scd ./cmd/scd` → `./bin/scd --help`.
- [ ] **Step 6: Verify:** `go vet ./... && go test -race ./... && golangci-lint fmt && golangci-lint config verify && golangci-lint run && go build -o bin/scd ./cmd/scd && ./bin/scd --help && ./bin/scd sfc --help && ./bin/scd gtl --help`. Expected: всё зелёное, help печатается. CI-шаг govulncheck локально не воспроизводится (403 на `vuln.go.dev` из песочницы) — проверяется только в CI; в CI `go run …govulncheck@latest` собирается toolchain'ом 1.27.2 от setup-go и работает (локально без `GOTOOLCHAIN=go1.27.2` он собрался бы go1.26 и упал бы — см. README плана).
- [ ] **Step 7: Commit** `feat(cli): bootstrap Go module, root command and Go CI job` (body: D1, D10).

---

### Task 1.2: `jsonout`, `listenaddr`, `regexlit`

**Read:** `07` D8, D9; `01` §2.3 п.7 (listen), §9 (regex); `04` §3.2 (listen), §4.5; `05` §3 (listen).

**Files:**
- Create: `internal/common/jsonout/jsonout.go`, `jsonout_test.go`
- Create: `internal/common/listenaddr/listenaddr.go`, `listenaddr_test.go`
- Create: `internal/common/regexlit/regexlit.go`, `regexlit_test.go`

**Interfaces:**
- Produces: `jsonout.Write(w io.Writer, v any) error` — отступ 2 пробела, `SetEscapeHTML(false)`, завершающий `\n`. `jsonout.Marshal(v any) ([]byte, error)` — то же без `\n` (тела HTTP).
- Produces: `listenaddr.Parse(s string) (host string, port int, err error)` через `net.SplitHostPort` + `strconv.Atoi`; ошибки: `ErrFormat` (текст `listen address must be in "host:port" format`), `ErrPort` (`listen address port must be an integer between 1 and 65535`). Пустой host допустим (`:8080` → все интерфейсы) — **изменение относительно TS** (там `:80` был невалиден); IPv6 только в скобках.
- Produces: `regexlit.Compile(s string) (*regexp.Regexp, error)` (D8): строка, начинающаяся с `/` и содержащая ещё один `/` после позиции 0, — литерал `/pattern/flags`; флаги ⊆ `{i,m,s}` без повторов → префикс `(?flags)`; иной флаг → ошибка `unsupported regex flag "g" (only i, m, s are allowed)`; `/` без закрывающего → `regex literal must end with "/flags"`; иначе вся строка — RE2-паттерн. Ошибка компиляции с `(?=`, `(?!`, `(?<=`, `(?<!` или `\1`…`\9` → `lookahead, lookbehind and backreferences are not supported (RE2 syntax): <исходная ошибка>`; прочие — текст `regexp`.

- [ ] **Step 1: Failing tests:**
  - `TestWriteNoHTMLEscape`: `Write(&b, map[string]string{"a":"<&>"})` → `"{\n  \"a\": \"<&>\"\n}\n"`.
  - `TestParseListen` (таблица): `127.0.0.1:9090`→(`127.0.0.1`,9090,nil); `[::]:8080`→(`::`,8080); `:8080`→(``,8080); `localhost`→ErrFormat; `h:0`/`h:65536`/`h:0x50`/`h: 80`→ErrPort.
  - `TestCompileLiteral`: `/Германия/i` матчит `🇩🇪 Франкфурт, германия`; `/a/b/i` → pattern `a/b`; `/,\s*Extra\s*$/i` матчит `X, extra`; `.*` (без слэшей) матчит всё.
  - `TestCompileRejects`: `/x/g`, `/x/ii`, `/x/y`, `/(/i`, `/broken`, `/a(?!b)/`, `/(a)\1/` → ошибки с ожидаемыми подстроками (`unsupported regex flag`, `must end with`, `lookahead`).
  - `TestMatchIsSubstringSearch`: `/Extra/` матчит `a Extra b` (D8 «MatchString»).
- [ ] **Step 2: Run** `go test ./internal/common/...` → FAIL.
- [ ] **Step 3: Implement** три пакета.
- [ ] **Step 4: Run** `go test -race ./internal/common/... && golangci-lint run` → PASS.
- [ ] **Step 5: Commit** `feat(common): add jsonout, listenaddr and regexlit packages` (body: D8, D9) — вместе с пунктами `docs/CONFIG-MIGRATION.md` (D32; ревью-2 F05): `listen` (`statusServer.listen` scd, `server.listen` sfc/gtl) — `:8080` теперь допустим (все интерфейсы), IPv6 только в скобках; regex-фильтры — RE2, `/pattern/flags` с `i,m,s` или голый паттерн (D8).

---

### Task 1.3: `configload` и `schema`

> **Поправка D29/D30 (обязательна; уточнена ревью-2, F03):** фильтр меток — `labelIncludeRegex` (строка или список, AND) + новый `labelExcludeRegex` (строка или список, OR). Пакет `internal/common/labelfilter` создаётся **в этой задаче** (после `schema`, т.к. `Parse` пишет issues в `schema.Node`):
> - `type Filter struct{ Include, Exclude []*regexp.Regexp; IncludeSrc, ExcludeSrc []string }`; `func Parse(n *schema.Node) Filter` — читает ключи `labelIncludeRegex`/`labelExcludeRegex` узла (строка → список из одного элемента; пустые после trim элементы и пустой список — issue `must not be empty`; ошибки `regexlit.Compile` — issue по пути `<…>.labelIncludeRegex.<i>` / `.labelExcludeRegex.<i>`, для строки — без индекса); `func (f Filter) Match(label string) bool` — все Include совпали **и** ни один Exclude не совпал (`MatchString`, D8); `func (f Filter) Empty() bool`; `func (f Filter) MarshalJSON()` — `{"labelIncludeRegex":[…]?,"labelExcludeRegex":[…]?}` из `*Src` (для `print-config`).
> - Файлы: Create `internal/common/labelfilter/labelfilter.go`, `labelfilter_test.go`.
> - Тесты (здесь и только здесь; в 3.1/4.1 не дублируются): `TestLabelFilterRealShapeEquivalence` — для корпуса `testdata/reference/common/label-filter.json` (0.6) решения Go-формы фильтров из D29 совпадают с решениями TS-регулярок на всех метках, кроме `allowedDiffs = {"Extra\nWhiteList": "JS . не матчит \\n: TS extraWhitelist=false, Go include [/Extra/i,/WhiteList/i] = true (12 §6.3); Go-поведение не меняется"}` (тест также падает, если метка из `allowedDiffs` на деле совпала); `TestLabelFilterStringOrList`, `TestLabelFilterIssuePaths`, `TestLabelFilterLookaheadRejected` (обычная ошибка `regexlit`, D31 — без спец-подсказок).
> - Проверка «Go-форма `docker/<svc>/config.real-shape.go.yml` проходит `validate-config`» здесь **невозможна** (конфиги и команды появляются в 2.1/3.1/4.1) — она перенесена в `TestGTLRealShapeGoFormValid` (2.1), `TestSFCRealShapeGoFormValid` (3.1), `TestSCDRealShapeGoFormValid` (4.1). Совместимость со старой формой не требуется (D31).

**Read:** `01` §2.1–§2.4 (смысл правил; тексты zod не повторять — D9); `04` §3, §3.1, §3.3; `05` §3; `06` §4–§5; `07` D9.

**Files:**
- Create: `internal/common/configload/configload.go`, `configload_test.go`
- Create: `internal/common/schema/schema.go`, `schema_test.go`

**Interfaces:**
- Produces (`configload`):
  - `type EnvMode int` с константами `EnvNone` (gtl), `EnvRawStrict` (scd: подстановка `${NAME}` (regex `(?i)\$\{([A-Z0-9_]+)\}`) по сырому тексту до YAML, отсутствующая → ошибка `missing environment variable "NAME" required by config`), `EnvStringsLenient` (sfc: после разбора YAML, только в строковых значениях, отсутствующая → `""`).
  - `func Load(path string, mode EnvMode, lookup func(string) (string, bool)) (Doc, error)`; `type Doc struct { Path string /*абсолютный*/; Dir string; Root any /* map[string]any | nil→map{} */ }`. YAML — `go.yaml.in/yaml/v3` в `any`; дубликат ключа → ошибка YAML; несколько документов → ошибка `config must contain a single YAML document`.
  - `func ResolveInput(dir, input string) string`: `-` и `^(?i)https?://` — как есть; абсолютный — как есть; иначе `filepath.Join(dir, input)`.
- Produces (`schema`) — маленький валидатор поверх `any`:
  - `type Issue struct{ Path, Message string }`; `type Issues []Issue`; `func (is Issues) Err(file string) error` → `nil` если пусто, иначе ошибка с текстом `invalid config <file>:` + по строке `\n  - <path>: <message>` (пустой путь → `<root>`).
  - `type Node struct{…}`; `func Root(v any, issues *Issues) *Node`; методы (все nil-safe, ошибки типа/отсутствия пишут Issue с путём `a.b.0.c`): `Object(key string, required bool) *Node`, `Array(key string, required bool, minItems int) []*Node`, `String(key string, o StrOpt) (string, bool)` (`StrOpt{Required, Trim, MinLen, Default *string, EmptyToAbsent bool}`), `StringList(key string, o StrOpt) []string`, `Int(key string, o IntOpt) int` (`IntOpt{Required bool; Default, Min, Max *int}`; только YAML `!!int`; `5000.0` → issue `expected integer, got float` — JS-нюанс «число с целым значением» не переносится, D9/D31; ревью-2 F22), `Bool(key string, def bool) bool` (только YAML bool; `yes` → issue), `Enum(key string, allowed []string, def string) string`, `URL(key string, required bool) (string, bool)` (`net/url.Parse` + непустые scheme и host), `Strict(known ...string)` (неизвестные ключи → `unknown key "x"`), `Has(key) bool`, `Path() string`, `Addf(key, format string, args ...any)`, `StringMap(key string) map[string]string` (D33 `fetchHeaders`: отсутствует → `nil`; не объект → `expected object, got <type>`; значение не строка → issue по пути `<key>.<имя>` `expected string, got <type>`; ключи — как в YAML, порядок не важен).
  - Сообщения: `required`, `expected <type>, got <type>`, `must be at least N`, `must be at most N`, `must be one of a|b|c`, `must not be empty`, `invalid URL`, `unknown key "x"`.

- [ ] **Step 1: Failing tests (`configload`):**
  - `TestLoadRawStrictInterpolatesBeforeYAML`: `a: ${X}` с X=`true` → `Root["a"]==true` (bool); `# ${NOPE}` в комментарии → ошибка `missing environment variable "NOPE"`.
  - `TestLoadLenientInterpolatesStringsOnly`: `a: "${X}"`, `n: 5`, X не задан → `a==""`; ключ `${X}` не трогается.
  - `TestLoadNoneKeepsLiteral`: `a: ${X}` → `"${X}"`.
  - `TestLoadDuplicateKeyFails`, `TestLoadEmptyFileIsEmptyMap`, `TestLoadMultiDocFails`, `TestLoadMissingFileError` (`errors.Is(err, fs.ErrNotExist)`).
  - `TestResolveInput`: `-`, `HTTP://x`, `/abs`, `rel/a.txt`, `ftp://x/y` (→ `<dir>/ftp:/x/y`).
- [ ] **Step 2: Failing tests (`schema`):** `TestRequiredMissing` (путь `subscriptions.0.id`), `TestWrongType`, `TestTrimAndMin`, `TestIntBounds`, `TestFloatRejected` (`5000.0` → issue `expected integer`), `TestBoolRejectsYesString`, `TestEnumDefault`, `TestStrictUnknownKey`, `TestIssuesErrFormat` (точный текст двух issues), `TestStringMap` (`{x-hwid: abc}` → `map[x-hwid:abc]`; `{x-n: 5}` → issue `fetchHeaders.x-n: expected string, got int`; `fetchHeaders: [a]` → `expected object`).
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/common/... && golangci-lint run` → PASS.
- [ ] **Step 6: Commit** `feat(common): add config loading and schema validation helpers`.

---

### Task 1.4: `cron` и `cronloop`

**Read:** `07` D7, D23 (Cron), **D24**; `03` §3.2–§3.5, §10.1; `04` §3.4, §4.8, §11 п.1–4; `06` §3; `testdata/reference/common/cron-next.json`, `cron-rejected.json` (из 0.2).

**Files:**
- Create: `internal/common/cron/cron.go`, `cron_test.go`
- Create: `internal/common/cronloop/cronloop.go`, `cronloop_test.go`

**Interfaces:**
- Produces (`cron`): `type Schedule interface{ Next(time.Time) time.Time }`; `func Parse(expr string) (Schedule, error)` — классический Linux crontab (D24): 5 полей `мин час день месяц день_недели` или 6 с ведущими секундами; списки, диапазоны, шаги, имена месяцев/дней (регистр не важен); макросы `@yearly @annually @monthly @weekly @daily @midnight @hourly`. Реализация (D28) — библиотека `robfig/cron/v3` `NewParser(SecondOptional|Minute|Hour|Dom|Month|Dow|Descriptor)` **как есть**: что она принимает (вкл. `?`, `@every`, `TZ=`/`CRON_TZ=`) — валидно, что отвергает — ошибка с её текстом. Своих синтаксических ограничений не добавляем. Только две защиты: `""` (`cron expression must not be empty`) и выражения, которые никогда не срабатывают: `s.Next(time.Date(2026,1,1,0,0,0,0,time.UTC)).IsZero()` → `cron expression never matches` (robfig принимает `0 0 31 2 *` и возвращает нулевое `Next` — проверено ревью; без проверки `cronloop` ушёл бы в цикл без сна); часовой пояс — `time.Local`.
- Produces (`cronloop`):
  - `func Run(ctx context.Context, s cron.Schedule, task func(context.Context) error, h Hooks)`; `type Hooks struct{ OnError func(error); OnOverrun func(scheduledAt time.Time) }`. Алгоритм `03` §3.2: первый тик сразу; `scheduledAt = s.Next(prevScheduled)`; если `scheduledAt.IsZero()` — новых тиков не планировать, ждать `ctx.Done()` (защита от busy-loop; `Parse` такие выражения уже отвергает); сон через `time.NewTimer` с `select` на `ctx.Done()` (D7: выход сразу, не ждать слот); занятый тик → `OnOverrun`, слот пропускается; при выходе ждёт активную задачу. Задача получает `ctx` (отмена — ответственность задачи).
  - `func RunLogged(ctx context.Context, s cron.Schedule, task func(context.Context) error, log *slog.Logger, ev Events, attrs ...any)`; `type Events struct{ OverrunEvent, OverrunMsg, FailureEvent, FailureMsg string }`. Overrun → `log.Warn(OverrunMsg, attrs..., "event", OverrunEvent, "scheduledAt", <RFC3339 millis UTC>)`; ошибка → `log.Error(FailureMsg, attrs..., "event", FailureEvent, "error", err.Error(), ["causes", []string])`.
  - `func Causes(err error) []string` — сообщения из `interface{ Unwrap() []error }` (непустые), иначе `nil`.

- [ ] **Step 1: Failing tests (`cron`):**
  - `TestParseMatchesReferenceNext`: для каждой записи `cron-next.json` (в `TestMain` установить `time.Local = time.UTC` — эталоны сняты при `TZ=UTC`) три последовательных `Next` равны эталону.
  - `TestParseRejects`: каждая строка `cron-rejected.json` → ошибка (включая `0 0 31 2 *` и `0 0 30 2 *` → `never matches`); `TestParseAcceptsLibrarySyntax`: `0 0 ? * *`, `@every 5m`, `CRON_TZ=UTC 0 0 * * *` принимаются (D28).
  - `TestParseLinuxMacros` (D24): все 7 макросов принимаются; `@midnight` и `@daily`, `@annually` и `@yearly` дают одинаковые три `Next` от `2026-01-01T00:00:00Z`.
  - `TestParseClassicForms` (D24): `1-30/5 * * * *`, `5 4 * jan,jul sun`, `0 9 * * mon-fri`, `0 0 29 2 *` (следующий — `2028-02-29T00:00:00Z`), шестиполевое `*/15 * * * * *` (следующий от `…00:00:00Z` — `…00:00:15Z`) → ок.
- [ ] **Step 2: Failing tests (`cronloop`, `testing/synctest`):**
  - `TestRunSkipsOverrunWithoutDrift` (`03` §10.1 п.1): расписание каждые 100 мс (фейковый `Schedule`), первый тик блокируется до первого overrun → старты `[0ms, 200ms]`, overrun один на `100ms`.
  - `TestRunStopsImmediatelyOnCancel` (D7): слот через 1 час, `cancel()` → `Run` возвращается без продвижения часов дальше `0`.
  - `TestRunWaitsForActiveTask`: cancel во время задачи → `Run` возвращается после завершения задачи.
  - `TestRunZeroNextDoesNotSpin`: фейковый `Schedule`, у которого `Next` возвращает `time.Time{}` → ровно один (первый) запуск задачи, затем `Run` спит до `cancel()`.
  - `TestCausesFromJoin` (`03` §10.1 п.2): `errors.Join(errors.New("first cause"), errors.New("second cause"))` → `["first cause","second cause"]`.
  - `TestRunLoggedOverrunLog` / `TestRunLoggedFailureLogsCauses` (`03` §10.1 п.3–4, `04` §11 п.3–4): JSON-handler в буфер; overrun → warn с `event`, `subscriptionId`, `scheduledAt`; ошибка → ровно один error с `causes`; loop продолжает работу.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement** (`go get github.com/robfig/cron/v3@v3.0.1`). **Step 5: Run** `go test -race ./internal/common/...` → PASS.
- [ ] **Step 6: Commit** `feat(common): add cron parser and cron loop with immediate shutdown` (body: D7, D23, D24, D28) — вместе с разделом «Cron» `docs/CONFIG-MIGRATION.md` (D32; ревью-2 F05): что принимает/отвергает `robfig/cron/v3` по сравнению с `cron-parser` (`@midnight`/`@annually`/`?`/`@every`/`CRON_TZ=` — да; `L/W/#/H`, dow `7`, `@minutely|@secondly|@weekdays|@weekends` — ошибка; никогда не срабатывающие — ошибка).

---

### Task 1.5: `logging` и `httpserve`

**Read:** `07` D7, D9, D23 (panic → 500), D27 (O10); `03` §6.1, §8; `04` §5.1, §7; `05` §5–§6.

**Files:**
- Create: `internal/common/logging/logging.go`, `logging_test.go`
- Create: `internal/common/httpserve/httpserve.go`, `httpserve_test.go`

**Interfaces:**
- Produces (`logging`): `func New(w io.Writer, level, format string) (*slog.Logger, error)`; level ∈ `fatal|error|warn|info|debug|trace|silent` (trace = `slog.Level(-8)`, fatal = `slog.Level(12)`, silent → `slog.DiscardHandler`); format `json` → `slog.JSONHandler`, `pretty` → `slog.TextHandler` (человекочитаемый `key=value`), иное → ошибка `unsupported log format "<f>" (expected json or pretty)`; `ReplaceAttr` переименовывает уровни −8/12 в `TRACE`/`FATAL`. D27 O10: все три сервиса создают логгер **только** как `logging.New(os.Stderr, cfg.Logging.Level, cfg.Logging.Format)`; stdout занят JSON-выводом команд. Соглашение для всех сервисов: `log.Info("<Human message.>", "event", "<event_name>", …)` — `event` первым атрибутом.
- Produces (`httpserve`):
  - `func Recover(log *slog.Logger, h http.Handler) http.Handler` — panic → 500 `text/plain; charset=utf-8` тело `internal server error\n`, лог `error` с `event` `http_handler_panic`, `panic`, `path`.
  - `func Run(ctx context.Context, ln net.Listener, h http.Handler, shutdownTimeout time.Duration) error` — `http.Server{Handler: h, ReadHeaderTimeout: 60*time.Second}` (без `WriteTimeout`: gtl отдаёт большие тела); при `ctx.Done()` → `Shutdown` с таймаутом; возвращает `nil` при штатной остановке.
  - `func Listen(addr string) (net.Listener, error)` — `net.Listen("tcp", addr)`.
  - `func CleanPath(escaped string) string` (ревью-2 F01; используют gtl 2.2 и sfc 3.3) — удаление dot-сегментов (`path.Clean`) **без** декодирования `%xx` и **с сохранением хвостового `/`** (как `pathname` WHATWG URL у TS: `/load/` → `/load/`, `/load/1/` → `/load/1/`, `/a/../load/1` → `/load/1`, `/s/../s/test-all` → `/s/test-all`, `""` → `/`).

- [ ] **Step 1: Failing tests:** `TestNewLevels` (trace пишет при level `trace`, не пишет при `debug`; `silent` ничего не пишет), `TestNewJSONHasEventAttr`, `TestNewPrettyIsText` (строка содержит `level=INFO` и `event=x`, не начинается с `{`), `TestNewRejectsUnknownLevel`, `TestNewRejectsUnknownFormat` (`text` → ошибка); `TestRecoverReturns500` (handler с panic → 500, лог содержит `http_handler_panic`), `TestRunShutsDownOnCancel` (запрос в процессе завершается, `Run` возвращает nil после cancel), `TestRunReturnsServeError`, `TestCleanPathKeepsTrailingSlash` (таблица из Interfaces).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/common/...` → PASS.
- [ ] **Step 5: Commit** `feat(common): add slog logger factory and HTTP serve helpers` (body: D7, D9, D23, D27) — вместе с разделом «Логи» `docs/CONFIG-MIGRATION.md` (D32; ревью-2 F05): все сервисы — stderr, `json` = slog JSON (`time` RFC3339, `level` строкой, `msg`, `event` + поля; вместо pino `level`-числа и `time` в мс), `pretty` = `slog.TextHandler`; уровни прежние.

---

### Task 1.6: `inputsrc` и `subscription` (загрузка источника, base64, строки)

**Read:** `01` §3, §4, §17.1 P11–P18; `04` §4.1–§4.3; `07` D9, D27 (O9), D33 (`fetchHeaders`); `12-real-subscriptions.md` §1 (заголовки HWID Remnawave, зачем нужна опция).

**Files:**
- Create: `internal/common/inputsrc/inputsrc.go`, `inputsrc_test.go`, `headers.go`, `headers_test.go`
- Create: `internal/common/subscription/lines.go`, `lines_test.go`

**Interfaces:**
- Produces (`inputsrc`):
  - `type Options struct{ Format string /* auto|plain|base64, "" = auto */; FetchTimeout time.Duration /* 0 → 5s */; UserAgent string /* "" → buildinfo.UserAgent() = "scd/<version>" (D27 O9) */; Headers Headers /* D33 fetchHeaders, только для http(s) */; Stdin io.Reader; HTTPClient *http.Client }`
  - (`headers.go`, D33) `type Headers map[string]string` — заголовки запроса подписки, **значения — секрет**: `func (h Headers) MarshalJSON() ([]byte, error)` пишет объект с теми же ключами (по `slices.Sorted`) и значением `"<redacted>"` у каждого (так `print-config` и любой случайный JSON-вывод не раскрывают значения); `func (h Headers) LogValue() slog.Value` — группа `ключ=<redacted>` (`slog.LogValuer`); `func (h Headers) Plain() map[string]string` — копия с настоящими значениями (только для HTTP-запроса и runtime-state при `includeSecrets: true`); `func CheckHeaders(h map[string]string) []HeaderIssue`, `type HeaderIssue struct{ Key, Message string }`: имя — HTTP-token по RFC 9110 (`httpguts.ValidHeaderFieldName` из `golang.org/x/net/http/httpguts`, иначе `invalid header name`); значение непустое после trim и без `\r`/`\n`/`\x00` (`must not be empty` / `must not contain control characters`); `User-Agent` (любой регистр) → `use fetchUserAgent instead`; `Host`, `Content-Length`, `Transfer-Encoding`, `Connection` → `header is not allowed`; два ключа, равные без учёта регистра → `duplicate header (case-insensitive)`. Конфиги scd/sfc (4.1, 3.1) превращают `HeaderIssue` в `schema`-issue по пути `subscriptions.N.fetchHeaders.<Key>`.
  - HTTP-запрос: сначала `Headers` (`req.Header.Set(k, v)`), затем `User-Agent` — так `fetchUserAgent` нельзя перебить. Значения заголовков **никогда** не попадают в тексты ошибок `Load` (там только URL/статус/таймаут).
  - `type Loaded struct{ Source, Content, Encoding string /* plain|base64 */ }`
  - `func Load(ctx context.Context, input string, o Options) (Loaded, error)`: `-` → stdin, Source `stdin`; `^(?i)https?://` → GET (редиректы по умолчанию Go; таймаут — `context.WithTimeout` на запрос+тело), Source = исходный URL; иначе файл (`filepath.Abs`), Source = абсолютный путь. У всех источников снимается ведущий U+FEFF. Пустой (после `strings.TrimSpace`) до и после декодирования → `ErrEmpty`, текст `input source <src> returned empty content`. HTTP не-2xx → `unable to download input from <url>: <resp.Status>`; таймаут → `input source <url> timed out after <N>ms`.
  - `func DecodeAuto(raw string) (content string, decoded bool)`: trimmed содержит `://` → как есть; иначе удалить все пробельные (`unicode.IsSpace`), пробовать `base64.StdEncoding`, `RawStdEncoding`, `URLEncoding`, `RawURLEncoding`; ошибка или результат без `://` → как есть. Формат `base64`: то же декодирование, ошибка → `input source <src> is not valid base64: <err>` (D9: без лояльности Node).
- Produces (`subscription`): `type Line struct{ Number int; Raw, Trimmed string }`; `func ScanLines(text string) []Line` — split по `\n`, у `Raw` срезается хвостовой `\r`, `Trimmed = strings.TrimSpace(Raw)`, пустые отфильтрованы, `Number` — индекс до фильтрации (1-based).

- [ ] **Step 1: Failing tests (`inputsrc`):**
  - `TestLoadFilePlain`, `TestLoadFileWithBOM` (BOM снят, Content начинается с `vless://`), `TestLoadFileIgnoresFetchTimeout` (P16).
  - `TestLoadHTTPBase64` (P12; `httptest.Server`; Source = URL, Encoding `base64`).
  - `TestLoadHTTPUserAgent` (O9): без `UserAgent` сервер видит `User-Agent: scd/dev`; с `UserAgent: "Happ/1.0"` — `Happ/1.0`.
  - `TestLoadHTTPSendsFetchHeaders` (D33): `Headers{"x-hwid":"abc","X-Device-OS":"linux"}` → сервер видит `X-Hwid: abc`, `X-Device-Os: linux`; файл/stdin заголовки игнорируют.
  - `TestLoadErrorDoesNotLeakHeaderValues` (D33): сервер отвечает 403 / висит до таймаута → `err.Error()` не содержит `secret-hwid-value`.
  - `TestHeadersRedacted` (D33): `json.Marshal(Headers{"x-hwid":"s3cr3t"})` → `{"x-hwid":"<redacted>"}`; slog JSON-handler с атрибутом `"headers", h` → в выводе нет `s3cr3t`, есть `<redacted>`; `Plain()` возвращает `s3cr3t`.
  - `TestCheckHeaders` (таблица): `x-hwid` ок; `bad name`, `""`-значение, значение с `\n`, `User-Agent`, `host`, пара `X-Hwid`/`x-hwid` → issue с ожидаемой подстрокой.
  - `TestLoadHTTPTimeout` (P13; сервер висит, `FetchTimeout: 10*time.Millisecond` → ошибка содержит `timed out after 10ms`).
  - `TestLoadHTTPNon2xx` (`404 Not Found` в тексте), `TestLoadEmptyRemote` (P14), `TestLoadEmptyFile` (P15, `"   \n"`), `TestLoadStdinBase64` (P17), `TestLoadEmptyStdin` (P18).
  - `TestDecodeAuto` (таблица, P11 + 01 §3.2): std, url-safe, без padding, с переносами каждые 20 символов, с пробелами по краям → decoded; `hello world`, `vless://…` plain, base64 текста без `://`, `!` внутри → не decoded.
  - `TestLoadBase64FormatInvalid` → ошибка `is not valid base64`.
- [ ] **Step 2: Failing tests (`subscription`):** `TestScanLinesNumbersAndCRLF` (`"a\r\n\r\n b \n"` → `[{1,"a","a"},{3," b ","b"}]`), `TestScanLinesTrailingNewline` (P20: одна строка + `\n` → 1 Line), `TestScanLinesLoneCRNotSeparator`.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/common/...` → PASS.
- [ ] **Step 6: Commit** `feat(common): add subscription input loading and line scanning` (body: D9, D27, D33).

---

### Task 1.7: `vless` — разбор строки подписки через `net/url`

**Read:** `01` §5 (смысл проверок и reason-коды), §16 п.4–11, §17.4; `04` §4.4; `09` §1.1 (строки `userinfo`, `host:port`); `11-xray-vless-matrix.md` §4, §4.1; `12-real-subscriptions.md` §2, §4, §9 п.5; `07` D9, D18, D26, D27 (O7), D28, D33; README плана O11, O13, O16.

**Files:**
- Create: `internal/common/vless/parse.go`, `parse_test.go`, `testdata/` не нужен (использовать `testdata/reference/scd/corpus/ts-parse.json` через относительный путь `../../../testdata/reference/...`)

**Interfaces:**
- Consumes: `subscription.Line` (1.6).
- Produces:
  - `type Param struct{ Key, Value string }`
  - `type Link struct{ Line int; Raw, Label, ID, Address string; Port int; Params []Param; Ignored []string }` + `func (l Link) Get(key string) (string, bool)`. `Ignored` — ключи вне `AllowedParams` в порядке появления (D28); в `Params` они **не** попадают; `normalize` (4.3) превращает непустой `Ignored` в warning `param_ignored`. Ключи lowercased; порядок — первого появления; при дубле значение последнего (позиция первого). Address — `u.Hostname()` (IPv6 без скобок).
  - `type Skip struct{ Line int; Raw string; Label *string; Code, Reason string; Details []string }` (JSON-теги `line, raw, label,omitempty, reasonCode, reason, details,omitempty`)
  - Коды: `const (CodeUnsupportedScheme="unsupported_scheme"; CodeInvalidURI="invalid_uri"; CodeMissingRequiredField="missing_required_field"; CodeUnsupportedValue="unsupported_value"; CodeUnsupportedCombo="unsupported_combo"; CodeDuplicateTag="duplicate_tag")`
  - `var AllowedParams = []string{"type","security","encryption","flow","sni","fp","alpn","pcs","vcn","ech","pbk","sid","spx","pqv","allowinsecure","headertype","host","path","mode","extra","fm","cs","heartbeatperiod","servicename","authority","seed","mtu","tti","support-x25519mlkem768","x_padding_bytes"}` — **общий белый список scd и sfc** (D27 O7): параметры стандарта #716 (вкл. grpc/kcp-параметры — чтобы ссылка с `type=grpc` получила понятный `unsupported_combo` в normalize, а не `unsupported_param`) + `cs` (TLS cipherSuites) и `heartbeatperiod` (ws, расширение Remnawave) — D33, 12 §3 + нестандартные параметры генераторов из 11 §4.1 (принимаются и игнорируются — README плана O13). Это список **известных** параметров (документация + тесты). Неизвестные параметры (D28) **игнорируются**: ссылка не отбрасывается, их имена попадают в `Link.Ignored` (дальше — warning `param_ignored`, лог уровня `debug`). Параметры из `AllowedParams`, неприменимые к transport/security (O16), здесь не различаются — их молча отбрасывает normalize (4.3 п.9), warning за них нет.
  - `func Parse(l subscription.Line) (Link, *Skip)`.
- Порядок проверок (reason — дословно, это пользовательский текст отчётов):
  1. нет `://` → `unsupported_scheme` «The line does not contain a supported URI scheme.»
  2. `url.Parse(trimmed)` ошибка → `invalid_uri` «The line is not a valid URI.»
  3. scheme (lowercase) ≠ `vless` → `unsupported_scheme` `Unsupported scheme "<scheme>:".`
  4. label: фрагмент отрезается **до** `url.Parse` — `rest, frag, _ := strings.Cut(trimmed, "#")`, в `url.Parse` идёт `rest`; label = `url.PathUnescape(frag)`; ошибка → `invalid_uri` «The line label is not valid percent-encoding.» (O11). (Если отдать `url.Parse` строку с `#100%`, он сам падает `invalid URL escape "%"` на шаге 2 и O11-текст недостижим — проверено ревью; поэтому шаги 2 и 4 выполняются над `rest` и `frag` соответственно.)
  5. id = `u.User.Username()`; пусто → `missing_required_field` «The VLESS URI does not contain a user id.» (D18; label задан)
  6. host пуст, порт пуст/не число/вне 1..65535 → `missing_required_field` «The VLESS URI does not contain a valid address and port.»
  7. query: свой разбор `u.RawQuery` по `&` (пустые сегменты пропускаются; ключ/значение через `url.QueryUnescape`, при ошибке — сырое значение); ключи вне `AllowedParams` → в `Link.Ignored` (в порядке появления, без дублей); текст warning'а в 4.3 — `Ignored unknown VLESS parameter(s): a, b.` — D28. Код `unsupported_param` больше не выдаётся (константу удалить).

- [ ] **Step 1: Failing tests** `parse_test.go` (таблица):
  - ok: `VLESS://<uuid>@Example.COM:0443?Type=TCP&Security=TLS#x` → Address `Example.COM`, Port 443, Params `[{type,TCP},{security,TLS}]`, Label `x`.
  - ok: `#🇩🇪 Берлин#2` → Label `🇩🇪 Берлин#2`; `#Hello%20World` → `Hello World`; без `#` → `""`.
  - ok: `sni=a&sni=b` → `sni=b`; `path=/a+b%2Fc` → `/a b/c`; `path=/a;x` → `/a;x`; `[2001:db8::1]:443` → Address `2001:db8::1`.
  - ok: id `my-user` (не UUID, D18) и uppercase UUID — без skip.
  - skip: `trojan://x` (scheme), `vless:uuid@h:443` (scheme), `vless://u@ho st:443` (invalid_uri), `vless://@h:443` (user id), `vless://u@h` / `vless://u@h:0` / `vless://u@h:65536` (address and port), `#100%` (O11, invalid_uri).
  - ok (D28): `foo=bar&ech=x&zz=1` → без skip, `Params == [{ech,x}]`, `Ignored == ["foo","zz"]`; `cs=TLS_AES_128_GCM_SHA256&heartbeatPeriod=30` → оба в `Params` (ключ `heartbeatperiod`), `Ignored` пуст.
  - ok: каждый ключ `AllowedParams` (в т.ч. `serviceName` в любом регистре, `support-x25519mlkem768`, `x_padding_bytes`, `fm`, `pqv`, `ech`) — без skip (`TestAllowedParamsAccepted`).
  - `TestParseCorpusAgainstTS`: для каждой строки `ts-parse.json` с `ts.ok==true` — Go тоже ok (на уровне `Parse`); для `ts.reasonCode=="unsupported_scheme"` — тот же код; для `ts.reasonCode=="unsupported_param"` — Go ok (D28: неизвестные параметры игнорируются; при ключах вне `AllowedParams` — warning `param_ignored`). Прочие расхождения — только по списку `allowedDiffs` в тесте с комментарием-причиной (D9/D18/D26/O11).
  - `TestParseRealShapes` (12 §9 п.5): все 54 строки `docker/fixtures/real-shapes/remnawave.txt` + `provider2.txt` (и `provider2.b64` после `inputsrc.DecodeAuto` — те же 50 `Link`) → `Parse` без skip и с пустым `Ignored`; у строк `remnawave.txt` ключи `Params` по порядку = `encryption,type,path,mode,extra,security,sni,fp,alpn`, `path` декодирован (`%2F` → начинается с `/`), `json.Valid([]byte(extra))`; у `provider2.txt` ключи = `type,path,security,alpn`; label `🇩🇪DE-1-X` и `🇨🇴 Colombia, Bogotá` декодированы.
  - `TestParseD26CorpusStageParse`: строки `d26-expect.json` со `stage=="parse"` → skip с тем же `reasonCode`; все остальные строки `d26-links.txt` проходят `Parse` без skip.
- [ ] **Step 2: Run** `go test ./internal/common/vless/` → FAIL.
- [ ] **Step 3: Implement** `parse.go`.
- [ ] **Step 4: Run** `go test -race ./internal/common/... && golangci-lint run` → PASS.
- [ ] **Step 5: Commit** `feat(common): add VLESS share-link parser on net/url` (body: D9, D18, D26, D27, D28, D33).
