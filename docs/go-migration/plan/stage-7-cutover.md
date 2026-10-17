# Этап 7. Переключение: Docker, compose, CI/publish, документация, полное удаление TypeScript

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development; TDD — где применимо (скрипты проверок пишутся так, чтобы сначала падать). Прочитать «Глобальные ограничения» в `docs/go-migration/plan/README.md`. Docker в песочнице есть (buildx, compose v2; Docker Hub и `gcr.io/distroless` доступны), недоступны только слои `ghcr.io` — Xray-образ подменяется локальным (README плана, «Локальный образ Xray»). Docker-шаги (сборка образа, compose-smoke) прогоняются локально, CI их повторяет.

**Goal:** один Dockerfile и один образ `ghcr.io/a1ekseev/scd`, compose с тремя сервисами на этом образе и Xray 26.10.10, CI/publish только на Go, итоговые `README.md`/`AGENTS.md`, удаление `scd/`, `sfc/`, `gtl/`, `tools/golden/` и любых упоминаний npm/Node/TypeScript.

**Architecture:** multi-stage Dockerfile (`golang:1.27.2-alpine` → `gcr.io/distroless/static-debian13`), кросс-компиляция amd64/arm64 через `$BUILDPLATFORM` без QEMU. Скрипты проверок в `scripts/` (bash). Эталоны `testdata/reference/` остаются как Go-testdata (D20).

**Tech Stack:** Docker Buildx, GitHub Actions (`actions/checkout@v7`, `actions/setup-go@v7`, `golangci/golangci-lint-action@v9`, `docker/setup-buildx-action@v4`, `docker/build-push-action@v7`, `docker/login-action@v4`, `docker/metadata-action@v6`, `browser-actions/setup-chrome@v2`).

**Spec:** `07` D1, D4, D10, D12, D20, D22, D23; `06` §11 (с поправкой D10: без target'ов и симлинков); `08` §5, §7 (CI-сверка версии Xray); `10` §6.10; README плана «Открытые вопросы», «Сознательно не делается».

---

### Task 7.1: Dockerfile, compose, docker-примеры и compose-smoke

**Read:** `07` D4, D10, D12, D22; `06` §11 «Dockerfile», «Выбор базы»; текущие `docker-compose.yml`, `docker/README.md`, `docker/*/config.example.yml`, `scd/Dockerfile`, `sfc/Dockerfile`, `gtl/Dockerfile`.

**Files:**
- Create: `Dockerfile`, `.dockerignore` (`.git`, `bin`, `docs`, `testdata`, `**/config.yml`, `**/.sfc-cache`, `scd`, `sfc`, `gtl`, `tools`)
- Create: `.xray-version` (одна строка `26.10.10 701af60772cd`)
- Create: `scripts/check-xray-version.sh`, `scripts/compose-smoke.sh`, `docker/compose.smoke.yml` (override для smoke: монтирует `./docker/<svc>/config.example.yml` как `/app/config/config.yml` для scd/sfc/gtl — игнорируемые `config.yml` не трогаются)
- Modify: `docker-compose.yml`, `docker/README.md`

**Interfaces:**
- `Dockerfile`: stage `build` — `FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine`, `ARG TARGETOS TARGETARCH VERSION=dev`, `COPY go.mod go.sum ./` + `RUN --mount=type=cache,target=/go/pkg/mod go mod download`, `COPY . .`, `RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/scd ./cmd/scd`; stage runtime — `FROM gcr.io/distroless/static-debian13` (запинить по digest на момент реализации, комментарий с тегом; выбран debian13 по D1 — самые свежие версии), `COPY --from=build /out/scd /usr/local/bin/scd`, `WORKDIR /app`, `CMD ["scd","daemon","--config","/app/config/config.yml"]` (D10). Пользователь root (совместимость с volume `.sfc-cache`). Прототип ревью (тот же Dockerfile на модуле с каталогами `scd/ sfc/ gtl/` рядом): `docker buildx build` amd64 и arm64 (кросс-компиляция без QEMU) проходят; в образе `PATH` содержит `/usr/local/bin`, exec-form `CMD ["scd",…]` находит бинарь, `/etc/ssl/certs/ca-certificates.crt` есть, uid 0; `-X main.version` доходит; `.dockerignore` с `scd`/`sfc`/`gtl` исключает только корневые каталоги — `internal/scd` попадает в контекст.
- `docker-compose.yml`: `xray` — `image: ghcr.io/xtls/xray-core:26.10.10` (остальное как сейчас); `scd`, `sfc`, `gtl` — `build: {context: .}`, `image: ghcr.io/a1ekseev/scd:local`, `command:` соответственно (по умолчанию CMD) / `["scd","sfc","serve","--config","/app/config/config.yml"]` / `["scd","gtl","serve","--config","/app/config/config.yml"]`; порты и volumes — как сейчас.
- `scripts/check-xray-version.sh`: читает `.xray-version`; проверяет, что `go.mod` содержит `github.com/xtls/xray-core v1.260327.1-0.20261010092107-<commit>` с тем же коммитом и `docker-compose.yml` содержит `ghcr.io/xtls/xray-core:<version>`; иначе exit 1 с понятным сообщением (D4).
- `scripts/compose-smoke.sh`: **не** копирует и не перезаписывает `docker/*/config.yml` (там могут быть реальные конфиги владельца, D12) — использует `docker compose -p scd-smoke -f docker-compose.yml -f docker/compose.smoke.yml up -d --build`; ждёт до 60 s (опрос, не `sleep`); проверяет: `curl -fsS http://127.0.0.1:9092/load/1 | sha256sum` = `8ab78394…cb01`; `curl -fsS http://127.0.0.1:9091/s/test-all` декодируется base64 в 8 строк; `curl -fsS http://127.0.0.1:9090/api/instance-status | jq -e '.version==1'` (опросом до 60 s); `curl -fsS 'http://127.0.0.1:9090/api/runtime-state?subscriptionId=test-fixture&targetAddress=xray:8080' | jq -e '.diff.outbounds.missing==[] and (.diff.outbounds.matched|length)>0'` (sync реально применил ресурсы в Xray 26.10.10); при `balancerSelection` в примере — `curl -fsS http://127.0.0.1:9090/api/instance-status | jq -e '.subscriptions[0].targets[0].balancerSelection.state != null'` (ключ — в `GroupedStatusTarget`, `10` §6.6); `trap` на выход: при провале `docker compose … logs`, всегда `docker compose -p scd-smoke … down -v`. Режим compose с тремя сервисами на одном `image:` + одинаковым `build:` проверен ревью: compose v5 собирает образ один раз (bake) и именует его трижды, каждому сервису передаётся свой `command:`.
- `docker/README.md`: команды проверки без Docker → `go build -o bin/scd ./cmd/scd && ./bin/scd validate-config --config docker/scd/config.example.yml`, `./bin/scd sfc validate-config …`, `./bin/scd gtl validate-config …`; описание одного образа и `command:`; упоминание `ObservatoryService` (D22) и `balancerSelection`.

- [ ] **Step 1: Failing check:** `bash scripts/check-xray-version.sh` до правки compose → exit 1 (`docker-compose.yml` ещё на 26.3.27).
- [ ] **Step 2: Implement** Dockerfile, compose, скрипты, README.
- [ ] **Step 3: Verify (локально):** `bash scripts/check-xray-version.sh` → 0; `go build -o bin/scd ./cmd/scd && for s in "" sfc gtl; do ./bin/scd $s validate-config --config docker/${s:-scd}/config.example.yml || break; done`; `docker buildx build --platform linux/arm64 -o type=cacheonly .` (arm64 собирается без QEMU); локальный образ Xray по README плана (тег `ghcr.io/xtls/xray-core:26.10.10`), затем `docker compose -f docker-compose.yml -f docker/compose.smoke.yml config -q && bash scripts/compose-smoke.sh` → exit 0; `docker compose -p scd-smoke -f docker-compose.yml -f docker/compose.smoke.yml ps -aq` → пусто (smoke убрал за собой). Внимание: в compose заданы фиксированные `container_name` (`xray-test`, `scd`, `sfc`, `gtl`) — smoke конфликтует с уже запущенным локальным стеком; скрипт должен проверить это заранее и выйти с понятным сообщением. В CI (7.2) smoke повторяется с настоящим образом из ghcr.
- [ ] **Step 4: Commit** `build(docker): single scd image, compose on one image with Xray 26.10.10` (body: D4, D10, D12).

---

### Task 7.2: CI и publish только на Go

**Read:** `07` D1, D4, D10; `06` §11 «GitHub Actions»; текущий `.github/workflows/docker.yml` (джобы `verify`, `go`, `publish`).

**Files:**
- Modify: `.github/workflows/docker.yml` (переписать целиком)

**Interfaces:**
- Джобы (триггеры как сейчас: PR, push в `master`, теги `v*.*.*`):
  1. `verify` (PR/master): checkout → setup-go@v7 (`go-version-file: go.mod`, cache; setup-go читает директиву `toolchain go1.27.2` — проверено по исходникам v7) → `go mod verify` → `bash scripts/check-xray-version.sh` → `go vet ./...` → golangci-lint-action@v9 `version: v2.15.0` (релизный бинарь собран go1.27.0 — проверено) → `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` → `go test -race -count=1 ./...` → скачать Xray по версии из `.xray-version` (`Xray-linux-64.zip` + `.dgst`, сверка sha256) → `SCD_XRAY_BIN=… go test -tags xraylive -race -count=1 ./internal/scd/livetest/...` → `browser-actions/setup-chrome@v2` с `id: chrome` → `SCD_CHROME=${{ steps.chrome.outputs.chrome-path }} go test -tags visual ./internal/scd/status/...` → `go build -o bin/scd ./cmd/scd` → `./bin/scd validate-config --config docker/scd/config.example.yml` (+ sfc, gtl).
  2. `smoke` (PR/master, `needs: verify`): `docker/setup-buildx-action@v4` → `docker compose -f docker-compose.yml -f docker/compose.smoke.yml config -q` → `bash scripts/compose-smoke.sh`.
  3. `publish` (теги): `needs` не используется (как сейчас), но повторяет `go test -race ./...`; `docker/login-action@v4` → `docker/metadata-action@v6` (`images: ghcr.io/a1ekseev/scd`, теги `type=raw,value=latest` и `type=semver,pattern={{version}}`, labels title `scd`, description `Subscription Control Daemon (scd, sfc, gtl)`) → `docker/build-push-action@v7` (`context: .`, `file: ./Dockerfile`, `platforms: linux/amd64,linux/arm64`, `build-args: VERSION=${{ github.ref_name }}`, cache gha). Без QEMU, без setup-node, без npm, без образов `sfc`/`gtl`.
- Удалить переменную `FORCE_JAVASCRIPT_ACTIONS_TO_NODE24` только если ни одно action её не требует (она про runtime actions, не про проект; оставить, если сомнение — это не упоминание Node в проекте; в отчёте указать решение).

- [ ] **Step 1: Failing check:** `grep -nE 'setup-node|npm |node-version|qemu|/sfc/Dockerfile|/gtl/Dockerfile|a1ekseev/(sfc|gtl)' .github/workflows/docker.yml` → есть совпадения (до правки).
- [ ] **Step 2: Implement** новый workflow.
- [ ] **Step 3: Verify:** тот же grep → пусто; YAML валиден (`go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/docker.yml` → без ошибок).
- [ ] **Step 4: Commit** `ci: verify, smoke and publish the Go-only scd image` (body: D1, D4, D10).

---

### Task 7.3: Удаление TypeScript, README.md и AGENTS.md

**Read:** `07` D8, D10, D11, D14–D16, D20–D23; `10` §6.10 (README); `08` §8 п.3–5; README плана «Открытые вопросы» (O1–O12) и «Сознательно не делается»; текущие `AGENTS.md`, `scd/AGENTS.md`, `sfc/AGENTS.md`, `gtl/AGENTS.md`, `scd/README.md`, `sfc/README.md`, `gtl/README.md`.

**Files:**
- Delete: `scd/`, `sfc/`, `gtl/` целиком (`git rm -r`), `tools/golden/`
- Modify: `.gitignore` (убрать Node-записи `node_modules/`, `dist/`, `.npm/`, `.pnpm-store/`, `.yarn/`, `.turbo/`, `*.tsbuildinfo`, `scd/*`, `xtls-sdk/`; оставить `**/config.yml`, `**/.sfc-cache/`, `/bin/`, `coverage/`, `.cache/`)
- Create: `README.md` (корень)
- Modify: `AGENTS.md` (корень; сервисные AGENTS.md удаляются вместе с каталогами, их содержание переносится разделами)
- Modify: `testdata/reference/README.md` (убрать инструкции по пересъёмке через `tools/golden`, указать коммит, на котором эталоны сняты)
- Create: `scripts/check-no-node.sh`

**Interfaces:**
- `README.md` — разделы: что это (scd/sfc/gtl в одном бинаре); сборка (`go build -o bin/scd ./cmd/scd`), команды (`scd sync|daemon|generate|validate-config|print-config`, `scd sfc …`, `scd gtl …`); Docker (один образ, compose, `command:`); конфиги трёх сервисов (перенос из сервисных README, с ключами `allowInsecure` (D15) и `balancerSelection` (D22)); фильтры — RE2 и `/pattern/flags` с `i,m,s`, примеры без lookahead (D8; переписать примеры `scd/README.md` «Regex examples»); поддерживаемые ссылки/профили и что пропускается (D14–D18, O1–O4, O11); Balancer selection (`10` §6.10: семантика стратегий, `ObservatoryService`+observatory обязательны, repaired-туннели вне балансера, предупреждение про override); известные ограничения Xray v26.10.10 (D23: встроенный CA-bundle, SOCKS UDP ASSOCIATE на эфемерном порту, `routing.script` не поддерживается; O5 `RemoveRule("")`); заметки миграции (D10 образы `sfc`/`gtl` не публикуются, новые `command:`; D11 одноразовый полный re-apply и смена `jsonOutbound`; D21 сдвиг портов и проверка `portRange`; cron без `L/W/#/H`, без `@minutely|@secondly|@weekdays|@weekends`, без дня недели `7` и пустого расписания, а никогда не срабатывающие выражения (`0 0 31 2 *`) — ошибка (D23; `@midnight`/`@annually` теперь принимаются); логи — slog (D9); O8, O9, O10, O12); разработка (`go vet`, `go test -race ./...`, `golangci-lint run`, live/visual-теги и env `SCD_XRAY_BIN`/`SCD_CHROME`).
- `AGENTS.md` — разделы: Repository Shape (один Go-модуль `github.com/a1ekseev/scd`, бинарь `scd`, раскладка `cmd/`, `internal/{cli,common,scd,sfc,gtl}`); Common Rules (Go `1.27`, `internal/{scd,sfc,gtl}` не импортируют друг друга — проверяет `internal/archtest`; общий код только `internal/common`; xray-core и образ Xray обновлять вместе, `.xray-version`; `**/config.yml` ignored); Checks (`go vet ./...`, `go test -race ./...`, `golangci-lint run`, `go build -o bin/scd ./cmd/scd`, `./bin/scd [sfc|gtl] validate-config --config docker/<svc>/config.example.yml`, live/visual); CI Contract; Docker Image (`ghcr.io/a1ekseev/scd`); краткие service notes (содержательная часть `scd/AGENTS.md` «Main Architecture» переписанная на Go-пакеты, `sfc/AGENTS.md` правила outputs/failure, `gtl` контракт payload).
- `scripts/check-no-node.sh`: exit 1, если `git ls-files -- ':!internal/scd/status/web/'` содержит `*.ts`, `*.js`, `*.mjs`, `package.json`, `package-lock.json`, `tsconfig*.json` (исключение — браузерный `internal/scd/status/web/dashboard.js` из 6.5: без него проверка была бы красной всегда), или если `git grep -nIiE '\b(npm|node|nodejs|node\.js|typescript|tsc|node_modules|pino|zod)\b' -- README.md AGENTS.md Dockerfile .dockerignore docker-compose.yml docker .github scripts ':!scripts/check-no-node.sh'` что-то находит (код Go не проверяется этим grep'ом: CSS-класс `node-card` дашборда легитимен; `docs/go-migration` — исторические документы миграции, их не трогаем).

- [ ] **Step 1: Failing check:** `bash scripts/check-no-node.sh` → exit 1 (TS ещё на месте).
- [ ] **Step 2: Убедиться, что Go-тесты не зависят от TS-исходников:** `git grep -n "scd/src\|sfc/src\|gtl/src" -- '*.go'` → пусто (иначе перенести нужное в `testdata/` перед удалением).
- [ ] **Step 3: Удалить** `scd/`, `sfc/`, `gtl/`, `tools/golden/`; обновить `.gitignore`.
- [ ] **Step 4: Написать** `README.md`, `AGENTS.md`, обновить `testdata/reference/README.md`.
- [ ] **Step 5: Verify (полная):**
  ```bash
  bash scripts/check-no-node.sh
  bash scripts/check-xray-version.sh
  go mod tidy && git diff --exit-code go.mod go.sum
  go vet ./... && go vet -tags 'xraylive visual' ./...
  golangci-lint run
  go test -race -count=1 ./...
  go build -o bin/scd ./cmd/scd
  ./bin/scd validate-config --config docker/scd/config.example.yml
  ./bin/scd sfc validate-config --config docker/sfc/config.example.yml
  ./bin/scd gtl validate-config --config docker/gtl/config.example.yml
  ```
  Expected: всё с кодом 0.
- [ ] **Step 6: Commit** `chore: remove TypeScript services and document the Go layout` (body: D20; перечислить удалённые каталоги).
