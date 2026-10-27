# Ревью плана миграции на Go (2026-10-17)

Ревьюер проверил каждый stage-файл плана (README + stage-0…7, 41 задача) по коду репозитория, документам 00–10 и реальному toolchain/библиотекам/Xray. Эксперименты шли в Docker и на хосте; все артефакты лежат в `/tmp/claude-0/-home-user-scd/b738d151-5a61-5b3f-9a86-51bb9c74ebc7/scratchpad/review/` (дальше `$R`). Код, конфиги, CI и compose в репозитории не менялись. Изменены только файлы `docs/go-migration/plan/*.md`.

> **Примечание (после D24–D27, 2026-10-17).** Номера задач ниже — по состоянию на момент ревью. После обновления плана под D24–D27 и матрицу `11-xray-vless-matrix.md`: добавлена задача 0.5 (корпус D26); нормализация разбита на 4.3 (scope/flow/ML-KEM, бывшая часть 4.4 про ключи ML-KEM), 4.4 (TLS/REALITY), 4.5 (транспорт); бывшие 4.5/4.6/4.7 → **4.6/4.7/4.8**; бывшая 5.6 → **5.6** (стенд + параметры) и **5.7** (REALITY, apply, паритет, balancer). Находки B1 (подавление логов — теперь 4.6), B2 (ключи ML-KEM — 0.5/4.3/5.6), M2 (cron — уточнено D24: `?` и `TZ=` тоже отвергаются), M4 (P1/P2 — 4.7), M11 (REALITY live — 5.7) сохранены в новых задачах.

## Вердикт

Структура плана, порядок зависимостей, трассировка D1–D23 и инвентарей тестов в целом корректны. Ключевые технические допущения подтвердились экспериментами:
- toolchain go1.27.2 и модуль в корне рядом с TS-каталогами;
- pseudo-version xray-core и API `infra/conf`;
- живые RPC Xray 26.10.10, bufconn и synctest;
- distroless, buildx arm64 без QEMU, compose на одном образе;
- релизный ассет Xray для CI.

В исходном виде план **не исполнялся бы** без вмешательства. Найдено 3 blocker'а, 12 major-находок и 13 minor-находок; все исправлены в файлах плана. Решения владельца (D1–D23) и умолчания O1–O12 не менялись. Технически неосуществимых умолчаний нет.

## Находки

Severity: **B** — blocker (задача/проверка гарантированно падает или ломает продукт), **M** — major (неверная инструкция, ведущая к ошибке или потере требования), **m** — minor.

| id | sev | Где в плане | Что было не так | Доказательство | Исправление |
|---|---|---|---|---|---|
| B1 | B | 4.5 «Подавление логов» (условное), 4.6 Step 7, 6.1 Step 6 | `common/log` в `init()` регистрирует логгер в **stdout**. `conf.Build()` печатает туда `[Warning] … WebSocket transport … deprecated` для каждого ws-outbound. JSON-вывод `scd generate`/`sync` засоряется, и `\| jq .parsed` падает. План делал подавление необязательным | `xray-core/common/log/logger.go:186` `RegisterHandler(NewLogger(CreateStdoutLogWriter()))`; `$R/xexp/bbuild 2>/dev/null \| grep -c Warning` → 3, в stderr → 0; после `xlog.RegisterHandler(sink)` stdout чист (`$R/xexp/silent`) | 4.5: регистрация handler'а обязательна, добавлен тест `TestToProtoIsSilent`; в 4.6/6.1 пометки |
| B2 | B | 0.2 корпус: «ключ ML-KEM = 1184 случайных байт» | Ключ из случайных байт проходит `conf.Build()`, но живой Xray отвергает его на `AddOutbound`. Поэтому `TestLiveEveryParamReachesXray` (5.6) упал бы. В продукте одна такая ссылка валила бы outbound-apply всего target'а с rollback | live (`$R/xexp/live.out`): `AddOutbound x-observe-mlkem-rnd ERR … failed to use encryption > mlkem: invalid polynomial encoding`; валидный ключ из `xray vlessenc` → OK и `proto.Equal` в `ListOutbounds` | 0.2: позитивные ключи берутся из `xray vlessenc`, случайный ключ оставлен как негативный кейс; 4.4: валидация ключей (`ecdh.X25519().NewPublicKey` / `mlkem.NewEncapsulationKey768`) → skip `unsupported_value` + `TestMLKEMInvalidKeySkipped`; README Review Focus 6 |
| B3 | B | 6.5 создаёт `internal/scd/status/web/dashboard.js`; 7.3 `check-no-node.sh` и DoD этапа 7 запрещают любые `*.js` в `git ls-files` | Проверка 7.3 и DoD были бы красными всегда | `git ls-files` после 6.5 содержит `.js`; grep DoD `\.(ts\|js)$` | Каталог `internal/scd/status/web/` исключён явно в 7.3 и в DoD (README); пометка в 6.5 |
| M1 | M | README «Глобальные ограничения», шапка stage-7 | «Docker-демона в песочнице нет». На самом деле Docker есть, недоступны только блобы `ghcr.io`. Из-за этого compose-smoke и сборка образа откладывались на CI | `docker info` → Server 29.8.2; `docker pull gcr.io/distroless/static-debian13` OK; `ghcr.io/v2/xtls/xray-core/manifests/26.10.10` → 200, блобы — нет | README: рецепт локального образа Xray из релизного zip с тегом `ghcr.io/xtls/xray-core:26.10.10` (compose его не тянет — проверено); 7.1: локальные docker-проверки |
| M2 | M | 0.2 `cron-rejected.json`, 1.4 `cron.Parse` | (а) `@midnight` в списке «обязан отвергать», но robfig его принимает, и `TestParseRejects` падал бы. (б) robfig принимает никогда не срабатывающие `0 0 31 2 *` и возвращает нулевой `Next`: cronloop ушёл бы в busy-loop. (в) TS-дескрипторы `@minutely/@secondly/@weekdays/@weekends` Go отвергает, а план это не документировал | `$R/cron/compare.out`: все 16 выражений `cron-next` совпадают; `@midnight` → go OK / node ERR; `0 0 31 2 *` → go `0001-01-01…` / node ERR | 0.2: список исправлен; 1.4: `Parse` отвергает zero-`Next` и `TZ=`/`CRON_TZ=`, guard в `cronloop`, новые тесты; 7.3 README: миграционные заметки |
| M3 | M | 1.7 шаг 4 (O11) | Label брался из `url.Parse(...).EscapedFragment()`, но `url.Parse` сам падает на `#100%` на шаге 2. Причина «The line label is not valid percent-encoding.» была недостижима, тест O11 падал | `$R/urlexp`: `parse "vless://u@h:443#100%": invalid URL escape "%"` | Фрагмент отрезается `strings.Cut(trimmed,"#")` до `url.Parse` |
| M4 | M | 4.6 P1/P2 | При дефолте D15 `deny` две ws-строки `vpn-sample.txt` (`allowInsecure=1` без pcs/vcn) пропускаются, профиля `ws-tls` нет, P2 падал бы | `scd/test/fixtures/vpn-sample.txt` строки 9–10; TS на node:24 даёт 12 entries (8 tcp-tls, 2 ws-tls, 2 reality) | Тесты разделены на `…AllowPolicy` (12 entries, 3 профиля, warnings) и `…DenyPolicy` (10 entries, 2 skip) |
| M5 | M | 6.5 visual test | `chromedp@latest` = **v0.21.0** с новым generic API. Привычные `chromedp.Run(ctx, a, b)` и `FullScreenshot(&buf, q)` не компилируются | `go build` в `$R/cdp`: `too many arguments in call to chromedp.Run … want (context.Context, chromedp.Action[T])` | В 6.5 прописан рабочий API (`chromedp.Do`, `chromedp.Run(ctx, chromedp.FullScreenshot(100))`); проверен на хосте с `headless_shell` и в контейнере `chromedp/headless-shell` |
| M6 | M | 0.4 `screenshots.mjs` | Без `--no-sandbox` headless_shell под root завершается и не создаёт PNG | `Running as root without --no-sandbox is not supported`; с флагом PNG ровно 1280×2400 и 390×2400 | Флаг добавлен; скрипт запускается на хосте; fallback заменён на полный Chromium из `/opt/pw-browsers` |
| M7 | M | README (toolchain), 1.1 Step 5/6 | Локально `go run golang.org/x/vuln/cmd/govulncheck@latest` на Go 1.24.7 + `GOTOOLCHAIN=auto` собирается go1.26 и не может разобрать пакеты go1.27. Кроме того, `vuln.go.dev` в песочнице закрыт | `package requires newer Go version go1.27 (application built with go1.26)`; с `GOTOOLCHAIN=go1.27.2`: `fetching vulnerabilities … Forbidden` | README: `GOTOOLCHAIN=go1.27.2` для всех `go run/install tool@ver`; govulncheck только в CI (там toolchain 1.27.2 от setup-go) |
| M8 | M | 1.1 `.golangci.yml`, «Lint» | Набор линтеров валиден, но revive по умолчанию требует `// Package …` и doc-комментарии у всего экспортируемого, а errcheck ругается на `defer resp.Body.Close()`. Subagent'ы без контекста получали бы красный lint на каждом пакете | `$R/gobin/golangci-lint config verify` OK; `run` на прототипе: `package-comments`, `exported: exported function … should have comment`, `errcheck … r.Body.Close` | Добавлены `linters.exclusions.presets: [std-error-handling]` и `golangci-lint fmt`, требования revive/noctx/errorlint описаны в «Lint» и 1.1 |
| M9 | M | 6.2 `TestConnectTimeout` | «Listener без Accept» не даёт таймаута connect: ядро завершает handshake в backlog | `$R/urlexp` (вариант с `DialTimeout`): `dial to non-accepting listener: <nil> 0s` | В `socks.Request` добавлен хук `Dial`; тест блокирует его до `ctx.Done()` |
| M10 | M | 7.1 `compose-smoke.sh` | Скрипт копировал `config.example.yml` → `config.yml`, то есть перезаписывал игнорируемые локальные конфиги, где по D12 будут реальные конфиги владельца | `.gitignore` `**/config.yml`; D12 | Override `docker/compose.smoke.yml` монтирует примеры напрямую; проект `-p scd-smoke`, `trap … down -v`; предупреждение о фиксированных `container_name` |
| M11 | M | 5.6 `TestLiveRealityDoesNotCrashXray` | Методика была «может быть, `t.Skip`», при том что регресс D13 обязателен. Неясно, работает ли `SSL_CERT_FILE` в процессе Xray | `$R/xexp/spider-with.out`: `REALITY: received real certificate`, Xray жив; `$R/xexp/spider-without.out`: `panic: index out of range [8] with length 0 … reality.go:298`, `exit status 2` | Тест стал детерминированным: `serverName: example.com`, ассерт на лог `received real certificate` + жив ли процесс; `t.Skip` только без `SCD_XRAY_BIN` |
| M12 | M | 0.3 `routes.json`/`server-cases.json` | `HEAD → 405` и нормализацию `/s/../s/test-all`, `/load/1?x=y` нельзя получить из `buildResponse`/`handleServerRequest`: метод проверяется в неэкспортируемом `handleRequest`, путь нормализует `new URL` в сервере | `gtl/src/runtime/server.ts:112-119`, `sfc/src/runtime/server.ts:84-86` | Явно: передавать `new URL(path,…).pathname`; HEAD снимать через `startServer` + `fetch` |
| m1 | m | README «Раскладка» | `Execute(os.Args, …)` противоречил 1.1 (`os.Args[1:]`) | 1.1 Files | Исправлено |
| m2 | m | README трассировка | D4 → «4.5 (go.mod)», но `go get` делается в 4.3; D22 → «7.1 (`01_api.json`)», но файл меняет 6.6 | stage-4 4.3 Step 3; stage-6 6.6 Files | Исправлено |
| m3 | m | 4.5 proto-golden mlkem | «Encryption без префикса `mlkem768x25519plus.`» неточно: conf отрезает `…<mode>.<rtt>.`, padding уходит в `Account.Padding`, `XorMode` 0/1/2, `Seconds` 1 для 0rtt | `infra/conf/vless.go:336-378`; `$R/xexp/build.out` `xor=0 sec=1` | Уточнено |
| m4 | m | 4.5 Step 5 | «~35 МБ» для `go build` без `-s -w`. Реально ~50 МБ, ~35 МБ даёт только stripped-сборка | `scratchpad/x2610/bin_subs` 50 820 365 / `bin_subs_stripped` 34 680 992 | Уточнено |
| m5 | m | 2.1, 3.1 типы конфигов | `Logging struct{ Level, Format string }` без json-тегов и `json:",omitempty"` без имени дают ключи `Level`/`Name`, и print-config-эталоны не совпали бы | семантика `encoding/json` | Теги прописаны явно |
| m6 | m | 3.1/3.2 escape id | Противоречие `url.PathEscape` vs `QueryEscape`. `PathEscape` расходится с `encodeURIComponent` TS на `+&=:@$` | `$R/urlexp`: `x+y&z=w:@$,;` → path=`x+y&z=w:@$%2C%3B`, query=`x%2By%26…` = TS | Единый `escapeID` = QueryEscape + `+`→`%20` (отличие от TS только на `!'()*`) |
| m7 | m | 6.5 / 7.2 CI visual | `steps.chrome.outputs.chrome-path` требует `id: chrome` | `browser-actions/setup-chrome@v2` `action.yml` (output `chrome-path`) | Добавлено |
| m8 | m | 2.3 Step 4 | Код выхода `serve` не проверялся | — | `kill -TERM; wait; echo exit=$?` |
| m9 | m | 5.6/7.2 CI-скачивание Xray | Нет проверки целостности; версия захардкожена | Ассет и `.dgst` существуют, sha256 совпал | Сверка `.dgst`, версия из `.xray-version` (7.2) |
| m10 | m | Stage 0 Tech Stack | Ориентир на Node 22 (ниже `engines`); Docker не использовался | `node:24-alpine` (24.22.0): scd 115/115, sfc 24/24, gtl 20/20 (`$R/ts24/node24-tests.txt`); ICU 22 vs 24 одинаковы (`$R/icu22.txt` = `$R/icu24.txt`) | Основной вариант — контейнер, шаблон команды в stage-0 |
| m11 | m | 6.1 Step 6 | `./bin/scd sync` на `xray:8080`: в песочнице имя резолвится прокси в посторонний HTTP/1.1, ошибка недетерминирована | `$R/xexp/bufc`: `error reading server preface … HTTP/1.1 header` | Смоук на копии конфига с `127.0.0.1:1` |
| m12 | m | 7.1 compose-smoke | Не указан эндпоинт проверки `balancerSelection` | 10 §6.6 | `/api/instance-status` |
| m13 | m | 0.2 Step 2 | У `tools/golden/` нет `node_modules`, поэтому `import 'cron-parser'` не разрешится | `$R/cron/next.mjs` работает с путём `…/scd/node_modules/cron-parser/dist/index.js` | Путь прописан |

## Подтверждено без изменений (ключевое)

- **Модуль в корне рядом с `scd/`.** `go list/vet/test ./...` и `golangci-lint run` не задевают TS-каталоги. `go build ./cmd/scd` без `-o` падает, как и предупреждал план.
- **xray-core.** `go get …@701af60772cda…` даёт ровно `v1.260327.1-0.20261010092107-701af60772cd`. `@latest` даёт `v1.260327.0`. `@v26.10.10` невалиден (`/v26`). Комментарий на строке require переживает `go mod tidy`, а `go get -u ./...` не откатывает версию.
- **`infra/conf`.**
  - Форма JSON из 4.5 принимается целиком: `tlsSettings:{}`, `pinnedPeerCertSha256`/`verifyPeerCertByName` как строки через запятую, `realitySettings.password/spiderX`, `wsSettings.host`, `?ed=2048` → `Ed`, `xhttpSettings.extra` (поля из extra доходят), `users[].email`, ML-KEM при `security:none`.
  - `allowInsecure:true` → ошибка `"allowInsecure" has been removed…`.
  - `SpiderY` всегда длины 10; `spx=/abc?p=10-20&c=2` → `[10 20 2 2 0…]`, `SpiderX="/abc"`; пустой sid → 8 нулевых байт.
  - `uuid.ParseString`: ≤30 символов → UUIDv5, 31 → ошибка, 32–36 — hex, иначе ошибка (O4 выполним).
  - `GetFingerprint("Chrome")==nil` (нужен lowercase).
  - Методы `GetEffectiveSecuritySettings/TransportSettings/Protocol` существуют.
  - `InboundDetourConfig` socks: `::` → 16-байтный ip, `localhost` → domain, иной домен → ошибка.
  - `RouterConfig` и прямой `router.RoutingRule{TargetTag: &RoutingRule_Tag{…}}` эквивалентны.
  - Вывод: `$R/xexp/build.out`.
- **gRPC-интерфейсы.** `HandlerServiceServer` (`Add/Remove/List Outbound|Inbound`), `RoutingServiceServer` (`AddRule/RemoveRule/ListRule/GetBalancerInfo`), `ObservatoryServiceServer.GetOutboundStatus` существуют. Сервис, не зарегистрированный на bufconn, даёт `Unimplemented` `unknown service xray.app.router.command.RoutingService`.
- **testing/synctest + bufconn + grpc** работают вместе (3 прогона с `-race` зелёные; `$R/xexp/synct`).
- **Live Xray 26.10.10** (`docker/xray/config` + `ObservatoryService`): все тексты ошибок из 02 §2.7/08 §3 совпадают; `ListOutbounds` возвращает отправленное (`proto.Equal`); `GetBalancerInfo("nope")` → `app/router: cannot find tag`. Вывод: `$R/xexp/live.out`.
- **Cron.** robfig v3.0.1 с `SecondOptional|…|Descriptor` совпадает с cron-parser 5.4.0 на всех 16 выражениях `cron-next`.
- **gtl.** Вектора `fill8` и sha256(`/load/1`) = `8ab78394…cb01` подтверждены Go-реализацией (`$R/gtlx`).
- **sfc/scd TS-эталоны на Node 24.** `sfc refresh` по docker-примеру: successful 3 (8/3/3 строк). `scd generate` по docker-фикстуре: parsed 8. Профильный заголовок `base64:QXZhaWxhYmxlIFByb2ZpbGU=` есть в `sfc/test/server.test.ts:79`.
- **CI/GitHub.**
  - Существуют теги `actions/setup-go@v7` (читает `toolchain`), `golangci/golangci-lint-action@v9`, `browser-actions/setup-chrome@v2`, `docker/{setup-buildx,build-push,login,metadata}-action@v4/v7/v4/v6`, `actions/checkout@v7`.
  - golangci-lint v2.15.0: релизный бинарь собран go1.27.0, на коде go1.27 даёт 0 issues.
  - actionlint@latest на текущем workflow отрабатывает.
  - Релиз Xray: `v26.10.10/Xray-linux-64.zip` скачивается, sha256 совпадает с `.dgst`.
- **Docker.**
  - `golang:1.27.2-alpine` → `gcr.io/distroless/static-debian13` собирается для amd64 и arm64 (без QEMU).
  - В образе: exec-form `CMD ["scd",…]` работает через PATH, CA-bundle на месте, uid 0, `-X main.version` доходит.
  - compose с тремя сервисами на одном `image:` собирает образ один раз и передаёт каждому свой `command:`.
  - Локальный тег `ghcr.io/xtls/xray-core:26.10.10` используется compose без pull.

## Эксперименты

| # | Что | Команда / место | Ключевой результат |
|---|---|---|---|
| E1 | Модуль в корне + TS-каталоги | `$R/repo` (копия репо + `go.mod`, `cmd/scd`) — `go build ./cmd/scd`, `go build -o bin/scd`, `go vet/test ./...` | `go: build output "scd" already exists and is a directory`; с `-o` OK |
| E2 | golangci-lint v2.15.0 | `GOTOOLCHAIN=go1.27.2 go install …@v2.15.0` → `$R/gobin`; `config verify`; `run`; релизный tar → `$R/glrel` | verify OK; revive/errcheck/noctx-замечания (M8); релиз собран go1.27.0 |
| E3 | govulncheck | `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` в `$R/getx` | без `GOTOOLCHAIN` — go1.26-ошибка; с ним — 403 на vuln.go.dev |
| E4 | Версии зависимостей | `go list -m -json <mod>@latest` | cobra v1.10.2, yaml v3.0.5, x/text v0.43.0, x/sync v0.24.0, x/net v0.61.0, grpc v1.84.0, protobuf v1.36.12, chromedp v0.21.0, actionlint v1.7.12; xray `@latest`=v1.260327.0 |
| E5 | `infra/conf` Build | `$R/xexp/build/main.go` → `$R/xexp/build.out` | см. «Подтверждено»; random ML-KEM проходит Build |
| E6 | Подавление логов Xray | `$R/xexp/silent`, `$R/xexp/bbuild` | warnings в stdout; handler перехватывает |
| E7 | Live Xray 26.10.10 | Образ `local/xray-core:26.10.10` из релизного zip (`$R/xrayrel`); `docker run --name xrev --network scdrev -v $R/xcfg:/etc/xray local/xray-core:26.10.10 run -confdir /etc/xray`; клиент `$R/xexp/live` собран в `golang:1.27.2-alpine`, запущен в `alpine:3.22` в той же сети | `$R/xexp/live.out` |
| E8 | REALITY spider (D13) | `$R/xexp/spider/main.go` против релизного бинаря | с SpiderY — жив; без — `panic … reality.go:298`, exit 2 |
| E9 | TS на Node 24 | `docker run node:24-alpine … npm ci/build/test` в копии `$R/ts24/repo` | 115/24/20 pass (`$R/ts24/node24-tests.txt`); refresh/generate выводы как в плане |
| E10 | ICU Node 22 vs 24 | `Intl.DisplayNames` на 45 ISO2 | идентично |
| E11 | cron robfig vs cron-parser | `$R/cron/next.mjs` (node:24), `$R/cron/main.go` → `$R/cron/compare.out` | 16/16 совпадений; расхождения см. M2 |
| E12 | net/url поведение | `$R/urlexp` | M3, m6, M9 |
| E13 | gtl payload | `$R/gtlx` | вектора совпали |
| E14 | bufconn/ошибки gRPC | `$R/xexp/bufc` | Unimplemented/Unavailable-тексты; m11 |
| E15 | synctest + gRPC | `$R/xexp/synct` (`go test -race` ×3) | PASS |
| E16 | Headless-скриншоты | `headless_shell --screenshot` в `$R/shot`; chromedp v0.21 в `$R/cdp` на хосте и в `chromedp/headless-shell` | M5, M6; full-page PNG получен |
| E17 | Dockerfile/compose | `$R/repo/Dockerfile`, `.dockerignore`, `compose.proto.yml`; `docker buildx build --load`, `--platform linux/arm64 -o type=cacheonly`, `docker compose up -d --build` | всё работает; образы и контейнеры удалены |
| E18 | GitHub actions/release | `git ls-remote --tags`, клоны `action.yml` в `$R/gh`, `curl` ассета Xray | теги есть; `chrome-path` output; ассет OK |

Контейнеры и сеть экспериментов удалены. Локальный тег `ghcr.io/xtls/xray-core:26.10.10` и прототипные образы `scd` тоже удалены. В Docker остались только базовые образы и `local/xray-core:26.10.10`.

## Нужен ответ владельца

1. **Расширение/сужение cron (D23).** robfig принимает `@midnight`/`@annually` (TS — нет) и отвергает `@minutely|@secondly|@weekdays|@weekends`, день недели `7`, `L/W/#/H` (TS принимал). План теперь фиксирует такое поведение и документирует его в README. Нужно подтвердить, что в реальных конфигах нет этих форм.
2. **Невалидные ML-KEM-ключи (новое, Review Focus 6).** План теперь пропускает такие ссылки как `unsupported_value` «Invalid VLESS encryption key.». Это развитие D19 («Xray rejected»), а не новое решение, но текст причины стоит утвердить.
3. **O1–O12** по-прежнему без ответа. Умолчания технически выполнимы: O2, O3, O4, O5, O11 проверены экспериментами; для O11 исправлена реализация (M3), а не умолчание.
4. **Свежесть D1 (не исправлялось).** Существуют `actions/checkout@v7` и `distroless/static-debian13`; план оставил `checkout@v5` и `debian12`. Обновить — решение владельца.

## Остаточные риски

- **govulncheck** в CI может стать красным из-за уязвимостей в транзитивных зависимостях xray-core (quic-go, gvisor и т.п.), которые проект не контролирует. Локально проверить нельзя (vuln.go.dev закрыт). Нужна политика: не блокировать или временно исключать с обоснованием.
- **Официальный образ `ghcr.io/xtls/xray-core:26.10.10`** локально не проверен: блобы недоступны, проверены только манифест (amd64/arm64) и релизный бинарь того же коммита. Его ENTRYPOINT и наличие `geoip.dat` подтвердит только CI-smoke.
- **Visual regression** зависит от сборки браузера: рендер на хосте и в контейнере отличается на байты. Сравнение TS-HTML и Go-HTML в одном браузере (как в плане) это нивелирует, но эталонные PNG этапа 0 — только справочные.
- **Фиксированные `container_name` в compose** мешают параллельному запуску smoke и локального стека. Скрипт обязан это проверять.
- **Observatory в docker-конфиге Xray** пробует `https://www.google.com/generate_204`. В CI-smoke это сетевой внешний вызов, но на результат smoke не влияет.
- **REALITY live-тест** опирается на SAN `example.com` в сертификате `httptest` (детали Go stdlib). Если формат сертификата изменится, тест надо будет поправить (ассерт на лог это сразу покажет).
