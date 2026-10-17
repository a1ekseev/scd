# Этап 2. gtl → `scd gtl`

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development; TDD обязателен. Прочитать «Глобальные ограничения» в `docs/go-migration/plan/README.md`.

**Goal:** перенести gtl (детерминированный HTTP-генератор нагрузки) в `internal/gtl` и подкоманды `scd gtl serve|validate-config|print-config`, с байт-идентичным payload (golden SHA-256).

**Architecture:** `internal/gtl/config` (загрузка через `configload.EnvNone` + `schema`), `internal/gtl/payload` (xorshift32, стриминг по 64 KiB), `internal/gtl/server` (`http.Handler`), `internal/gtl/cli` (cobra). Не импортирует scd/sfc.

**Tech Stack:** stdlib, `internal/common/{configload,schema,listenaddr,logging,httpserve,jsonout}`.

**Spec:** `05-gtl.md` (весь), `07` D7, D9, D10, D27 (O10); README плана O10.

## Контекст этапа

- Отличия от TS, принятые по D9/O10: тексты ошибок конфига Go-формата (`schema.Issues.Err`); логи в stderr через `internal/common/logging` (`event` = `gtl_server_started|gtl_server_stopping|gtl_server_stopped`, msg — то же имя); нет `--version` у `scd gtl`; при ошибке bind печатается `err.Error()` (не stack).
- Сохраняются: маршруты, коды, тела, заголовки `x-gtl-*`, `/healthz` → `ok` без `\n` с `content-length: 2`, 405 для не-GET (включая HEAD), payload и его 64 KiB-пересев.

---

### Task 2.1: Конфиг gtl и команды `validate-config` / `print-config`

**Read:** `05` §2, §3, §10 тесты 1–10; `docker/gtl/config.example.yml`; `testdata/reference/gtl/print-config-docker-example.json`.

**Files:**
- Create: `internal/gtl/config/config.go`, `config_test.go`
- Modify: `internal/gtl/cli/cli.go` (+ `cli_test.go`)

**Interfaces:**
- Consumes: `configload.Load(path, configload.EnvNone, nil)`, `schema.*`, `listenaddr.Parse`.
- Produces: `type Config struct{ Server struct{ Listen string `json:"listen"` } `json:"server"`; Logging struct{ Level string `json:"level"`; Format string `json:"format"` } `json:"logging"`; Load struct{ Path string `json:"path"`; MaxSizeKb int64 `json:"maxSizeKb"` } `json:"load"` }` (порядок JSON: server, logging, load); `func Load(path string) (Config, error)`.
- Правила: все объекты strict; `server.listen` обязателен, `listenaddr.Parse`; `logging` по умолчанию `{info, json}`, `format` ∈ `json|pretty` (D27 O10), логгер — `logging.New(os.Stderr, …)`; `load.path` обязателен, проверки накапливаются: `must start with /`, `must not equal /`, `must not end with /`, `must not contain query or hash`; `load.maxSizeKb` default 10240, `1..8796093022207`.
- CLI: `validate-config --config <path>` → stdout `Config is valid\n`; `print-config --config <path>` → `jsonout.Write(cfg)`; `--config` обязателен (`MarkFlagRequired`).

- [ ] **Step 1: Failing tests** (`config_test.go`, переносы `05` §10 п.1–10):
  - `TestLoadValid` (listen `127.0.0.1:8080`, maxSizeKb 64, logging debug/pretty сохраняются)
  - `TestLoadDefaults` (10240, info, json)
  - `TestLoadBadListen` (`localhost` → issue `server.listen`, и одновременно issue `load` при отсутствии секции — ошибки накапливаются)
  - `TestLoadMaxSizeZero` (`load.maxSizeKb`), `TestLoadMissingPath` (`load.path`)
  - `TestLoadPathRules` (таблица `/`, `load`, `/load/`, `/load?x=1`, `/load#x` → путь `load.path`; `lo?ad/` → 3 сообщения)
  - `TestLoadUnknownKey` (`server.foo` → `unknown key "foo"`)
  - `TestLoadNoEnvInterpolation` (`path: /${X}` остаётся буквально)
  - `cli_test.go`: `TestPrintConfigMatchesReference` — вывод для `docker/gtl/config.example.yml` семантически равен `testdata/reference/gtl/print-config-docker-example.json`; `TestValidateConfigOutput` → `Config is valid\n`; `TestMissingConfigFlag` → код 1.
- [ ] **Step 2: Run** `go test ./internal/gtl/...` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify** `go test -race ./internal/gtl/... && go build -o bin/scd ./cmd/scd && ./bin/scd gtl validate-config --config docker/gtl/config.example.yml && ./bin/scd gtl print-config --config docker/gtl/config.example.yml`.
- [ ] **Step 5: Commit** `feat(gtl): port gtl config and validate/print commands` (body: D9, D10).

---

### Task 2.2: Payload-генератор и HTTP-обработчик

**Read:** `05` §4 (весь), §10 тесты 11–20, §11; `testdata/reference/gtl/payload-vectors.json`, `testdata/reference/gtl/routes.json`.

**Files:**
- Create: `internal/gtl/payload/payload.go`, `payload_test.go`
- Create: `internal/gtl/server/server.go`, `server_test.go`

**Interfaces:**
- Produces (`payload`): `const ChunkSize = 65536`; `func Fill(b []byte, offset uint64)` — алгоритм `05` §4.5 дословно (seed `uint32(0x9e3779b9) ^ uint32(offset)`, шаги 13/17/5, байт после шага); `func Write(w io.Writer, total uint64) error` — один буфер `min(ChunkSize, max(total,1))`, `Fill(buf[:n], off)` для каждого чанка, при ошибке `Write` — вернуть её (без зависаний).
- Produces (`server`): `func New(cfg config.Config) http.Handler`. Путь: из `r.URL.EscapedPath()` с удалением dot-сегментов (`path.Clean`, сохранив отсутствие декодирования `%xx`); порядок: метод ≠ GET → 405 `method not allowed\n`; `/healthz` → 200 `ok`; нет префикса `load.path+"/"` → 404 `not found\n`; хвост не `^[0-9]+$` → 400 `invalid load route, expected <load.path>/:sizeKb\n`; число > maxSizeKb (длинные — по длине после срезания ведущих нулей > 16) → 400 `sizeKb must be an integer from 0 to <max>\n`; иначе 200 с заголовками `content-type: application/octet-stream`, `content-length`, `x-gtl-status-code: 200`, `x-gtl-requested-size-kb: <нормализованное число>`, `x-gtl-requested-size-bytes`, `x-gtl-actual-size-bytes`. Текстовые ответы — `text/plain; charset=utf-8` + `content-length`.

- [ ] **Step 1: Failing tests (`payload`):**
  - `TestFillVectors`: `fill8` из `payload-vectors.json` (offset 0 → `193e3ab51f37d0bf`, 65536 → `190f1a4032fb31ec`, 2^32 → как 0, 2^32+65536 → как 65536).
  - `TestWriteSHA256Vectors`: для каждого `sizeBytes` из эталона sha256 и first16hex совпадают (включая 10485760).
  - `TestWriteDiffersByOffset` (`05` §10 п.20), `TestWriteNotASCIIa` (п.19: 98311 байт, первые 8 ≠ `aaaaaaaa`, >128 различных среди первых 256).
  - `TestWriteStopsOnWriterError` (writer, падающий на втором чанке → ошибка, без паники).
- [ ] **Step 2: Failing tests (`server`, `httptest`):** каждая запись `routes.json` → тот же статус, тело (для 200 — длина и sha256), заголовки `x-gtl-*`/`content-type`/`content-length`; плюс `05` §10 п.11–18: `/download/32` при `load.path: /download` → 200; `/load/32` при `/download` → 404; `/a/../load/1` → 200; `/load/%33` → 400; HEAD `/healthz` → 405.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/gtl/...` → PASS.
- [ ] **Step 6: Commit** `feat(gtl): port deterministic payload generator and HTTP handler`.

---

### Task 2.3: `scd gtl serve` — жизненный цикл и смоук

**Read:** `05` §4.1, §5, §6; `07` D7; `internal/common/httpserve` (1.5); README плана O10.

**Files:**
- Create: `internal/gtl/cli/serve.go`, `serve_test.go`
- Modify: `internal/gtl/cli/cli.go` (подключить `serve`)

**Interfaces:**
- Consumes: `config.Load`, `server.New`, `logging.New(os.Stderr, …)`, `httpserve.Listen/Run/Recover`.
- Produces: `func RunServe(ctx context.Context, cfgPath string, stderr io.Writer) error` (используется командой и тестом). Поведение: загрузить конфиг → логгер → `Listen` (ошибка bind → вернуть ошибку, лог не писать) → лог `gtl_server_started` (`listen`) → `httpserve.Run` до `ctx.Done()` → лог `gtl_server_stopping` (`signal` — имя сигнала, если известно, иначе `context canceled`) → после Shutdown лог `gtl_server_stopped`. Команда `serve` создаёт `ctx` через `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`.

- [ ] **Step 1: Failing tests:**
  - `TestServeServesAndStops`: временный конфиг `listen: 127.0.0.1:0`… (порт 0 запрещён `listenaddr` → в тесте взять свободный порт через `net.Listen` + Close); `RunServe` в горутине; GET `/healthz` → `ok`; `cancel()` → `RunServe` возвращает nil; stderr содержит три события по порядку.
  - `TestServeBindError`: порт уже занят → `RunServe` возвращает ошибку, содержащую `address already in use`.
  - `TestServeFinishesActiveDownload`: начать `/load/10240`, отменить ctx после первых байт → клиент получает все 10485760 байт.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Verify:**
  ```bash
  go test -race ./internal/gtl/... && go build -o bin/scd ./cmd/scd
  sed 's/0.0.0.0:8080/127.0.0.1:18790/' docker/gtl/config.example.yml > /tmp/gtl.yml
  ./bin/scd gtl serve --config /tmp/gtl.yml & p=$!; sleep 1
  curl -s http://127.0.0.1:18790/load/1 | sha256sum
  kill -TERM $p; wait $p; echo exit=$?
  ```
  Expected: sha256 `8ab783947bae918d01b74448625dfd0fef119e16e97d6fc9fdf15e310c87cb01` (ревью пересчитало этот вектор Go-реализацией xorshift32 — совпадает, как и `fill8` для offset 0/65536/2^32/2^32+65536), затем `exit=0`.
- [ ] **Step 5: Commit** `feat(gtl): add scd gtl serve with graceful shutdown` (body: D7, D10, O10).
