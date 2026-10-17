# План реализации: миграция scd + sfc + gtl на Go (один бинарь `scd`)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Каждая задача пишется по superpowers:test-driven-development: сначала падающий тест, потом минимальный код.

**Goal:** заменить три TypeScript/Node-сервиса (`scd/`, `sfc/`, `gtl/`) одним Go-модулем `github.com/a1ekseev/scd` с одним бинарём `scd` и одним Docker-образом `ghcr.io/a1ekseev/scd`, сохранив функциональность, JSON API status-сервера, вид дашборда и поведение по отношению к Xray; затем удалить весь TypeScript.

**Architecture:** один модуль в корне репозитория; `cmd/scd` собирает дерево cobra-команд (`scd sync|daemon|generate|validate-config|print-config`, `scd sfc …`, `scd gtl …`). Сервисная логика — в `internal/{scd,sfc,gtl}` (друг друга не импортируют), действительно общий код — в `internal/common/*`. Подписочная ссылка → JSON outbound в форме Xray v26.10.10 → `infra/conf` `Build()` → protobuf → gRPC API Xray (D19).

**Tech Stack:** Go 1.27 (`go 1.27.0`, `toolchain go1.27.2`), cobra, `go.yaml.in/yaml/v3`, `github.com/robfig/cron/v3` (только парсер), `log/slog`, stdlib `regexp`/`net/url`/`encoding/base64`/`net/http`/`html/template`+`embed`, `github.com/xtls/xray-core` (pseudo-version 26.10.10) + `google.golang.org/grpc` (+ `grpc/test/bufconn`), `testing/synctest`, `github.com/chromedp/chromedp` (только тесты под build tag `visual`), golangci-lint v2.15.0, govulncheck.

**Spec:** `docs/go-migration/07-decisions.md` (высший приоритет), затем `00-overview.md`, `09-subscriptions-xray-26.10.10.md`, `10-balancer-selection.md`, `08-xray-26.10.10.md`, `02-scd-xray-api.md`, `01-scd-domain.md`, `03-scd-runtime.md`, `04-sfc.md`, `05-gtl.md`, `06-go-stack.md`. При противоречии действует документ выше по списку. Читать 01/03/04/05/06 с поправкой на D9: «повторить Node 1:1» означает «поведение зафиксировать тестом, реализовать идиоматично на Go».

---

## Файлы плана

| Этап | Файл | Задач | Зависит от |
|---|---|---|---|
| 0. Эталоны с TS (до удаления TS) | [`stage-0-references.md`](stage-0-references.md) | 4 | — |
| 1. Каркас и `internal/common` | [`stage-1-skeleton.md`](stage-1-skeleton.md) | 7 | 0 (только задача 1.4 читает cron-эталоны) |
| 2. gtl | [`stage-2-gtl.md`](stage-2-gtl.md) | 3 | 1 |
| 3. sfc | [`stage-3-sfc.md`](stage-3-sfc.md) | 4 | 1 |
| 4. scd: домен (конфиг, разбор, нормализация, билдер, манифест, топология) | [`stage-4-scd-domain.md`](stage-4-scd-domain.md) | 7 | 1 |
| 5. scd: Xray API (клиент, fake, apply, live-тесты) | [`stage-5-scd-xray-api.md`](stage-5-scd-xray-api.md) | 6 | 4 |
| 6. scd: runtime (sync, мониторинг, status server, дашборд, balancer selection, daemon) | [`stage-6-scd-runtime.md`](stage-6-scd-runtime.md) | 7 | 5 |
| 7. Переключение: Docker, compose, CI, README/AGENTS.md, удаление TS | [`stage-7-cutover.md`](stage-7-cutover.md) | 3 | 2, 3, 6 |

Итого **41 задача**.

### Граф зависимостей

```
Stage 0 (0.1 → 0.2, 0.3, 0.4)
   │
Stage 1: 1.1 → 1.2 → 1.3 → 1.4 → 1.5 → 1.6 → 1.7
   │
   ├──► Stage 2 gtl: 2.1 → 2.2 → 2.3 ─────────────────────────────┐
   ├──► Stage 3 sfc: 3.1 → 3.2 → 3.3 → 3.4 ───────────────────────┤
   └──► Stage 4: 4.1 → 4.2 → 4.3 → 4.4 → 4.5 → 4.6 → 4.7          │
            │                                                     │
         Stage 5: 5.1 → 5.2 → 5.3 → 5.4 → 5.5 → 5.6              │
            │                                                     │
         Stage 6: 6.1 → 6.2 → 6.3 → 6.4 → 6.5 → 6.6 → 6.7         │
            │                                                     │
         Stage 7: 7.1 → 7.2 → 7.3 ◄───────────────────────────────┘
```

**Критический путь:** 0.1 → 0.4 (эталоны дашборда нужны 6.5) … 1.1 → 1.7 → 4.1 → 4.7 → 5.1 → 5.6 → 6.1 → 6.7 → 7.1 → 7.3 (32 задачи последовательно).

**Независимые ветки.** Этапы 2 (gtl) и 3 (sfc) не зависят от этапов 4–6 и друг от друга. SDD запрещает параллельных implementer'ов в одном worktree, поэтому по умолчанию они идут последовательно (рекомендуемый порядок: 2 → 3 → 4 …, чтобы каркас обкатался на простом gtl). Параллельно их можно вести только в отдельных git worktree (`superpowers:using-git-worktrees`) с последующим rebase на `move-to-go`. Задачи 0.2, 0.3, 0.4 независимы друг от друга (после 0.1).

---

## Глобальные ограничения (Global Constraints)

Каждая задача неявно включает этот раздел. Значения — дословно из 07.

- Ветка: `move-to-go`. Коммиты только в неё, без push (push делает координатор).
- `go.mod`: `module github.com/a1ekseev/scd`, `go 1.27.0`, `toolchain go1.27.2` (D1, D10). Все зависимости — последних версий на момент добавления (`go get <mod>@latest`), кроме xray-core.
- xray-core: `github.com/xtls/xray-core v1.260327.1-0.20261010092107-701af60772cd` с комментарием `// = Xray v26.10.10 (701af60), pre-release; Go tag v1.261010.0 does not exist` (D4). Никогда не `@latest` (откатит на v1.260327.0). Образ в compose: `ghcr.io/xtls/xray-core:26.10.10`. Обновляются только вместе.
- Один бинарь `scd` (`cmd/scd`), один образ `ghcr.io/a1ekseev/scd`, без `blanc`, без multicall по argv[0] (D10).
- CLI: `scd sync|daemon|generate|validate-config|print-config` на верхнем уровне; `scd sfc serve|refresh|validate-config|print-config`; `scd gtl serve|validate-config|print-config` (D10, 04 §2, 05 §2).
- Пакеты `internal/scd`, `internal/sfc`, `internal/gtl` не импортируют друг друга; общий код — только `internal/common/...` (D10). Проверяется тестом `internal/archtest` (задача 1.1).
- Поведение — по-Go (D9): Go-тексты ошибок (кроме текстов Xray API-слоя, D2), stdlib base64, `net/url`, `slog`, `regexp` (RE2, D8), `slices.Sort`/`strings.Compare` (D23). Имена лог-событий (`event`) и поля сохраняются (03 §8.1, 04 §7.1, 05 §6).
- Xray API (D2): тот же набор мутирующих RPC + read-only `GetBalancerInfo`/`GetOutboundStatus` (D22); порядок apply outbound → inbound → routing; fixed-ресурсы сохраняются; rollback только outbound; хэши сохраняются только при успехе всех kinds; repair/rejoin; per-target lock; тексты ошибок API-слоя из 02 §2.7.
- REALITY: `SpiderX`/`SpiderY` (10 элементов) заполнены всегда (D13, 09 §1.4).
- JSON API status-сервера (`/api/status`, `/api/instance-status`, `/api/instances`, `/api/runtime-state`) — контракт: имена и схема полей сохраняются; новые поля только аддитивно, `version` = 1 (D3, D22, D23).
- Дашборд визуально идентичен TS-версии (D3), проверка — visual regression (задача 6.5).
- Graceful immediate shutdown через `signal.NotifyContext` (D7).
- Сборка бинаря **всегда** `go build -o bin/scd ./cmd/scd` (пока в корне есть каталог `scd/`, `go build ./cmd/scd` без `-o` падает: `go: build output "scd" already exists and is a directory` — проверено). `/bin/` добавляется в `.gitignore` в задаче 1.1. Каталоги `scd/`, `sfc/`, `gtl/` (включая `node_modules`) Go-инструментам не мешают: `go list ./...`, `go vet ./...`, `go test ./...`, `golangci-lint run` их игнорируют (нет `.go`-файлов) — проверено.
- Локальный Go 1.24.7: toolchain 1.27.2 скачивается автоматически (`GOTOOLCHAIN=auto`, через прокси). **Любой `go run|go install <tool>@<ver>` локально запускать с `GOTOOLCHAIN=go1.27.2`**: иначе `auto` берёт toolchain по `go`-строке самого инструмента (govulncheck → go1.26) и анализ падает на `package requires newer Go version go1.27 (application built with go1.26)` (проверено). golangci-lint: `GOTOOLCHAIN=go1.27.2 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0` (или релизный бинарь v2.15.0 — он собран go1.27.0 и работает с этим модулем; проверено). govulncheck локально недоступен (прокси песочницы отдаёт 403 на `vuln.go.dev`) — только в CI.
- Docker **доступен** (`docker info`, buildx v0.37, compose v5.6; Docker Hub и `gcr.io/distroless/*` тянутся). **Блокируется** скачивание слоёв `ghcr.io` (манифесты читаются, блобы — нет), поэтому `ghcr.io/xtls/xray-core:26.10.10` локально подменяется образом из релизного архива (команды — «Локальный образ Xray» ниже). Docker-шаги этапа 7 (сборка образа, compose-smoke) можно и нужно прогонять локально; CI их повторяет.
- `**/config.yml` игнорируется git; трекаются только `docker/{scd,sfc,gtl}/config.example.yml`, `docker/fixtures/*`, `docker/README.md`. Локальные `docker/*/config.yml` скрипты плана **не перезаписывают** (там будут реальные конфиги владельца, D12).

**Локальный образ Xray 26.10.10** (вместо недоступного ghcr; релизный ассет проверен: `Xray 26.10.10 … 701af60 (go1.27.2 linux/amd64)`, sha256 совпадает с `.dgst`):
```bash
d=$(mktemp -d) && cd "$d"
curl -fsSLO https://github.com/XTLS/Xray-core/releases/download/v26.10.10/Xray-linux-64.zip
curl -fsSLO https://github.com/XTLS/Xray-core/releases/download/v26.10.10/Xray-linux-64.zip.dgst
grep -q "$(sha256sum Xray-linux-64.zip | cut -d' ' -f1)" Xray-linux-64.zip.dgst && unzip -q Xray-linux-64.zip -d out
printf 'FROM alpine:3.22\nCOPY out/xray /usr/local/bin/xray\nCOPY out/geoip.dat out/geosite.dat /usr/local/share/xray/\nENV XRAY_LOCATION_ASSET=/usr/local/share/xray\nENTRYPOINT ["/usr/local/bin/xray"]\n' > Dockerfile
docker build -q -t ghcr.io/xtls/xray-core:26.10.10 .   # локальный тег; compose не будет его тянуть (pull_policy missing)
```
Тот же `out/xray` годится как `SCD_XRAY_BIN` для live-тестов (5.6).

## Соглашения

**Раскладка модуля (итоговая):**
```
go.mod, go.sum, .golangci.yml, Dockerfile, .dockerignore, docker-compose.yml, README.md, AGENTS.md
cmd/scd/main.go                  точка входа: os.Exit(internal/cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
internal/cli/                    корневая cobra-команда, версия, монтирование scd/sfc/gtl
internal/archtest/               тест запрета импортов между сервисами
internal/common/
  jsonout/ listenaddr/ regexlit/ configload/ schema/ cron/ cronloop/ logging/
  inputsrc/ subscription/ vless/ httpserve/
internal/gtl/      config/ payload/ server/ cli/
internal/sfc/      config/ refresh/ cache/ server/ cli/
internal/scd/
  config/ country/ normalize/ outbound/ manifest/ topology/
  xrayapi/ (+ xrayapi/xrayfake/) resources/ apply/
  state/ syncer/ socks/ monitor/ balancer/ status/ daemon/ cli/
testdata/reference/             эталоны этапа 0 (Go-тесты читают их по относительному пути)
tools/golden/                   Node-скрипты этапа 0 (удаляются в задаче 7.3)
```

**Стиль тестов:** stdlib `testing`, table-driven, `t.Run` с именами кейсов; `httptest`, `bufconn`, `testing/synctest` для времени; без моков там, где можно поднять настоящий объект (in-process gRPC, `httptest.Server`, `net.Listen("tcp","127.0.0.1:0")`). Сравнение protobuf — `proto.Equal`/поля через `GetInstance()`, не байты. Сравнение JSON с эталонами — семантическое (unmarshal в `any` и `reflect.DeepEqual`), кроме отмеченных мест. Тестовые хелперы — в `_test.go` или `internal/.../testutil` (не в production-типах). Live-тесты: build tag `xraylive` + env `SCD_XRAY_BIN`; visual: build tag `visual` + env `SCD_CHROME`; без env тест вызывает `t.Skip`.

**Lint:** `.golangci.yml` (v2, задача 1.1): `linters.default: standard` + `errorlint, gocritic, revive, bodyclose, contextcheck, noctx, modernize, usestdlibvars, perfsprint, misspell, unconvert`; `linters.exclusions.presets: [std-error-handling]`; `formatters: gofumpt, goimports`. Без baseline, без `//nolint` без комментария-причины. Конфиг проверен `golangci-lint config verify` на v2.15.0 (все линтеры существуют). Что это значит для кода (проверено на прототипе):
- revive по умолчанию включает `package-comments` и `exported`: **каждый пакет** начинается с `// Package <name> …`, **каждый экспортируемый** тип/функция/метод/переменная имеет doc-комментарий, иначе `golangci-lint run` красный;
- `noctx`: никаких `http.Get`/`http.Post`/`net.Dial` без контекста — только `http.NewRequestWithContext` + `Client.Do`, `(&net.Dialer{}).DialContext`;
- `errorlint`: сравнение ошибок только `errors.Is/As`;
- `std-error-handling` снимает шум errcheck на `defer resp.Body.Close()` и подобных;
- перед `golangci-lint run` выполнять `golangci-lint fmt` (gofumpt + goimports с `local-prefixes`).

**Проверки после каждой задачи (минимум):**
```bash
go vet ./...
go test -race ./...
golangci-lint fmt && golangci-lint run
go build -o bin/scd ./cmd/scd
```
плюс CLI-смоуки, указанные в задаче.

**Формат коммита:** Conventional Commits, scope = область (`common`, `gtl`, `sfc`, `scd`, `ci`, `docker`, `docs`, `refs`). Каждое сообщение заканчивается ровно так:
```
<type>(<scope>): <summary>

<body: что и почему, ссылки на D-решения>

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_017d6TaUbiiVgm9WSVUN3Lpx
```
Пример команды:
```bash
git commit -F - <<'EOF'
feat(common): add cron parser wrapper

Single parser for config validation and scheduling (D23).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_017d6TaUbiiVgm9WSVUN3Lpx
EOF
```

## Definition of Done по этапам

| Этап | Готово, когда |
|---|---|
| 0 | `testdata/reference/` содержит все артефакты из stage-0 (индекс `testdata/reference/README.md`), TS-тесты всех трёх сервисов зелёные на момент снятия, скрипты в `tools/golden/` воспроизводят эталоны одной командой |
| 1 | `bin/scd --help`, `bin/scd sfc --help`, `bin/scd gtl --help` работают; все `internal/common/*` покрыты тестами; CI-джоба `go` (параллельно Node-джобам) зелёная: vet, golangci-lint, govulncheck, `go test -race` |
| 2 | `bin/scd gtl validate-config --config docker/gtl/config.example.yml` ок; golden SHA-256 из 05 §4.5 совпадают; тесты 05 §10 перенесены |
| 3 | `bin/scd sfc validate-config/print-config/refresh --config docker/sfc/config.example.yml` ок; выход refresh совпадает с эталоном 0.3; тесты 04 §11 перенесены |
| 4 | `bin/scd validate-config` и `bin/scd generate` работают на `docker/` фикстурах; тесты 01 §17 и 09 §7.11 перенесены; каждый поддерживаемый параметр ссылки доходит до proto (unit-golden) |
| 5 | apply против fake Xray покрывает 02 §2, rollback/fixed/порядок RPC; live-тест `-tags xraylive` проходит против бинаря Xray 26.10.10 (локально — релизный бинарь, см. «Локальный образ Xray»; в CI — шаг, добавленный в 5.6) |
| 6 | `bin/scd sync`/`daemon` работают против fake и live Xray; JSON API семантически равен эталонам 0.4; visual regression дашборда проходит (`-tags visual`); тесты 03 §10 и 10 §6.9 перенесены |
| 7 | В репозитории нет TS/Node (`git ls-files | grep -vE '^internal/scd/status/web/' | grep -E '\.(ts|mjs|js)$|package(-lock)?\.json|tsconfig'` пусто; браузерный `dashboard.js` дашборда — не Node-код и исключён явно), CI только Go, один образ (amd64+arm64), compose на одном образе, compose-smoke зелёный локально и в CI, AGENTS.md/README.md описывают Go-раскладку |

## Как исполнять (subagent-driven-development)

1. Контроллер запускает `superpowers:subagent-driven-development` на **одном файле этапа** за раз (план-файл = `docs/go-migration/plan/stage-N-*.md`). Ledger: `<repo>/.superpowers/sdd/<plan>/progress.md` (git-ignored).
2. В каждом stage-файле задачи пронумерованы `Task N.M`; бриф для implementer'а — текст одной задачи + раздел «Глобальные ограничения» этого README (контроллер прикладывает его к брифу) + раздел «Контекст этапа» stage-файла.
3. Implementer читает только перечисленные в задаче секции документов (указаны `файл §раздел`), не весь корпус.
4. Модели: этап 0, 4.3–4.5, 5.1, 5.3, 6.1, 6.3, 6.6 — standard/capable (судейские задачи); 1.2, 2.1, 3.3 — cheap допустим.
5. Ревью после каждой задачи: spec (этот план + 07) и quality. Финальное ревью ветки — после этапа 7.
6. Если ревьюер находит противоречие плана с 07 — прав 07; контроллер фиксирует `Ruling:` в ledger.
7. Открытые вопросы (раздел ниже) — контроллер принимает предложенный по умолчанию вариант и ledger'ит `Ruling:`, если владелец не ответил до начала соответствующей задачи.

## Открытые вопросы / допущения плана (не решены в 07)

| # | Вопрос | Где встаёт | Вариант по умолчанию в плане |
|---|---|---|---|
| O1 | `tcp+reality` **без** `flow` (D16 «пустой flow — норма», Xray это принимает; TS — `unsupported_combo`; в D14 такого профиля нет) | 4.3 | Оставить `unsupported_combo` (scope D14 не расширять) |
| O2 | ML-KEM (`encryption=mlkem768x25519plus.*`) при `security=none` (типичный случай у 3x-ui) — входит ли в D14? | 4.4 | Да: профили `tcp-mlkem`, `ws-mlkem`, `xhttp-mlkem` только при mlkem-шифровании; `security=none` без mlkem — `unsupported_combo`. Плюс mlkem поверх tls/reality-профилей |
| O3 | Где допустим `xtls-rprx-vision` (D16 «по коду Xray»): код Xray принимает vision на ws (conf) и падает в рантайме | 4.3 | Vision сохраняется, если `network=tcp && security∈{tls,reality}` **или** encryption = mlkem; иначе сброс + warning `flow_cleared` |
| O4 | id, который `uuid.ParseString` не принимает (31–36 символов не-UUID, >36) при D18 «любая непустая строка» | 4.3 | skip `unsupported_value` «The VLESS user id is not accepted by Xray.» |
| O5 | `RemoveRule("")` для статического правила без `ruleTag` (02 §1.5 «решение за владельцем») | 5.4 | D2 «строго»: поведение TS сохраняется (вызов, ошибка, routing failed); документировать в README |
| O6 | Имя ключа D15 | 4.1 | `subscriptions[].allowInsecure: deny|allow`, default `deny` |
| O7 | sfc и расширенный белый список параметров (D14 сформулирован для scd) | 1.7, 3.2 | Общий белый список в `internal/common/vless` (sfc выдаёт те строки, которые принял бы разбор scd) |
| O8 | Маршрутизация status server: TS сравнивает `request.url` целиком (`/api/status?x=1` → HTML) | 6.4 | По-Go: маршрут по пути, query игнорируется (`/api/status?x=1` → JSON); тест фиксирует |
| O9 | `User-Agent` при загрузке подписок: D9 запрещает эмуляцию `node`, но провайдеры ветвятся по UA | 1.6 | Go default UA; риск в README; при необходимости владелец добавит опцию |
| O10 | gtl: логи в stdout с pid/hostname, `--version` → `1.0.2` | 2.3 | Логи всех сервисов в stderr через общий `logging`; `scd --version` печатает версию сборки; у `scd gtl` своего `--version` нет |
| O11 | Кривой percent-encoding в label (`#100%`): TS валит весь источник (`URI malformed`) | 1.7 | По-Go: skip строки `invalid_uri` «The line label is not valid percent-encoding.» |
| O12 | sfc `serve` делает refresh дважды подряд при старте (начальный + немедленный первый тик cron, 04 §12 п.1) | 3.4 | По-Go: начальный refresh, затем тики по расписанию (без второго немедленного) |

## Трассировка решений D1–D23

| D | Суть | Задачи |
|---|---|---|
| D1 | Go 1.27, свежие deps, CI vet/lint/govulncheck/race | 1.1 (go.mod, .golangci.yml, Go-джоба), 7.2 (финальный CI) |
| D2 | Xray API строго: RPC, порядок, fixed, rollback, хэши, repair/rejoin, lock, тексты | 5.1, 5.3, 5.4, 6.1, 6.3, 5.6 |
| D3 | Дашборд визуально тот же, JSON API контракт | 0.4, 6.4, 6.5, 6.6 |
| D4 | xray-core pseudo-version + образ 26.10.10, обновлять вместе | 4.3 (`go get` по коммиту → go.mod), 5.6 (CI-скачивание релиза), 7.1 (compose, `.xray-version`), 7.2 (CI-проверка коммита ↔ тега) |
| D5 | нативные пакеты xray-core (перекрыт D19) | 4.5, 5.1 |
| D6 | ws path/Host и TLS fingerprint доходят до Xray | 4.5 (unit), 5.6 (live) |
| D7 | Немедленный graceful shutdown | 1.4 (cronloop), 2.3, 3.4, 6.7 |
| D8 | RE2, `/p/flags` с `i,m,s`, lookaround → ошибка | 1.2 (regexlit), 3.1, 4.1, 7.3 (README) |
| D9 | Поведение по-Go | все; явно 1.2–1.7, 4.3, 6.4 |
| D10 | Один бинарь/образ/модуль `scd`, sfc/gtl подкоманды, compose на одном образе | 1.1, 2.3, 3.4, 7.1, 7.2 |
| D11 | Подписки адаптированы под v26.10.10 | 4.3, 4.4, 4.5, 5.6 |
| D12 | Тестовые конфиги `docker/*/config.example.yml` | 2.3, 3.4, 4.1, 6.6, 7.1 |
| D13 | TS не трогаем; SpiderX/Y всегда + регресс-тест | 4.5, 5.6 |
| D14 | + xhttp, ML-KEM, `spx`/`pcs`/`vcn`/пустой `sid`; ech/fm/неподдерж. encryption → skip | 1.7, 4.3, 4.4, 4.5 |
| D15 | `allowInsecure` deny/allow | 4.1, 4.3 |
| D16 | flow: пустой норма, недопустимый для транспорта → сброс + warning | 4.3, 4.4 |
| D17 | пустой `sid` разрешён | 4.3, 4.5 |
| D18 | id — любая непустая строка, канонизация как в Xray | 1.7, 4.3 |
| D19 | JSON → `infra/conf` → gRPC; все параметры доходят до Xray | 4.5, 5.2, 5.6 |
| D20 | TS удаляется последним; эталоны — Go testdata | 0.1–0.4, 7.3 |
| D21 | Сдвиг портов допустим, проверка ёмкости `portRange` | 4.7 |
| D22 | Balancer selection на дашборде | 4.1 (конфиг), 5.5 (RPC), 6.6 (цикл, state, API, UI, `docker/xray/config/01_api.json` + `ObservatoryService`), 7.1 (compose-smoke проверяет `balancerSelection`) |
| D23 | Ошибка портов валит только target; cron robfig (+ отказ от никогда не срабатывающих выражений); SOCKS общий дедлайн; panic → 500; сортировки; порядок ключей JSON; README-ограничения | 6.1, 1.4, 6.2, 1.5/6.4 (httpserve), 6.4, 7.3 |

## Трассировка инвентарей тестов

Каждый ID — из соответствующего документа. «Изм.» — тест переписан по решению (причина в скобках).

| Инвентарь | ID | Задача |
|---|---|---|
| 01 §17.1 parser | P1, P2, P7, P8, P9, P10, P19, P20 | 4.6 |
| | P3, P5 | 4.6 (фильтры) |
| | P4 | 4.6, изм. (lookahead в RE2 недоступен; пример переписан на `/,\s*Extra\s*$/i`, D8) |
| | P6 | 4.2 |
| | P11, P12 (изм.: без проверки «нет заголовков», D9), P13–P18 | 1.6 |
| | P21, P22 | 4.5, изм. (проверка proto через `GetInstance`, а не подстрок raw-байт) |
| | P23, P24 | 5.3 |
| | P25 | 4.6 (generate) |
| 01 §17.2 topology | T1, T2, T3 | 4.7 |
| | T4, T5 | 6.4 |
| 01 §17.3 service | S1–S14 | 4.1 (S6, S8 изм.: тексты Go; S8 — RE2) |
| | S15 | 4.6 (сортировка `strings.Compare`) |
| | S16, S17 | 5.4 |
| | S18, S19, S20 | 6.1 |
| 01 §17.4 доп. | все пункты | 1.7 (URI), 1.6 (base64), 4.2 (теги, sha8), 4.3 (encryption/headerType/allowInsecure/combo), 4.1 (конфиг), 4.6 (CLI), изм.: `#100%` → O11, BOM/NEL → TrimSpace |
| 02 §6 уровни 1–4 | unit apply / bufconn contract / семантика билдеров / smoke | 5.3–5.4 / 5.1 / 4.5 / 5.6 + 7.2 |
| 03 §10.1 run-daemon | 1–4 | 1.4 |
| 03 §10.2 socks-http | 1–7 + доп. | 6.2 (изм. таймауты — общий дедлайн, D23) |
| 03 §10.3 aggregation | 1–3 | 6.4 |
| 03 §10.4 status-server | 1, 2, 3, 4, 8 (HTML) | 6.5 |
| | 5, 6, 7, 9, 10, 11, 12 (JSON, runtime-state маршрут) | 6.4 |
| 03 §10.5 runtime-state | 1–6 | 6.4 (форма против эталонов 0.4; значения `parsed` ws/fp — изм., D6) |
| | 7, 8 (группировка, сортировка) | 6.4 (изм.: `strings.Compare`, D23) |
| 03 §10.6 monitoring | 1–25 | 6.3 |
| 03 §10.7 service runtime | 1–8 | 6.1 |
| 03 §10.8 доп. | хэши/topology при частичной и полной ошибке, порты | 6.1 (порты — изм., D23) |
| | маршруты status server | 6.4 (изм., O8) |
| | golden HTML/JSON | 6.4 (JSON), 6.5 (HTML → visual, D3) |
| | cron `next()`, первый тик, быстрый выход | 1.4 |
| 04 §11 sfc | 1–4 (cron-цикл) | 1.4, 3.4 |
| | 5–12 (сервер) | 3.3 |
| | 13–16 (конфиг) | 3.1 |
| | 17–24 (refresh, кэш) | 3.2 (п.20 изм.: текст `cacheError` — Go) |
| 05 §10 gtl | 1–10 (конфиг) | 2.1 (тексты — изм., D9) |
| | 11–20 + доп. (HTTP, payload) | 2.2 |
| 09 §7.11 | фикстуры, ссылки 3x-ui/v2rayN, neg[0..12], SpiderY | 4.3, 4.4, 4.5, 5.6 |
| 10 §6.9 | 1 (декодирование, ошибки) | 5.5 |
| | 2–4 (ResolveSelection, сопоставление тегов, тик) | 6.6 |
| | 5–6 (JSON, HTML) | 6.6 |
| | 7 (конфиг) | 4.1 |
| | 8 (live) | 5.6 |

## Сознательно не делается

| Что | Почему |
|---|---|
| Эмуляция zod-текстов, pino-формата, ICU-коллации, lenient base64 Node, WHATWG URL, `User-Agent: node` | D9 |
| Hotfix SpiderY в TS | D13 |
| `OverrideBalancerTarget`, `TestRoute` | D22 п.4, 10 §6.8 |
| `pqv`, grpc, httpupgrade, kcp, ech, finalmask (`fm`), `headerType=http` | D14 (ссылки с `ech`/`fm` → skip `unsupported_param`) |
| Lua `routing.script`, частные CA, SOCKS UDP снаружи Docker — код | D23: только README |
| Cron `L`/`W`/`#`/`H`, пустое расписание sfc | D23 (ошибка `validate-config`) |
| Байтовое совпадение HTML и JSON-форматирования (отступы, порядок ключей remote/local) | D3, D23 |
| Публикация образов `sfc`/`gtl`, `blanc`, multicall | D10 |
| Persist хэшей/state между рестартами | не было в TS (03 §RS), D2 |
| Hot-reload конфига | не было в TS |

## Review Focus (общий для всех этапов)

Пять классов входов/сбоев, которые спецификация подразумевает, но инвентари не покрывают. Тест на каждый добавлен в задачу-владельца.

1. **SIGTERM посреди apply.** Ожидание: текущая последовательность RPC apply/repair/rejoin доводится до конца (контекст мутаций — `context.WithoutCancel` + собственный таймаут), новые тики не стартуют, процесс выходит. Тест — задача 6.7 (`TestDaemonShutdownWaitsForInFlightApply`).
2. **Xray недоступен при старте daemon.** Ожидание: sync-отчёт с failed по каждому kind, status server отвечает, следующий тик повторяет apply. Тест — 6.7 (`TestDaemonStartsWhenXrayDown`).
3. **Подписка с CRLF/BOM/пустыми строками/хвостовыми пробелами.** Ожидание: те же entries и теги, что у чистого текста (кроме строк, у которых меняется `raw`). Тест — 1.6 (`TestLoadFileWithBOM`), 4.6 (`TestBuildManifestCRLFEqualsLF`).
4. **Переполнение `portRange` у одного target.** Ожидание (D23): только этот target failed с понятным текстом, остальные применены. Тест — 6.1 (`TestSyncPortRangeOverflowFailsOnlyThatTarget`).
5. **Враждебные строки в remote snapshot / label** (`<script>`, кавычки, очень длинный label). Ожидание: дашборд экранирует (html/template), JSON passthrough без изменений, ничего не падает (panic → 500, D23). Тест — 6.5 (`TestDashboardEscapesHostileStrings`), 6.4 (`TestRemoteSnapshotPassthroughKeepsUnknownFields`).
6. **Ссылка, которую `infra/conf` принимает, а живой Xray отвергает на `AddOutbound`** (добавлено ревью: ML-KEM-ключ правильной длины, но не валидный ключ — `conf.Build()` OK, `AddOutbound` → `proxy/vless/outbound: failed to use encryption > mlkem: invalid polynomial encoding`, проверено на 26.10.10). Ожидание: строка пропускается на этапе манифеста (`unsupported_value`), а не валит outbound-apply всего target'а с rollback. Тест — 4.4 (`TestMLKEMInvalidKeySkipped`), 5.6 (`TestLiveEveryParamReachesXray` — все принятые entries добавляются без ошибок).
