# Повторное ревью плана миграции на Go (ревью-2, 2026-10-17)

Независимое второе ревью плана (`README.md` + `stage-0…7`, 45 задач) **против кода** `scd/src`, `sfc/src`, `gtl/src`, тестов, Dockerfile'ов, compose, `docker/`, CI. Цели (бриф владельца): ничего не выдумано, ничего не потеряно, TS-нюансы не перетаскиваются (D9/D31), план согласован с D1–D33. Изменены только `docs/go-migration/plan/*.md`; 07 и документы 00–12 не трогались. Код, конфиги, CI не менялись, ничего не запущено. Эксперименты лежат в `/tmp/claude-0/-home-user-scd/b738d151-5a61-5b3f-9a86-51bb9c74ebc7/scratchpad/review2/` (дальше `$R2`). Находки ревью-1 (`REVIEW.md`) не откатывались: все его исправления на месте (`-o bin/scd`, B1 подавление логов, B2 ключи ML-KEM, M3 `strings.Cut` для `#`, M9 хук `Dial`, M10 smoke без перезаписи `config.yml` и т.д.).

## Вердикт

План в целом **верен коду**. Тексты RPC-ошибок, алгоритмы apply/repair/rejoin, события логов, схемы JSON, CLI, маршруты sfc/gtl, профильные заголовки и инвентари тестов сверены с исходниками:
- все 51 имя `event` и reason-кода из кода есть в 03/04/плане;
- 115/24/20 TS-тестов разнесены по задачам;
- монитор — 25 тестов, 1:1.

Найдено **3 blocker**, **4 major**, **15 minor** (итого 22). Все исправлены в файлах плана, новых задач не понадобилось (по-прежнему 45). Вопросов к владельцу нет: всё решено в рамках D1–D33. Противоречий решений с кодом не найдено.

Главное:
- **gtl/sfc и хвостовой `/` (F01).** Нормализация пути через голый `path.Clean` ломала маршруты: `/load/` (TS 400 → Go 404), `/load/1/` (TS 400 → Go 200), оба кейса есть в эталоне `routes.json`. Добавлен `httpserve.CleanPath`.
- **Задача 0.6 (F07).** Импортировала неэкспортируемую функцию scd и упала бы на импорте.
- **Фильтры D29 (F03, F04).** Задача 1.3 требовала проверку конфигов, которых ещё нет. Тест эквивалентности был размножен по трём задачам. Структуры конфигов 3.1/4.1 противоречили D29: строка вместо списков include/exclude.
- **D32 (F05, F06).** Правило «дописать CONFIG-MIGRATION.md в том же коммите» отсутствовало в шагах 7 задач, включая весь gtl. У `TestConfigMigrationDocCoversKeys` не было источника TS-ключей.
- **runtime-state `expected` (F16).** Алгоритм не был закреплён: какой `allowInsecure`-policy, текст для remote/stdin, Go-правило редактирования URL.

## Находки

Severity: **blocker** — задача/проверка гарантированно падает; **major** — неверная/неполная инструкция, ведущая к потере требования или неверному коду; **minor** — уточнение. Категории: A — выдумано/неверно о коде, B — потеряна функциональность, C — перетащен TS-нюанс, D — согласованность/порядок/D-решения.

| id | sev | cat | Где | Проблема | Доказательство | Исправление |
|---|---|---|---|---|---|---|
| F01 | blocker | A/D | 2.2 `server.New`; 3 «Контекст» (путь sfc) | «`EscapedPath()` + `path.Clean`» срезает хвостовой `/`: `/load/` → `/load` (404 вместо 400), `/load/1/` → `/load/1` (200 вместо 400). Оба пути есть в `routes.json` (0.3), поэтому `TestRoutesReference` красный. Для sfc `/s/test-all/` давал бы 200 вместо 404 | `gtl/src/runtime/server.ts:30-38,112-119` (`new URL().pathname` хвост сохраняет); `node -e … buildResponse('/load/')` → 400, `'/load/1/'` → 400; `$R2/pathclean/main.go`: `path.Clean` → `/load`, `/load/1`; с сохранением `/` → `/load/`, `/load/1/` | 1.5: `httpserve.CleanPath` (dot-сегменты без декодирования, хвостовой `/` сохраняется) + `TestCleanPathKeepsTrailingSlash`. 2.2 и 3 «Контекст» используют его. В 2.2 явные кейсы `/load/` и `/load/1/` → 400 |
| F02 | minor | D | 1.3 «Поправка D29» | `labelfilter` без Files, без точной сигнатуры (`Parse(node)` — какого типа?), без правил issues и `print-config` | — | 1.3: файлы, `Parse(*schema.Node) Filter`, `Match`, `Empty`, `MarshalJSON`, пути issues |
| F03 | blocker | D | 1.3, 3.1, 4.1 «Поправки D29/D30» | (а) В 1.3 стоял тест «Go-форма `config.real-shape.go.yml` проходит `validate-config`». Конфигов и команд в 1.3 ещё нет (они появляются в 2.1/3.1/4.1), то есть задача использует результат более поздних. (б) `TestLabelFilterRealShapeEquivalence` был предписан трижды (1.3, 3.1, 4.1) | Тексты поправок; граф задач README | Эквивалентность и `allowedDiffs` — только в 1.3 (пакет `labelfilter`). Проверка Go-форм — `TestGTLRealShapeGoFormValid` (2.1), `TestSFCRealShapeGoFormValid` (3.1), `TestSCDRealShapeGoFormValid` (4.1) |
| F04 | major | A/D | 3.1 `Output`, 4.1 `SubscriptionFiltersConfig` | Типы — `LabelIncludeRegex string` (+ один `*regexp.Regexp`), правило 3.1 — «`labelIncludeRegex` обязателен». Это противоречит D29 (include/exclude — строка **или список**, в sfc обязателен хотя бы один) и поправкам в тех же задачах | `07` D29; текст 3.1/4.1 | Типы на `labelfilter.Filter`; `print-config` печатает списки; в `TestPrintConfigDockerExample` (3.1) строка эталона оборачивается в список; `TestOutputRequiresSomeFilter` |
| F05 | major | D | 1.2, 1.4, 1.5, 2.1, 4.3, 6.6, 7.1 | Правило D32 («дописать `docs/CONFIG-MIGRATION.md` в том же коммите») было только в README; в шагах этих задач его не было. Весь gtl (2.x) не создавал ни раздела, ни `docker/gtl/config.real-shape.go.yml`, хотя D32 требует разобранный пример для `docker/{scd,sfc,gtl}`. Файл `docker/gtl/config.real-shape.yml` существует | `07` D32; `git ls-files docker` → `docker/gtl/config.real-shape.yml`; `grep -n CONFIG-MIGRATION stage-*.md` до правки: только 1.1, 3.1, 3.2, 4.1, 5.2, 7.3 | Пункты D32 вписаны в Files/Commit каждой задачи. В 2.1 добавлены `config.real-shape.go.yml`, раздел gtl (stderr-логи, `--version`, listen) и `TestGTLRealShapeGoFormValid`. README: строка D32 и правило синхронизированы |
| F06 | major | D/B | 7.3 `TestConfigMigrationDocCoversKeys` | Тест сверяет «ключи Go-схем vs TS-схем из `testdata/reference`», но ни одна задача этапа 0 не снимала полный набор TS-ключей. `print-config` эталонных конфигов не содержит необязательных ключей (`filters`, `remotePing.url`, `inboundSocks`, …). Схемы zod не экспортируются. Способ получения Go-ключей не задан | `scd/dist/config/load-config.js` экспортирует только `loadConfig` (проверено `node -e import(...)`); 0.2/0.3 Files | 0.2/0.3: `testdata/reference/<svc>/config-full.yml` (каждый ключ) → `config-keys.json`. 7.3: тест в `internal/cli/configdoc_test.go`, Go-ключи — `reflect` по json-тегам типов конфигов; плюс шаги `validate-config` всех Go-форм real-shape |
| F07 | blocker | A | 0.6 Step 2 | «через тот же `parseRegexLiteral`, что TS (`scd/src/config/load-config.ts`)». В scd функция локальная (`function parseRegexLiteral`, не `export`), из `scd/dist` не импортируется, поэтому скрипт падает | `scd/src/config/load-config.ts:30` (без `export`), `sfc/src/config/load-config.ts:22` (`export`); `node --input-type=module -e "import {parseRegexLiteral} from './sfc/dist/config/load-config.js' …"` → работает; `import('./scd/dist/config/load-config.js')` → `[ 'loadConfig' ]` | Брать экспортируемую копию из `sfc/dist` (та же семантика `new RegExp(pattern, flags)`, как и `scd/src/runtime/filter-manifest.ts`) |
| F08 | minor | D | README «Раскладка», «Глобальные ограничения» | В раскладке `internal/common` нет `cliutil/` (создаётся в 1.1) и `labelfilter/`. «Трекаются только `config.example.yml`, `fixtures/*`, `docker/README.md`» неверно: трекаются ещё `config.real-shape.yml`, `fixtures/real-shapes/*`, `docker/xray/config/*.json` | `git ls-files docker` | Исправлено |
| F09 | minor | D | README трассировка D30/D32 | D30 не упоминал 2.1/7.3, D32 не перечислял 1.2/1.4/1.5/2.1/4.3/7.1 | — | Исправлено |
| F10 | minor | B | 3.1 `TestPrintConfigDockerExample` | После F04 Go печатает список, а TS-эталон 0.3 — строку: тест упал бы | `testdata/reference/sfc/print-config-docker-example.json` (форма TS: `labelIncludeRegex: string`) | Правило сравнения: строка эталона → список |
| F11 | minor | B | 4.7 `ApplyFilters`, `Summary` | Не определено, куда считается отбрасывание exclude-фильтром (D29). Не сказано, что `unsupportedParam` остаётся в JSON, но всегда 0 после D28 | `scd/src/runtime/filter-manifest.ts:60-80` (`filteredByLabelRegex`, `parsed = entries.length`); `scd/src/manifest.ts` (13 счётчиков) | Include-провал и exclude-совпадение → `filteredByLabelRegex`; `unsupportedParam` = 0. Тест `TestExcludeCountsAsLabelFiltered`. Проверено: provider2 + allowlist 24 кода → 25 entries, `filteredByCountry=25`, Extra/WhiteList среди них 0 (`python3 -I`, см. E4) — утверждение 4.7 верно |
| F12 | minor | B | 3.2, 4.7 | В `inputsrc.Options` явно передавались только `UserAgent`/`Headers`; `format` и `fetchTimeoutMs` подписки (TS: `loadInputSource(input, {format, fetchTimeoutMs})`) не упоминались | `sfc/src/runtime/refresh.ts:183-186`; `scd/src/runtime/generate-manifest-from-source.ts:71-74` | `Options{Format, FetchTimeout, …}` + `TestLoadSubscriptionsPassesFormatAndTimeout` |
| F13 | minor | B | 3.2 | Не сказано: (а) TS-sfc имел **свой** парсер со строгой проверкой UUID v1–5, а общий `vless.Parse` (D18) пропустит `my-user` — видимое изменение выхода; (б) строки со `Skip` молча отбрасываются | `sfc/src/subscription/parse-subscription-line.ts:20-22,38-41`, `sfc/src/runtime/refresh.ts:66-75` | (а) в `TestRefreshUsesSharedParamWhitelist` + CONFIG-MIGRATION «Подписки»; (б) правило «молча» |
| F14 | minor | B | 5.2 `DescribeOutbound` | Форматы значений JSON-контракта не заданы: `publicKey` — std-base64 с паддингом, `shortId` — hex, `alpn` всегда (даже `[]`), пустые строки → ключ отсутствует | `scd/src/api/protobuf.ts:333-380` | Описано + `TestDescribeRealityKeyFormats` |
| F15 | minor | B | 6.3 Remote Push | Логи remote ping пишут только `remoteHost`, а не URL (в query обычно токен) — в плане не было | `scd/src/runtime/monitoring.ts:100-106,647-650` | Правило + `TestRemotePingLogsOnlyHost` |
| F16 | major | B | 6.4 runtime-state | Алгоритм `expected` не закреплён в задаче. Нет: `allowInsecure`-policy подписки (без неё ws-туннели с `allowInsecure=1` при `allow` пропали бы из `expected` → ложный `unexpected`), дословный текст для remote/stdin, «файл перечитывается на запрос», обнуление при выключенных resources. Go-правило вместо WHATWG `origin`/`null/<redacted>`. Заголовок `Accept: application/json` и таймаут на тело у remote collector | `scd/src/runtime/current-runtime-state.ts:213-252,85-148`; `scd/src/runtime/status-aggregation.ts:115-118` | 6.4 дополнена; 4.7 — `manifest.PolicyFor` (используют sync и runtime-state); тесты `TestRuntimeStateExpectedUsesAllowInsecurePolicy`, `TestRedactURLGoRules`, `TestFromSubscriptionUsesAllowInsecurePolicy` |
| F17 | minor | D | 6.6 | Включение `balancerSelection` в `docker/scd/config.example.yml` ломает `TestPrintConfigDockerExample` из 4.1 (там ожидается дефолт) | 4.1 Step 2; 6.6 Files | В 6.6 добавлена правка теста |
| F18 | minor | B | 6.2 | Пропущен текст `Unexpected EOF while reading <n> bytes from SOCKS proxy.` (попадает в `lastError` дашборда) | `scd/src/runtime/socks-http.ts:76` | Добавлен |
| F19 | minor | B | 7.3 | (а) `.gitignore`: первая строка `vpn` (локальная подписка владельца, `generate --input ../vpn`) не была в списке «оставить» — её стёрли бы. (б) `gtl/AGENTS.md` утверждает «payload — ASCII `a`», хотя в коде xorshift32 — перенос в новый AGENTS.md дал бы ложь | `.gitignore:1`; `scd/AGENTS.md` (CLI Surface); `gtl/AGENTS.md` «Runtime Model» vs `gtl/src/runtime/server.ts:96-106` | Оба пункта уточнены |
| F20 | minor | C | 6.3 `RemotePingURL` | Требование «`~` → `%7E`» — побайтная эмуляция сериализатора WHATWG `URLSearchParams` | D9 («WHATWG-нюансы URL не повторять»); `scd/src/runtime/monitoring.ts:89-98` | Оставлено функциональное (порядок, `set` на месте, `ping` в конце). Кодирование — `url.QueryEscape`, тест сравнивает разобранный query |
| F21 | minor | C | 2.1, 2.2 | Граница `maxSizeKb ≤ 8796093022207` (= `floor(MAX_SAFE_INTEGER/1024)`) и «длинные — по длине > 16» — JS-эвристики safe-integer | `gtl/src/config/load-config.ts:55-60`, `gtl/src/runtime/server.ts:41-50` | Go-native: `≤ math.MaxInt64/1024`, `strconv.ParseUint` + сравнение |
| F22 | minor | C | 1.3 `schema.Int` | «YAML-float с целым значением допустим» (`5000.0`) — JS-семантика `number` | D9/D31 | Только `!!int`, `TestFloatRejected` |

Итого: blocker 3 (F01, F03, F07), major 4 (F04, F05, F06, F16), minor 15. По категориям (смешанные — по первой букве): A — 3 (F01, F04, F07), B — 9 (F10–F16, F18, F19), C — 3 (F20–F22), D — 7 (F02, F03, F05, F06, F08, F09, F17).

## Что подтверждено без изменений (выборочно, с доказательством)

- **Тексты API-слоя Xray.**
  - `Unable to connect to Xray API at <host>. Start the Xray service first, for example with "docker compose up -d".`
  - `Request to Xray API at <host> timed out after <N>ms.`
  - `gRPC call failed with status <n>`

  Источник: `scd/src/api/xray-handler-client.ts:48-80`. Совпадает с 5.1.
- **Apply.**
  - outbound: pre-check fixed без RPC; удаление всех не-fixed с backup; rollback в обратном порядке, затем re-add backup в порядке удаления; id failed-item `current || last added || entries[0] || unknown` — `scd/src/apply/apply-outbounds.ts`.
  - inbound/routing: без rollback, `replaced` по `existingTags`, failed-item = первый тег — `scd/src/apply/apply-inbounds.ts:67-111`, `apply-routing.ts:64-110`.

  Всё совпадает с 5.3/5.4.
- **Хэши.** outbound — `sha256(JSON(entries[].jsonOutbound))` после сортировки tag/line/raw; inbound — `[{inboundTag, listen, port}]` (`scd/src/apply/outbound-applicator.ts`, `inbound-applicator.ts`). Совпадает с 5.4.
- **Repair.** `removeRule/removeOutbound` (ошибки проглатываются) → `addOutbound(unprefixed)` → `addRule`.
- **Rejoin.** `addOutbound(initial)` → `removeRule` → `addRule` → `removeOutbound`, откат при ошибке. Тексты `Tunnel repaired; awaiting next successful monitor check.`, `Tunnel healthy but balancer rejoin failed: …`.

  Источник: `scd/src/runtime/monitoring.ts:260-427`. Совпадает с 6.3.
- **Sync.** `skipped_no_changes`, `resource_apply_started`, `apply_finished|apply_failed` с `errorMessages`/`failedItems`, `manifest_validation_failed`, `subscription_fetch_failed`, overlap → пустой отчёт (`scd/src/runtime/sync-once.ts`). Совпадает с 6.1 через 03 §2.
- **Daemon.** Циклы sync, monitor/balancer (включая disabled-подписки) и remote instances, события и тексты (`scd/src/runtime/run-daemon.ts:142-244`). Совпадает с 6.7.
- **sfc.**
  - `/healthz` → `ok\n`, 502 `output is not available yet\n`, 404 `not found\n`, метод не проверяется, `profile-title: base64:…`/`profile-update-interval` по префиксу UA (`sfc/src/runtime/server.ts`).
  - Кэш `.sfc-cache/<encodeURIComponent(id)>.subscription` (`sfc/src/runtime/source-cache.ts`).
  - Отчёт refresh, `usedCachedValue`, `usedSourceCache` (`sfc/src/runtime/refresh.ts`).
  - Заголовков `subscription-userinfo` / `content-disposition` в коде **нет** (`grep` по `sfc/src`, `scd/src`, `docker/` пуст) — план их правильно не вводит.
- **gtl.** Маршруты, `x-gtl-*`, 405 на не-GET, xorshift32 с пересевом каждые 64 KiB (`gtl/src/runtime/server.ts`). Совпадает с 2.2.
- **Инвентари тестов.** `grep -cE '^\s*(test|it)\('`: scd 115 (monitoring 25, parser 25, service 26, status-server 12, runtime-state 8, socks-http 7, topology 5, run-daemon 4, status-aggregation 3), sfc 24, gtl 20 — совпадает с 0.1. 25 тестов мониторинга сопоставлены 1:1 с именами в 6.3.
- **Каталог событий.** 51 уникальное имя `event`/reason-кода из `scd|sfc|gtl/src` найдено в 03/04/плане. События gtl — это `msg` pino (`gtl_server_*`), план их корректно делает `event`+`msg`.

## Функциональный инвентарь (из кода) → задачи

Обозначения: «=» — переносится как есть; «изм.» — меняется по указанному решению.

### gtl (`gtl/src`)

| # | Элемент (код) | Задача | Примечание |
|---|---|---|---|
| G1 | CLI `serve`, `validate-config` (`Config is valid\n`), `print-config` (JSON), `--config` обязателен (`cli.ts`) | 2.1, 2.3 | =; `scd gtl …` (D10) |
| G2 | `gtl --version` → `1.0.2` | — | drop: D27 O10 |
| G3 | Конфиг: `server.listen` (strict), `logging.{level,format}` default info/json, `load.path` (4 правила, накапливаются), `load.maxSizeKb` default 10240; все объекты strict; без env (`config/load-config.ts`) | 2.1 | изм.: граница F21, listen `:port` (1.2) |
| G4 | `/healthz` → 200 `ok`, `content-length: 2` | 2.2 | = |
| G5 | `<load.path>/<sizeKb>` → 200 octet-stream + `content-length` + `x-gtl-status-code/requested-size-kb/requested-size-bytes/actual-size-bytes` | 2.2 | = |
| G6 | 400 `invalid load route…`, 400 `sizeKb must be…`, 404 `not found\n`, 405 `method not allowed\n` (вкл. HEAD) | 2.2 | = |
| G7 | Нормализация пути `new URL().pathname` (dot-сегменты, query отброшен, хвостовой `/` сохранён) | 1.5 `CleanPath`, 2.2 | исправлено F01 |
| G8 | Payload xorshift32, seed `0x9e3779b9 ^ offset`, пересев на чанк 64 KiB, стриминг с backpressure | 2.2 | = (golden SHA-256) |
| G9 | `requested size is too large` | — | drop: недостижимо (max ≤ MAX_SAFE/1024), D9 |
| G10 | Логи `gtl_server_started{listen}`, `gtl_server_stopping{signal}`, `gtl_server_stopped`; pino в **stdout** | 2.3, 1.5 | изм.: stderr + slog (D27 O10) — CONFIG-MIGRATION (F05) |
| G11 | SIGINT/SIGTERM → `server.close()` (ждёт активные загрузки) | 2.3 | изм.: D7 (Shutdown с таймаутом, загрузка дописывается) |
| G12 | Docker: `node:24-alpine`, `WORKDIR /app`, `CMD serve --config /app/config/config.yml`, порт 8080 | 7.1 | изм.: один образ, `command:` (D10) |
| G13 | `docker/gtl/config.real-shape.yml` | 2.1 | Go-форма + раздел D32 (F05) |

### sfc (`sfc/src`)

| # | Элемент | Задача | Примечание |
|---|---|---|---|
| S1 | CLI `serve`, `refresh` (JSON отчёт, exit 1 при failed>0), `validate-config` (`{configPath, valid}`), `print-config` | 3.1, 3.4 | = |
| S2 | Конфиг: `subscriptions[].{id,input,enabled,format,fetchTimeoutMs,pathRoute,outputs[]}`, `outputs[].{id,enabled,name,labelIncludeRegex,userAgent[],profileTitle,profileUpdateInterval 1..24}`, `runtime.refreshSchedule`, `server.listen`, `logging`; non-strict; trim; пустые optional → отсутствуют | 3.1 | изм.: D29 (F04), D33 `fetchHeaders`, O9 `fetchUserAgent` |
| S3 | Кросс-правила: ≥1 enabled подписка, ≥1 enabled output, уникальность id подписок и output'ов (глобально), `pathRoute` (3 правила), cron, listen | 3.1 | = (тексты Go, D9) |
| S4 | Env `${NAME}` после YAML, только строки, отсутствующая → `""` | 1.3, 3.1 | = |
| S5 | `input` относительный → от каталога конфига; `-`, http(s), абсолютный — как есть | 1.3 | = |
| S6 | Загрузка: stdin/http(s)/файл, `format auto|plain|base64`, таймаут, пустой контент → ошибка, редиректы | 1.6, 3.2 | изм.: stdlib base64 (D9), UA (O9), заголовки (D33); F12 |
| S7 | Собственный парсер sfc: строгий UUID, 13 параметров, невалидные строки молча отбрасываются | 1.7, 3.2 | изм.: общий `vless.Parse` (D27 O7, D18, D28) — F13 |
| S8 | Фильтр по label регуляркой `/p/f`, порядок строк сохранён, base64 от `join("\n")` | 1.3, 3.2 | изм.: RE2 + include/exclude (D8, D29) |
| S9 | Кэш источника `.sfc-cache/<encodeURIComponent(id)>.subscription`, атомарная запись (temp + rename, удаление temp при ошибке), запись до разбора, fallback при ошибке загрузки | 3.2 | = (`escapeID`) |
| S10 | Отказы: нет кэша → failure всех enabled outputs с сохранением lastGood; пустой фильтр → `Filtered output is empty.`; `usedCachedValue`, `usedSourceCache` | 3.2 | = |
| S11 | Состояние outputs: все (включая disabled) зарегистрированы; `outputsByPath[pathRoute/encodeURIComponent(id)]`; атомарная замена | 3.2 | = |
| S12 | Отчёт `{refreshedAt, sourceCount, outputCount, successful, failed, outputs[]}`, сортировка по id | 3.2 | изм.: `strings.Compare` (D23) |
| S13 | HTTP: `/healthz` → `ok\n`; 200 base64; 502 `output is not available yet\n`; 404; метод не проверяется; `text/plain; charset=utf-8` | 3.3 | = |
| S14 | Профильные заголовки `profile-title: base64:<b64>` / `profile-update-interval` по префиксу UA (lowercase) | 3.3 | = |
| S15 | Логи `config_loaded`, `server_started`, `refresh_tick`, `refresh_tick_skipped_overrun`, `refresh_failed` (+`causes`), `subscription_source_cache_written|write_failed|used|missed`, `subscription_refreshed`, `subscription_refresh_failed` | 1.4, 3.2, 3.4 | = + новый `subscription_line_param_ignored` (D28) |
| S16 | serve: начальный refresh → listen → cron (первый тик сразу = двойной refresh) | 3.4 | изм.: O12 |
| S17 | Остановка: ждать слот cron | 1.4, 3.4 | изм.: D7 |
| S18 | Docker: `serve`, volume `.sfc-cache` у конфига | 7.1 | = (путь тот же) |
| S19 | `docker/sfc/config.real-shape.yml` (lookahead) | 3.1 | Go-форма D29 |

### scd (`scd/src`)

| # | Элемент | Задача | Примечание |
|---|---|---|---|
| C1 | CLI `generate --input --output --log` (summary в stdout, файлы только при успехе), `sync` (exit 1 при failed>0), `daemon`, `validate-config`, `print-config`; ошибка → stderr, exit 1 | 1.1, 4.1, 4.7, 6.1, 6.7 | = |
| C2 | Конфиг: все ключи `scd/src/types.ts:196-331` (subscriptions/filters/target/inboundSocks/monitor/retry/request/balancerMonitor/socks5/remotePing/observatorySubjectSelectorPrefix/visionUdp443Override/fixed*; runtime; logging; resources; statusServer/instanceId/remoteInstances/runtimeState) | 4.1 | = + D15, D22, D27, D29, D33 |
| C3 | Strict/strip-объекты, дефолты, trim, `countryAllowlist` upper + 45 кодов, кросс-правила 01 §2.3 | 4.1 | изм.: все issues без раннего return |
| C4 | Env `${NAME}` по сырому тексту до YAML, отсутствующая → ошибка | 1.3 | = |
| C5 | Отказ legacy-форм (`targets[]`, `speedtest`, `successGet`) | 4.1 | изм.: обычное `unknown key` (D31) |
| C6 | Загрузка подписок: только enabled, сорт id/input, последовательно, failed не роняет | 4.7 | = (F12) |
| C7 | Разбор строки: коды `unsupported_scheme`, `invalid_uri`, `missing_required_field`, `unsupported_param`, тексты | 1.7 | изм.: D18, D28, O11 |
| C8 | Нормализация: 3 профиля, combo/encryption/ws path/REALITY params/allowInsecure/headerType | 4.3–4.5 | изм.: D26 (10 профилей), D15, D16, O15 |
| C9 | Тег `vless-<cc>-<city>-<profile>-<sha8(raw)>`, slug NFKD/32, карта 45 флагов (`nameEn` ICU, `nameRu`) | 4.2 | = (теги старых профилей не меняются, D33) |
| C10 | JSON outbound + protobuf | 4.6 | изм.: D2/D6/D19 (infra/conf, корректные поля) |
| C11 | Манифест: summary 13 счётчиков, `duplicate_tag`, фильтры (country → regex; `parsed` переписывается), `produced no valid outbound entries …` | 4.7 | изм.: D29 (F11), skip «Xray rejected» (D26) |
| C12 | Override `visionUdp443Override` (только reality+vision), prefix `observatorySubjectSelectorPrefix` только у outbound | 4.6, 4.8 | = |
| C13 | Топология: сортировка tag/line/raw, порты `start+i`, `in-`/`route-`, ошибка ёмкости | 4.8 | изм.: ошибка валит только target (D23) |
| C14 | Xray RPC: List/Add/Remove Outbound/Inbound, ListRule/AddRule(append)/RemoveRule; таймаут на вызов; маппинг ошибок | 5.1 | = (D2) + read-only D22 (5.5) |
| C15 | Apply outbound/inbound/routing, fixed, rollback outbound, отчёты `ApplyReport` | 5.3, 5.4 | = |
| C16 | Планы и хэши по kind, `managedIds`, применение всех kinds при изменении любого, хэши/topology только при полном успехе, сброс monitor-state | 5.4, 6.1 | = (managedCount/skipped без дублей — D9) |
| C17 | In-memory state, per-target mutex, overlap-guard sync | 6.1 | = |
| C18 | Sync-события (`sync_started`, `subscription_fetched`, `subscription_fetch_failed`, `manifest_built`, `manifest_validation_failed`, `skipped_no_changes`, `resource_apply_started`, `apply_finished|failed`, `sync_skipped_overlap`, `sync_failed`, `config_loaded`) | 6.1 | = + `manifest_entry_warning` |
| C19 | Монитор туннелей: `maxParallel`, retry в слоте (`monitor_check_retry` debug), состояния, repair/rejoin под lock, `tunnel_repaired|repair_failed|rejoined_balancer|rejoin_failed` | 6.3 | = |
| C20 | Balancer monitor через внешний SOCKS5, retry, состояния | 6.3 | = |
| C21 | Remote Push: async, overlap-guard `pending`, `viaSocks`, прямой GET без чтения тела, 2xx, `buildRemotePingUrl`, логи с `remoteHost` | 6.3 | = (F15, F20) |
| C22 | SOCKS5+HTTP проба: протокол, ATYP, TLS SNI, `Connection: close`, POST `Content-Length: 0`, latency от записи запроса, тексты ошибок | 6.2 | изм.: общий дедлайн (D23); F18 |
| C23 | `resolveProxyHost` 0.0.0.0/:: → host target | 6.3 | изм.: IPv6-корректно (D9) |
| C24 | Status server: `/api/status`, `/api/instance-status`, `/api/instances`, `/api/runtime-state` (404/400/404/502/500/200), всё остальное → HTML; пересборка снимка на запрос; `instanceId ?? hostname` | 6.4 | изм.: query игнорируется (O8), panic → 500 (D23) |
| C25 | JSON-схемы `StatusSnapshotTunnel`, `InstanceStatusSnapshot` v1, `GroupedStatus*`, `AggregatedStatusSnapshot` | 6.4 | = (D3) |
| C26 | Группировка: сорт подписок/target'ов, туннели по latency (idle/нет → в конец), configured targets без туннелей, disabled не показываются | 6.4 | изм.: `strings.Compare` (D23) |
| C27 | Runtime-state: поиск, `expected` из локального файла, классификации, diff, `parsed`, `rawBase64` (`includeRaw`), редактирование input/monitor URL, `serviceState.balancerMonitor` | 5.2, 6.4 | = + D33 (path/xhttp host/fetchHeaders); F14, F16 |
| C28 | Remote instances: placeholder, collector (Accept json, таймаут, non-2xx текст, сохранение последнего snapshot), `parseRemoteSnapshot` (неглубокая валидация, passthrough), события `remote_status_*` | 6.4, 6.7 | = (F16) |
| C29 | Дашборд: вкладки инстансов (ARIA, клавиатура), subscription/target/balancer-first, карточки, хелперы, «No snapshot/tunnel data» | 6.5 | изм.: html/template, визуально = (D3) |
| C30 | Daemon: проверка `mode`, `config_loaded`, status server, циклы sync/monitor/balancer (включая disabled)/remote, события `daemon_tick*`, `monitor_tick_*`, `balancer_monitor_tick_*`, `remote_status_tick_*`, `status_server_started` | 6.7 | изм.: D7 |
| C31 | Cron-цикл: первый тик сразу, без дрейфа, overrun → пропуск, `causes` | 1.4 | = (без бага таймера >24.8 дня, D7) |
| C32 | Логи: pino JSON/pretty в stderr, уровни fatal…silent | 1.5 | изм.: slog (D9/D27) |
| C33 | Docker: `CMD daemon --config /app/config/config.yml`, root, `/app` | 7.1 | = (distroless, D10) |
| C34 | Balancer selection (новое, D22) | 4.1, 5.5, 5.7, 6.6 | новое |
| C35 | `docker/scd/config.real-shape.yml` | 4.1 | Go-форма D29/D30 |

### Общее: репозиторий, Docker, CI

| # | Элемент | Задача |
|---|---|---|
| R1 | `docker-compose.yml`: xray + три сервиса, порты 9090/9091/9092, volumes (config ro, fixtures ro, `.sfc-cache`), `container_name` | 7.1 |
| R2 | `docker/xray/config/*.json` (API, observatory, балансер `x-balancer`) | 6.6 (`ObservatoryService`), 7.1 |
| R3 | CI: verify на PR/master, publish по тегам `v*.*.*`, amd64+arm64, метки OCI | 1.1, 5.6, 6.5, 7.2 |
| R4 | `AGENTS.md` (корень + сервисные), сервисные README (`Regex examples` с lookahead) | 7.3 (F19) |
| R5 | `.gitignore` (`vpn`, `**/config.yml`, `**/.sfc-cache/`) | 7.3 (F19) |

Непокрытых пунктов после правок нет.

## Сознательные отказы (с D-ссылкой)

| Что убирается/меняется | Решение |
|---|---|
| `gtl --version 1.0.2`; `--version` у `scd sfc`/`scd gtl` | D27 O10 |
| Тексты zod, формат pino (`level` числом, `time` в мс), `User-Agent: node`, lenient base64, ICU-коллация, WHATWG `origin`/`null/<redacted>`, `URLSearchParams` `~`→`%7E`, JS-границы safe-integer, «float с целым значением» | D9 (F20–F22) |
| `unsupported_param` и строгий белый список параметров; строгий UUID (scd и sfc) | D28, D18 |
| Отказы TS «WebSocket VLESS requires a path.», REALITY без `sni/fp/sid`, «Only headerType=none is supported in v1.» | D26, O15 |
| Передача `allowInsecure` в Xray | D4, D15 |
| Неверные номера полей ws/TLS fingerprint в ручном protobuf; ручной HTTP/2-клиент | D2, D5, D6, D19 |
| Двойной refresh sfc при старте | O12 |
| Ожидание cron-слота после SIGTERM; переполнение таймера >24.8 дня; обрыв загрузки gtl | D7 |
| Падение всего sync при переполнении `portRange` | D23 |
| Дубли `skipped` по числу applicators, `managedCount=0` у inbound/routing | D9 (03 §0 п.6–7) |
| Точное сравнение URL с query на status server | O8 |
| Собственное HTML-экранирование и побайтный HTML | D3 |
| Подсказки для legacy-форм конфига (`targets[]`, `speedtest`, `successGet`) | D31 |
| Образы `sfc`/`gtl`, Node/npm в CI и дереве | D10, D20 |
| Падение подписки sfc из-за `#100%` (URIError → fallback на кэш) | O11 |
| Логи gtl в stdout | D27 O10 |

## Вопросы владельцу

Нет. Все технические развилки этого ревью решены в рамках D9/D18/D26–D33 (см. «Исправление» в таблице находок). Решения 07 с кодом не конфликтуют.

## Эксперименты

| # | Что | Команда / место | Результат |
|---|---|---|---|
| E1 | `path.Clean` vs сохранение хвостового `/` | `$R2/pathclean/main.go`, `go run` (Go 1.24.7) | `/load/` → `/load` vs `/load/`; `/load/1/` → `/load/1` vs `/load/1/`; `/a/../load/1` → `/load/1` в обоих вариантах |
| E2 | TS-поведение gtl на хвостовом `/` | `node --input-type=module -e "import {buildResponse} from './gtl/dist/runtime/server.js' …"` | `/load/` → 400, `/load/1/` → 400; `new URL('/load/1/').pathname` = `/load/1/` |
| E3 | Экспорт `parseRegexLiteral` и схем | `node … import('./scd/dist/config/load-config.js')` → `['loadConfig']`; из `sfc/dist` импорт работает (`DE-1-X` → true, `DE Extra` → false) | F06, F07 |
| E4 | Утверждение 4.7 «provider2 + allowlist → 25 entries» | `python3 -I` по `docker/fixtures/real-shapes/provider2.txt` и `supportedRegions` (45) | 50 меток, 25 в allowlist, 0 из них с Extra/WhiteList — утверждение верно |
| E5 | Число TS-тестов и событий | `grep -cE '^\s*(test|it)\('`; извлечение `event:`/кодов из `*/src` → `scratchpad/events.txt` (51) | 115/24/20; все имена есть в 03/04/плане |
| E6 | Leak guard | `scratchpad/realsubs/guard/check.sh` | `LEAK-GUARD: clean` (до и после правок) |
