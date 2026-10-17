# Этап 1. Каркас модуля и `internal/common`

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. TDD (superpowers:test-driven-development) обязателен: тест → RED → код → GREEN → commit. Перед задачей прочитать «Глобальные ограничения» и «Соглашения» в `docs/go-migration/plan/README.md`.

**Goal:** Go-модуль `github.com/a1ekseev/scd` с бинарём `bin/scd`, деревом cobra-команд, CI-джобой Go (параллельно существующим Node-джобам) и общими пакетами, которые нужны всем трём сервисам.

**Architecture:** `cmd/scd/main.go` → `internal/cli.Execute`. Общие пакеты маленькие и независимые; каждый — отдельная задача с тестами. Только то, что действительно одинаково в scd и sfc (и gtl), уходит в `internal/common` (D10).

**Tech Stack:** Go 1.27.2, `github.com/spf13/cobra@latest`, `go.yaml.in/yaml/v3@latest`, `github.com/robfig/cron/v3@v3.0.1`, stdlib.

**Spec:** `07` D1, D7, D8, D9, D10, D18, D23; `06` §2–§6, §10–§12 (с поправками D9/D10); `01` §2–§5; `03` §3; `04` §3–§4.

## Контекст этапа

- Пакеты сервисов пока пустые-заглушки: `internal/scd/cli`, `internal/sfc/cli`, `internal/gtl/cli` возвращают команды без логики (наполняются в этапах 2–6).
- Тексты ошибок — Go-стиль (строчная буква, без точки в конце), кроме мест, где план указывает текст явно.
- Все общие пакеты не логируют сами и не читают глобальные переменные окружения, кроме `configload` (env-интерполяция).

---

### Task 1.1: Модуль, корневая команда, архитектурный тест, lint, CI-джоба Go

**Read:** `07` D1, D10; `06` §2 (cobra), §11 (CI, `.golangci.yml`), §12; `.github/workflows/docker.yml`; `.gitignore`.

**Files:**
- Create: `go.mod` (`module github.com/a1ekseev/scd`, `go 1.27.0`, `toolchain go1.27.2`), `go.sum`
- Create: `cmd/scd/main.go` — `func main() { os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr)) }`, переменная `var version = "dev"` (ldflags `-X main.version=…`), передаётся в `cli.Execute` через `cli.Version = version` до вызова
- Create: `internal/cli/root.go`, `internal/cli/root_test.go`
- Create: `internal/common/cliutil/cliutil.go` (`var ErrReportFailed = errors.New("report has failed items")` — sentinel «отчёт напечатан, но есть failed»; сервисные пакеты не могут импортировать `internal/cli`)
- Create: `internal/scd/cli/cli.go` (`func Commands() []*cobra.Command` → пока пусто), `internal/sfc/cli/cli.go` (`func New() *cobra.Command` → `Use: "sfc"`, Short `Filter upstream subscriptions and serve them by public URL.`), `internal/gtl/cli/cli.go` (`func New() *cobra.Command` → `Use: "gtl"`, Short `Gen Test Load`)
- Create: `internal/archtest/imports_test.go`
- Create: `.golangci.yml`
- Modify: `.gitignore` — добавить `/bin/`
- Modify: `.github/workflows/docker.yml` — новая джоба `go` (условие как у `verify`), Node-шаги не трогать

**Interfaces:**
- Produces: `package cli` — `var Version = "dev"`; `func NewRoot() *cobra.Command` (Use `scd`, Short `Generate and apply Xray resources from subscriptions.`, `SilenceUsage: true`, `SilenceErrors: true`, `Version: Version`, шаблон версии `{{.Version}}\n`, completion отключён); `func Execute(args []string, stdout, stderr io.Writer) int` — 0 при успехе, 1 при любой ошибке (печатает `err.Error()+"\n"` в stderr). Если ошибка `errors.Is(err, cliutil.ErrReportFailed)` — `Execute` возвращает 1, ничего не печатая.
- Produces: корень монтирует `scdcli.Commands()...`, `sfccli.New()`, `gtlcli.New()`.

- [ ] **Step 1: Failing tests** в `internal/cli/root_test.go`:
  - `TestHelpListsServiceCommands`: `Execute([]string{"--help"}, &out, &errb)` == 0; `out` содержит `sfc` и `gtl`.
  - `TestUnknownCommandFails`: `Execute([]string{"nope"}, …)` == 1; `errb` содержит `unknown command "nope"`.
  - `TestVersionFlag`: `Version="1.2.3"`; `Execute([]string{"--version"})` == 0, `out.String()=="1.2.3\n"`.
  - `TestReportFailedExitCode`: команда-заглушка, возвращающая `cliutil.ErrReportFailed`, даёт код 1 и пустой stderr.
  - `internal/archtest/imports_test.go` `TestServicesDoNotImportEachOther`: обходит `internal/scd`, `internal/sfc`, `internal/gtl` (`go/parser` с `parser.ImportsOnly`), ни один файл не импортирует `github.com/a1ekseev/scd/internal/<другой сервис>`; `internal/common/...` не импортирует ни один сервис.
- [ ] **Step 2: Run** `go test ./...` → FAIL (нет пакетов/функций).
- [ ] **Step 3: Реализация** `go.mod` (`go get github.com/spf13/cobra@latest`), `root.go`, заглушки, `main.go`.
- [ ] **Step 4: `.golangci.yml`** (v2): `version: "2"`; `linters.default: standard`; `linters.enable: [errorlint, gocritic, revive, bodyclose, contextcheck, noctx, modernize, usestdlibvars, perfsprint, misspell, unconvert]`; `formatters.enable: [gofumpt, goimports]`; `formatters.settings.goimports.local-prefixes: [github.com/a1ekseev/scd]`; `run.build-tags: [xraylive, visual]`.
- [ ] **Step 5: CI-джоба `go`** в `.github/workflows/docker.yml`: `actions/checkout@v5` → `actions/setup-go@v7` (`go-version-file: go.mod`, `cache: true`) → `go mod verify` → `go vet ./...` → `golangci/golangci-lint-action@v9` (`version: v2.15.0`) → `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` → `go test -race -count=1 ./...` → `go build -o bin/scd ./cmd/scd` → `./bin/scd --help`.
- [ ] **Step 6: Verify:** `go vet ./... && go test -race ./... && golangci-lint run && go build -o bin/scd ./cmd/scd && ./bin/scd --help && ./bin/scd sfc --help && ./bin/scd gtl --help`. Expected: всё зелёное, help печатается.
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
- [ ] **Step 5: Commit** `feat(common): add jsonout, listenaddr and regexlit packages` (body: D8, D9).

---

### Task 1.3: `configload` и `schema`

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
  - `type Node struct{…}`; `func Root(v any, issues *Issues) *Node`; методы (все nil-safe, ошибки типа/отсутствия пишут Issue с путём `a.b.0.c`): `Object(key string, required bool) *Node`, `Array(key string, required bool, minItems int) []*Node`, `String(key string, o StrOpt) (string, bool)` (`StrOpt{Required, Trim, MinLen, Default *string, EmptyToAbsent bool}`), `StringList(key string, o StrOpt) []string`, `Int(key string, o IntOpt) int` (`IntOpt{Required bool; Default, Min, Max *int}`; YAML-float с целым значением допустим), `Bool(key string, def bool) bool` (только YAML bool; `yes` → issue), `Enum(key string, allowed []string, def string) string`, `URL(key string, required bool) (string, bool)` (`net/url.Parse` + непустые scheme и host), `Strict(known ...string)` (неизвестные ключи → `unknown key "x"`), `Has(key) bool`, `Path() string`, `Addf(key, format string, args ...any)`.
  - Сообщения: `required`, `expected <type>, got <type>`, `must be at least N`, `must be at most N`, `must be one of a|b|c`, `must not be empty`, `invalid URL`, `unknown key "x"`.

- [ ] **Step 1: Failing tests (`configload`):**
  - `TestLoadRawStrictInterpolatesBeforeYAML`: `a: ${X}` с X=`true` → `Root["a"]==true` (bool); `# ${NOPE}` в комментарии → ошибка `missing environment variable "NOPE"`.
  - `TestLoadLenientInterpolatesStringsOnly`: `a: "${X}"`, `n: 5`, X не задан → `a==""`; ключ `${X}` не трогается.
  - `TestLoadNoneKeepsLiteral`: `a: ${X}` → `"${X}"`.
  - `TestLoadDuplicateKeyFails`, `TestLoadEmptyFileIsEmptyMap`, `TestLoadMultiDocFails`, `TestLoadMissingFileError` (`errors.Is(err, fs.ErrNotExist)`).
  - `TestResolveInput`: `-`, `HTTP://x`, `/abs`, `rel/a.txt`, `ftp://x/y` (→ `<dir>/ftp:/x/y`).
- [ ] **Step 2: Failing tests (`schema`):** `TestRequiredMissing` (путь `subscriptions.0.id`), `TestWrongType`, `TestTrimAndMin`, `TestIntBounds`, `TestFloatIntegralAccepted` (`5000.0` → 5000), `TestBoolRejectsYesString`, `TestEnumDefault`, `TestStrictUnknownKey`, `TestIssuesErrFormat` (точный текст двух issues).
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/common/... && golangci-lint run` → PASS.
- [ ] **Step 6: Commit** `feat(common): add config loading and schema validation helpers`.

---

### Task 1.4: `cron` и `cronloop`

**Read:** `07` D7, D23 (Cron); `03` §3.2–§3.5, §10.1; `04` §3.4, §4.8, §11 п.1–4; `06` §3; `testdata/reference/common/cron-next.json`, `cron-rejected.json` (из 0.2).

**Files:**
- Create: `internal/common/cron/cron.go`, `cron_test.go`
- Create: `internal/common/cronloop/cronloop.go`, `cronloop_test.go`

**Interfaces:**
- Produces (`cron`): `type Schedule interface{ Next(time.Time) time.Time }`; `func Parse(expr string) (Schedule, error)` — `robfig/cron/v3` `NewParser(SecondOptional|Minute|Hour|Dom|Month|Dow|Descriptor)`, дополнительно отвергает `""` (`cron expression must not be empty`) и `@every …` (`@every is not supported`); часовой пояс — `time.Local`.
- Produces (`cronloop`):
  - `func Run(ctx context.Context, s cron.Schedule, task func(context.Context) error, h Hooks)`; `type Hooks struct{ OnError func(error); OnOverrun func(scheduledAt time.Time) }`. Алгоритм `03` §3.2: первый тик сразу; `scheduledAt = s.Next(prevScheduled)`; сон через `time.NewTimer` с `select` на `ctx.Done()` (D7: выход сразу, не ждать слот); занятый тик → `OnOverrun`, слот пропускается; при выходе ждёт активную задачу. Задача получает `ctx` (отмена — ответственность задачи).
  - `func RunLogged(ctx context.Context, s cron.Schedule, task func(context.Context) error, log *slog.Logger, ev Events, attrs ...any)`; `type Events struct{ OverrunEvent, OverrunMsg, FailureEvent, FailureMsg string }`. Overrun → `log.Warn(OverrunMsg, attrs..., "event", OverrunEvent, "scheduledAt", <RFC3339 millis UTC>)`; ошибка → `log.Error(FailureMsg, attrs..., "event", FailureEvent, "error", err.Error(), ["causes", []string])`.
  - `func Causes(err error) []string` — сообщения из `interface{ Unwrap() []error }` (непустые), иначе `nil`.

- [ ] **Step 1: Failing tests (`cron`):**
  - `TestParseMatchesReferenceNext`: для каждой записи `cron-next.json` (в `TestMain` установить `time.Local = time.UTC` — эталоны сняты при `TZ=UTC`) три последовательных `Next` равны эталону.
  - `TestParseRejects`: каждая строка `cron-rejected.json` → ошибка.
- [ ] **Step 2: Failing tests (`cronloop`, `testing/synctest`):**
  - `TestRunSkipsOverrunWithoutDrift` (`03` §10.1 п.1): расписание каждые 100 мс (фейковый `Schedule`), первый тик блокируется до первого overrun → старты `[0ms, 200ms]`, overrun один на `100ms`.
  - `TestRunStopsImmediatelyOnCancel` (D7): слот через 1 час, `cancel()` → `Run` возвращается без продвижения часов дальше `0`.
  - `TestRunWaitsForActiveTask`: cancel во время задачи → `Run` возвращается после завершения задачи.
  - `TestCausesFromJoin` (`03` §10.1 п.2): `errors.Join(errors.New("first cause"), errors.New("second cause"))` → `["first cause","second cause"]`.
  - `TestRunLoggedOverrunLog` / `TestRunLoggedFailureLogsCauses` (`03` §10.1 п.3–4, `04` §11 п.3–4): JSON-handler в буфер; overrun → warn с `event`, `subscriptionId`, `scheduledAt`; ошибка → ровно один error с `causes`; loop продолжает работу.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement** (`go get github.com/robfig/cron/v3@v3.0.1`). **Step 5: Run** `go test -race ./internal/common/...` → PASS.
- [ ] **Step 6: Commit** `feat(common): add cron parser and cron loop with immediate shutdown` (body: D7, D23).

---

### Task 1.5: `logging` и `httpserve`

**Read:** `07` D7, D9, D23 (panic → 500); `03` §6.1, §8; `04` §5.1, §7; `05` §5–§6.

**Files:**
- Create: `internal/common/logging/logging.go`, `logging_test.go`
- Create: `internal/common/httpserve/httpserve.go`, `httpserve_test.go`

**Interfaces:**
- Produces (`logging`): `func New(w io.Writer, level, format string) (*slog.Logger, error)`; level ∈ `fatal|error|warn|info|debug|trace|silent` (trace = `slog.Level(-8)`, fatal = `slog.Level(12)`, silent → `slog.DiscardHandler`); format `json` → `slog.JSONHandler`, `pretty` → `slog.TextHandler`; `ReplaceAttr` переименовывает уровни −8/12 в `TRACE`/`FATAL`. Соглашение для всех сервисов: `log.Info("<Human message.>", "event", "<event_name>", …)` — `event` первым атрибутом.
- Produces (`httpserve`):
  - `func Recover(log *slog.Logger, h http.Handler) http.Handler` — panic → 500 `text/plain; charset=utf-8` тело `internal server error\n`, лог `error` с `event` `http_handler_panic`, `panic`, `path`.
  - `func Run(ctx context.Context, ln net.Listener, h http.Handler, shutdownTimeout time.Duration) error` — `http.Server{Handler: h, ReadHeaderTimeout: 60*time.Second}` (без `WriteTimeout`: gtl отдаёт большие тела); при `ctx.Done()` → `Shutdown` с таймаутом; возвращает `nil` при штатной остановке.
  - `func Listen(addr string) (net.Listener, error)` — `net.Listen("tcp", addr)`.

- [ ] **Step 1: Failing tests:** `TestNewLevels` (trace пишет при level `trace`, не пишет при `debug`; `silent` ничего не пишет), `TestNewJSONHasEventAttr`, `TestNewRejectsUnknownLevel`; `TestRecoverReturns500` (handler с panic → 500, лог содержит `http_handler_panic`), `TestRunShutsDownOnCancel` (запрос в процессе завершается, `Run` возвращает nil после cancel), `TestRunReturnsServeError`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/common/...` → PASS.
- [ ] **Step 5: Commit** `feat(common): add slog logger factory and HTTP serve helpers` (body: D7, D9, D23).

---

### Task 1.6: `inputsrc` и `subscription` (загрузка источника, base64, строки)

**Read:** `01` §3, §4, §17.1 P11–P18; `04` §4.1–§4.3; `07` D9, O9 в README плана.

**Files:**
- Create: `internal/common/inputsrc/inputsrc.go`, `inputsrc_test.go`
- Create: `internal/common/subscription/lines.go`, `lines_test.go`

**Interfaces:**
- Produces (`inputsrc`):
  - `type Options struct{ Format string /* auto|plain|base64, "" = auto */; FetchTimeout time.Duration /* 0 → 5s */; Stdin io.Reader; HTTPClient *http.Client }`
  - `type Loaded struct{ Source, Content, Encoding string /* plain|base64 */ }`
  - `func Load(ctx context.Context, input string, o Options) (Loaded, error)`: `-` → stdin, Source `stdin`; `^(?i)https?://` → GET (редиректы по умолчанию Go; таймаут — `context.WithTimeout` на запрос+тело), Source = исходный URL; иначе файл (`filepath.Abs`), Source = абсолютный путь. У всех источников снимается ведущий U+FEFF. Пустой (после `strings.TrimSpace`) до и после декодирования → `ErrEmpty`, текст `input source <src> returned empty content`. HTTP не-2xx → `unable to download input from <url>: <resp.Status>`; таймаут → `input source <url> timed out after <N>ms`.
  - `func DecodeAuto(raw string) (content string, decoded bool)`: trimmed содержит `://` → как есть; иначе удалить все пробельные (`unicode.IsSpace`), пробовать `base64.StdEncoding`, `RawStdEncoding`, `URLEncoding`, `RawURLEncoding`; ошибка или результат без `://` → как есть. Формат `base64`: то же декодирование, ошибка → `input source <src> is not valid base64: <err>` (D9: без лояльности Node).
- Produces (`subscription`): `type Line struct{ Number int; Raw, Trimmed string }`; `func ScanLines(text string) []Line` — split по `\n`, у `Raw` срезается хвостовой `\r`, `Trimmed = strings.TrimSpace(Raw)`, пустые отфильтрованы, `Number` — индекс до фильтрации (1-based).

- [ ] **Step 1: Failing tests (`inputsrc`):**
  - `TestLoadFilePlain`, `TestLoadFileWithBOM` (BOM снят, Content начинается с `vless://`), `TestLoadFileIgnoresFetchTimeout` (P16).
  - `TestLoadHTTPBase64` (P12; `httptest.Server`; Source = URL, Encoding `base64`).
  - `TestLoadHTTPTimeout` (P13; сервер висит, `FetchTimeout: 10*time.Millisecond` → ошибка содержит `timed out after 10ms`).
  - `TestLoadHTTPNon2xx` (`404 Not Found` в тексте), `TestLoadEmptyRemote` (P14), `TestLoadEmptyFile` (P15, `"   \n"`), `TestLoadStdinBase64` (P17), `TestLoadEmptyStdin` (P18).
  - `TestDecodeAuto` (таблица, P11 + 01 §3.2): std, url-safe, без padding, с переносами каждые 20 символов, с пробелами по краям → decoded; `hello world`, `vless://…` plain, base64 текста без `://`, `!` внутри → не decoded.
  - `TestLoadBase64FormatInvalid` → ошибка `is not valid base64`.
- [ ] **Step 2: Failing tests (`subscription`):** `TestScanLinesNumbersAndCRLF` (`"a\r\n\r\n b \n"` → `[{1,"a","a"},{3," b ","b"}]`), `TestScanLinesTrailingNewline` (P20: одна строка + `\n` → 1 Line), `TestScanLinesLoneCRNotSeparator`.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/common/...` → PASS.
- [ ] **Step 6: Commit** `feat(common): add subscription input loading and line scanning` (body: D9).

---

### Task 1.7: `vless` — разбор строки подписки через `net/url`

**Read:** `01` §5 (смысл проверок и reason-коды), §16 п.4–11, §17.4; `04` §4.4; `09` §1.1 (строки `userinfo`, `host:port`), §7 п.1; `07` D9, D14, D18; README плана O7, O11.

**Files:**
- Create: `internal/common/vless/parse.go`, `parse_test.go`, `testdata/` не нужен (использовать `testdata/reference/scd/corpus/ts-parse.json` через относительный путь `../../../testdata/reference/...`)

**Interfaces:**
- Consumes: `subscription.Line` (1.6).
- Produces:
  - `type Param struct{ Key, Value string }`
  - `type Link struct{ Line int; Raw, Label, ID, Address string; Port int; Params []Param }` + `func (l Link) Get(key string) (string, bool)`. Ключи lowercased; порядок — первого появления; при дубле значение последнего (позиция первого). Address — `u.Hostname()` (IPv6 без скобок).
  - `type Skip struct{ Line int; Raw string; Label *string; Code, Reason string; Details []string }` (JSON-теги `line, raw, label,omitempty, reasonCode, reason, details,omitempty`)
  - Коды: `const (CodeUnsupportedScheme="unsupported_scheme"; CodeInvalidURI="invalid_uri"; CodeMissingRequiredField="missing_required_field"; CodeUnsupportedParam="unsupported_param"; CodeUnsupportedValue="unsupported_value"; CodeUnsupportedCombo="unsupported_combo"; CodeDuplicateTag="duplicate_tag")`
  - `var AllowedParams = []string{"type","path","security","alpn","encryption","fp","sni","flow","host","allowinsecure","headertype","pbk","sid","spx","pcs","vcn","mode","extra"}` (D14; `ech`, `fm`, `pqv`, `serviceName`… не входят → `unsupported_param`).
  - `func Parse(l subscription.Line) (Link, *Skip)`.
- Порядок проверок (reason — дословно, это пользовательский текст отчётов):
  1. нет `://` → `unsupported_scheme` «The line does not contain a supported URI scheme.»
  2. `url.Parse(trimmed)` ошибка → `invalid_uri` «The line is not a valid URI.»
  3. scheme (lowercase) ≠ `vless` → `unsupported_scheme` `Unsupported scheme "<scheme>:".`
  4. label = `url.PathUnescape(u.EscapedFragment())`; ошибка → `invalid_uri` «The line label is not valid percent-encoding.» (O11)
  5. id = `u.User.Username()`; пусто → `missing_required_field` «The VLESS URI does not contain a user id.» (D18; label задан)
  6. host пуст, порт пуст/не число/вне 1..65535 → `missing_required_field` «The VLESS URI does not contain a valid address and port.»
  7. query: свой разбор `u.RawQuery` по `&` (пустые сегменты пропускаются; ключ/значение через `url.QueryUnescape`, при ошибке — сырое значение); ключи вне `AllowedParams` → `unsupported_param` `Unsupported VLESS parameter(s): a, b.` + details (в порядке появления).

- [ ] **Step 1: Failing tests** `parse_test.go` (таблица):
  - ok: `VLESS://<uuid>@Example.COM:0443?Type=TCP&Security=TLS#x` → Address `Example.COM`, Port 443, Params `[{type,TCP},{security,TLS}]`, Label `x`.
  - ok: `#🇩🇪 Берлин#2` → Label `🇩🇪 Берлин#2`; `#Hello%20World` → `Hello World`; без `#` → `""`.
  - ok: `sni=a&sni=b` → `sni=b`; `path=/a+b%2Fc` → `/a b/c`; `path=/a;x` → `/a;x`; `[2001:db8::1]:443` → Address `2001:db8::1`.
  - ok: id `my-user` (не UUID, D18) и uppercase UUID — без skip.
  - skip: `trojan://x` (scheme), `vless:uuid@h:443` (scheme), `vless://u@ho st:443` (invalid_uri), `vless://@h:443` (user id), `vless://u@h` / `vless://u@h:0` / `vless://u@h:65536` (address and port), `#100%` (O11, invalid_uri), `foo=bar&ech=x` → details `["foo","ech"]`.
  - `TestParseCorpusAgainstTS`: для каждой строки `ts-parse.json` с `ts.ok==true` и без новых параметров (не содержит `spx|pcs|vcn|mode|extra|xhttp|mlkem` и не id-нестандартный) — Go тоже ok; для `ts.reasonCode=="unsupported_scheme"` — тот же код. Расхождения допускаются только по списку `allowedDiffs` в тесте с комментарием-причиной (D9/D14/D18/O11).
- [ ] **Step 2: Run** `go test ./internal/common/vless/` → FAIL.
- [ ] **Step 3: Implement** `parse.go`.
- [ ] **Step 4: Run** `go test -race ./internal/common/... && golangci-lint run` → PASS.
- [ ] **Step 5: Commit** `feat(common): add VLESS share-link parser on net/url` (body: D9, D14, D18).
