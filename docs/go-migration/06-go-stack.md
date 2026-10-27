# 06. Go-стек для переписывания scd / sfc / gtl в один бинарь

Статус: исследование (research only). Дата проверки версий: 2026-10-17.
Версии модулей проверены через `go list -m -versions` (proxy.golang.org), требования к Go — по `go.mod` соответствующих релизов на proxy.golang.org. Поведение `cron-parser@5.4.0`, `zod@4.1.5`, `pino@10`, `pino-pretty@13`, `yaml@2` проверено запуском в Node (scratch-директория, не в репозитории).

---

## 0. Что именно используется в TS-коде (то, что надо воспроизвести)

| Библиотека | Где | Как используется |
|---|---|---|
| `commander@14` | `*/src/cli.ts` | Подкоманды с одной обязательной опцией: `scd generate --input --output --log`, `scd sync|daemon|validate-config|print-config --config`, `sfc serve|refresh|validate-config|print-config --config`, `gtl serve|validate-config|print-config --config`. `showHelpAfterError()` в scd/sfc; в gtl — `.version("1.0.2")` (флаг `-V, --version`). Ошибки действия: scd/sfc печатают `error.message` в stderr, exit code 1; gtl — `formatCliError` (для `ConfigError` только message, иначе `stack`). |
| `cron-parser@5.4.0` | `scd/src/config/load-config.ts`, `scd/src/runtime/run-daemon.ts`, `sfc/src/config/load-config.ts`, `sfc/src/runtime/run-server.ts` | Только `CronExpressionParser.parse(expr)` (валидация) и `CronExpressionParser.parse(expr, { currentDate }).next().toDate()`. Опции `tz`, `strict`, `hashSeed` **не используются** → часовой пояс = локальный пояс процесса (в `node:24-alpine` без `TZ` это UTC). Текст ошибки парсера попадает пользователю как сообщение валидации конфига. |
| `runCronLoop` | scd и sfc (код побайтно идентичен) | Первый запуск задачи сразу; далее `scheduledAt = next(schedule, scheduledAt)` (от предыдущего *запланированного* времени, а не от `now`), `delay = max(0, scheduledAt - now)`; если предыдущий тик ещё идёт — `onOverrun(scheduledAt)` и пропуск; при остановке ждёт активную задачу. |
| `yaml@2` (eemeli) | `load-config.ts` всех трёх | `parseYaml(interpolated) ?? {}` → значение как plain JS, дальше zod. YAML 1.2 core schema. |
| `${ENV}` | `interpolateEnv` | Regex `/\$\{([A-Z0-9_]+)\}/gi` по **сырому тексту** до парсинга YAML; отсутствие переменной → `Missing environment variable "NAME" required by config.` |
| `zod@4` | `load-config.ts` | `.default()`, `.strict()`, `.preprocess()` (trim, upper-case ISO2, пустая строка → undefined), `.superRefine()` с кастомными сообщениями, `z.string().url()`, `z.enum`, `int/positive/min/max`. **Три разных формата ошибки**: scd — `Invalid config at <path>: a.b: msg; c: msg` (пустой путь → `<root>`); sfc — сообщения через `\n` без путей; gtl — `z.prettifyError()`. |
| `pino@10` + `pino-pretty@13` | `logging/create-logger.ts` | scd/sfc: `base: undefined` (без pid/hostname), вывод в **stderr** (`pino.destination(2)`), pretty: `colorize, translateTime: 'SYS:standard', ignore: 'pid,hostname'`, тоже stderr. gtl: `base` по умолчанию (**с pid и hostname**), вывод в **stdout**. Вызовы вида `logger.info({ event: 'config_loaded', configPath }, 'Config loaded.')`. Уровни: `fatal|error|warn|info|debug|trace|silent`. |
| SOCKS5 | `scd/src/runtime/socks-http.ts` | Ручной клиент: no-auth, CONNECT, ATYP 1 (IPv4) или 3 (domain), TLS поверх сокета для https (`servername`), сырой HTTP/1.1 запрос `Connection: close`, читается только **заголовок ответа** (до `\r\n\r\n`). |
| gRPC | `scd/src/api/xray-handler-client.ts`, `protobuf.ts` | **Нет** grpc-библиотеки и сгенерированных стабов: ручной protobuf-энкодер/декодер (~440 строк) + `node:http2`, новое HTTP/2-соединение на каждый вызов. Методы: `HandlerService/{List,Add,Remove}{Outbound(s),Inbound(s)}`, `RoutingService/{ListRule,AddRule,RemoveRule}`. TypedMessage-типы: `xray.proxy.vless.outbound.Config`, `xray.proxy.vless.Account`, `xray.proxy.socks.ServerConfig`, `xray.app.proxyman.{SenderConfig,ReceiverConfig}`, `xray.transport.internet.{tls,reality,websocket}.Config`, `xray.app.router.Config`. |
| HTML | `scd/src/runtime/status-server.ts` | Строковые шаблоны + собственный `escapeHtml` (`& < > "`; одинарная кавычка **не** экранируется), inline `<style>` и `<script>`. |

---

## 1. Toolchain: Go 1.24.7 недостаточно

Локально стоит Go 1.24.7, но на 2026-10 актуальны **Go 1.27.2** и **1.26.x** (1.24 и 1.25 уже вне поддержки). Ключевые зависимости требуют новее:

| Модуль | Последняя версия | `go` в go.mod | Последняя версия, совместимая с 1.24 |
|---|---|---|---|
| `google.golang.org/grpc` | v1.84.0 | 1.25.0 (с v1.81) | v1.80.0 |
| `golang.org/x/net` | v0.61.0 | 1.26.0 (с v0.59) | v0.50.0 |
| `github.com/xtls/xray-core` | v1.260327.0 (= Xray 26.3.27, тот же, что в `docker-compose.yml`) | **1.26** | v1.251208.0 требует уже 1.25 — совместимой нет |
| `github.com/go-playground/validator/v10` | v10.30.5 | 1.26.0 | v10.30.1 |
| `github.com/netresearch/go-cron` | v0.16.1 | 1.26 + `toolchain go1.27.1` | — |
| `google.golang.org/protobuf` | v1.36.12 | 1.23 | ок |

> **Решение (2026-10-17, требование заказчика): самый современный стек.** `go 1.27.0` + `toolchain go1.27.2` в `go.mod`, все зависимости — последних версий, в CI обязателен golangci-lint (v2.15.0, `golangci/golangci-lint-action@v9`), сборка падает при любом замечании линтера. Минимальные версии ради старого toolchain не выбираем.

~~Исходная рекомендация:~~ `go 1.26.0` в `go.mod` (+ при желании `toolchain go1.27.2`), CI — `actions/setup-go` с `go-version-file: go.mod`. Локально `GOTOOLCHAIN=auto` (дефолт) сам скачает нужный toolchain — проверено: `GOTOOLCHAIN=go1.26.9 go build` из-под 1.24.7 работает через прокси. Бонусы ≥1.25: `testing/synctest` (стабилен), `sync.WaitGroup.Go`, container-aware `GOMAXPROCS`; ≥1.26: «Green Tea» GC по умолчанию.

---

## 2. CLI

### Варианты

| | `github.com/spf13/cobra` v1.10.2 | `github.com/urfave/cli/v3` v3.14.0 | stdlib `flag` |
|---|---|---|---|
| `scd sync --config x` / `--config=x` | да (pflag) | да | да (`-config`/`--config`, оба вида значения) |
| Вложенность `blanc scd daemon` | естественно | да | ручной dispatch |
| Required-флаги | `MarkFlagRequired` → `required flag(s) "config" not set` | `Required: true` → `Required flag "config" not set` | вручную |
| Help/usage | авто, `help` подкоманда, `completion` (отключаемо) | авто | вручную |
| Зависимости | pflag (+ опц. mousetrap на Windows) | ноль | ноль |

Тексты ошибок commander (`error: required option '--config <config>' not specified`, `error: unknown command 'x'`, `error: unknown option '--foo'`, затем help из‑за `showHelpAfterError`) ни одна библиотека не воспроизводит «из коробки». Если тексты нужны 1:1 (вряд ли — это не машинный контракт), в cobra это делается `SilenceErrors/SilenceUsage` + `SetFlagErrorFunc` + собственная проверка обязательных флагов в `PreRunE`.

Отличия, которые нужно сохранить вручную:
- exit code 1 при ошибке; `sync`/`refresh` → exit 1 при `report.failed > 0` (после печати JSON-отчёта);
- `gtl --version`/`-V` печатает `1.0.2` (у cobra по умолчанию `-v` и `gtl version 1.0.2` — переопределить `SetVersionTemplate("{{.Version}}\n")` и флаг `-V`);
- gtl печатает stack для не-`ConfigError` — в Go аналог: `%+v` если ошибка несёт стек, иначе просто `err.Error()` (принять как допустимое отличие).

**Рекомендация:** cobra (де‑факто стандарт, удобное дерево `blanc → scd/sfc/gtl → подкоманды`, `DisableDefaultCmd` для completion).

### Single binary + multicall

```
blanc scd daemon --config /app/config/config.yml
blanc sfc serve  --config ...
blanc gtl serve  --config ...
blanc version
```

`main()` смотрит `filepath.Base(os.Args[0])`: если это `scd`, `sfc` или `gtl` (без суффикса `.exe`), то argv трактуется как `blanc <name> ...`. В образе/пакете — симлинки `scd -> blanc`, `sfc -> blanc`, `gtl -> blanc`, поэтому `scd daemon --config ...` работает как раньше (паттерн busybox). Корневые подкоманды каждого сервиса реализуются как отдельные `*cobra.Command`-конструкторы (`scdcli.New()`, `sfccli.New()`, `gtlcli.New()`), которые монтируются и в `blanc`, и как корень при multicall — так `--help` у `scd` выглядит как `Usage: scd daemon ...`, а не `blanc scd ...`.

---

## 3. Cron

### Фактическая семантика cron-parser 5.4.0 (как вызывается в коде)

- 5 полей (`min hour dom mon dow`) или 6 (секунды **первым** полем); 7 и более → `Invalid cron expression, too many fields`.
- Спецсимволы: `* ? , - /`, `L` (dom и dow, `5L`), `#` (dow, `1#1`), `H` (случайное значение; без `hashSeed` — **новый random при каждом `parse`**, а код парсит заново на каждом `next()` → фактически недетерминированно).
- Алиасы `JAN-DEC`, `SUN-SAT`; dow `0-7` (0 и 7 = воскресенье).
- Макросы: `@yearly @monthly @weekly @daily @hourly @minutely @secondly @weekdays @weekends` (`@annually`, `@every` — нет).
- dom и dow оба ограничены → **OR** (vixie-семантика); одно — wildcard → используется другое.
- `next()` строго больше `currentDate`, миллисекунды отбрасываются.
- Часовой пояс: локальный (luxon, системная зона) — в контейнере без `TZ` = UTC.
- Тексты ошибок (идут в сообщение конфига): `Invalid cron expression, expected 6 fields`… реально встречаются `Constraint error, got value 61 expected range 0-59`, `Validation error, cannot resolve alias "foo"`, `Invalid characters, got value: x`, `Invalid range: 5-1, min > max`, `Invalid step: 0, must be positive`, `Constraint error, invalid dayOfWeek occurrence number (#)` и т.п.

### Сравнение Go-библиотек

| | `github.com/robfig/cron/v3` v3.0.1 | `github.com/netresearch/go-cron` v0.16.1 | `github.com/adhocore/gronx` v1.20.5 | `github.com/hashicorp/cronexpr` v1.1.3 |
|---|---|---|---|---|
| Поддержка | заброшен с 2020 | активный форк robfig; Go ≥1.26, toolchain 1.27.1 | активный, 0 deps | заморожен |
| Опциональные секунды первым полем | да (`SecondOptional`) | да | 6 полей = секунды первым, **но если 6-е поле похоже на год (4 цифры) — это год**; 7 полей (+год) | 6 полей = **год в конце**, 7 = сек…год |
| dow `7` | **нет** (`end of range (7) above maximum (6)`) | да | да | да |
| `L`, `#` | нет | да (+`W`) | да (+`W`) | да (+`W`) |
| `H` | нет | частично (hash) | нет | нет |
| dom/dow оба заданы | OR | **AND** по умолчанию (намеренное отличие!) | OR | OR |
| Диапазон `5-1` | ошибка | **wraparound** (валиден) | ошибка | ошибка |
| Макросы | `@yearly…@hourly`, `@every` | то же + `@triggered` | `@yearly…`, `@5minutes`, `@always`… | `@yearly…@hourly` |
| Next строго после t | да (`t + 1s − ns`) | да | `NextTickAfter(expr, t, false)` | да |
| Тексты ошибок | свои | свои | свои | свои |

Ни одна библиотека не даёт тот же язык + те же тексты ошибок. Самый близкий по семантике — robfig с `cron.NewParser(SecondOptional|Minute|Hour|Dom|Month|Dow|Descriptor)` + нормализация `7→0`, но без `L`/`#`/`@minutely`/`@secondly`/`@weekdays`/`@weekends`.

**Рекомендация:** собственный пакет `internal/common/cron` (~300–400 строк): парсер подмножества cron-parser v5 (5/6 полей, `* ? , - /`, имена, `L`, `#`, макросы, OR-семантика, dow 0–7) с **теми же текстами ошибок**, и `Next(t time.Time) time.Time` в `time.Local`. `H` — либо отвергать с понятной ошибкой (поведение в TS всё равно недетерминированное), либо реализовать как случайное значение при парсинге. Корректность — golden-тестами: таблица (expr, currentDate) → ожидаемый `next` и тексты ошибок, сгенерированная один раз скриптом на Node с `cron-parser@5.4.0` и закоммиченная как `testdata/*.json`. Если писать свой парсер не хочется — `robfig/cron/v3` (только `Parser`/`Schedule.Next`, без его планировщика) + прекомпиляция-нормализация, с явным списком отличий в docs.

`runCronLoop` переносится 1:1 (не использовать планировщик `cron.Cron`, у него иная семантика пропусков): goroutine + `time.Timer` + `context.Context` для остановки, `atomic.Bool` для `tickInProgress`, `sync.WaitGroup` для ожидания активной задачи. Тестировать через `testing/synctest` (виртуальное время) вместо инъекции `nowFn/waitFn`, либо сохранить инъекцию интерфейсом `Clock`.

Docker-нюанс: в `scratch` нет `/etc/localtime` и zoneinfo → `time.Local` = UTC (как сейчас). Если нужен `TZ=Europe/...` — `import _ "time/tzdata"` (+~450 КБ) или distroless (там tzdata есть).

---

## 4. YAML и `${ENV}`

| | `gopkg.in/yaml.v3` v3.0.1 | `go.yaml.in/yaml/v3` v3.0.5 | `github.com/goccy/go-yaml` v1.19.2 | `sigs.k8s.io/yaml` v1.6.0 |
|---|---|---|---|---|
| Поддержка | архивирован (2025) | официальный преемник от YAML org, тот же API | активный | активный, но обёртка над `go.yaml.in/yaml/v2` |
| `yes`/`on` в `any` | строка | строка | строка | **bool** (YAML 1.1) |
| `yes` в типизированный `bool` | **true** | **true** | — | true |
| `017` | **15** (октал 1.1) | **15** | **15** | 15 |
| дубликаты ключей | ошибка `mapping key "a" already defined at line 1` | так же | ошибка с фрагментом исходника | — |

Эталон (`yaml@2`, проверено): `{"a":"yes","b":"on","c":15 (0o17),"d":17 (017 — десятичное!),"e":1,"f":null,"g":31}`; дубликат → `Map keys must be unique at line 2, column 1: ...`.

Выводы:
1. **Не декодировать сразу в типизированные структуры** — иначе `yes` станет `true`, `"1.0"`/`1.0` не влезет в `int`, а сообщения будут yaml-шные вместо zod-ных. Декодировать в `any` (или `yaml.Node`, если нужны строки/колонки), дальше — свой валидатор (раздел 5), затем маппинг в структуры.
2. Отличие `017 → 15` vs `17` — единственное заметное расхождение со схемой 1.2 при декодировании в `any`; реалистично не встречается (порты/таймауты с ведущим нулём), но можно исправить, проверив в `yaml.Node` теги `!!int` со значением, начинающимся с `0` и без `o`/`x`. Задокументировать.
3. Пустой документ → `nil` → трактовать как `{}` (аналог `?? {}`).

**Рекомендация:** `go.yaml.in/yaml/v3` (поддерживаемый drop-in, минимум сюрпризов). `goccy/go-yaml` — альтернатива, если хочется более красивых ошибок с контекстом.

`${ENV}`: один `regexp.MustCompile(`(?i)\$\{([A-Z0-9_]+)\}`)` + `ReplaceAllStringFunc` + `os.LookupEnv` по сырому тексту до парсинга, с ошибкой при отсутствии переменной (первая встреченная) — точная копия `interpolateEnv`. `os.ExpandEnv` **не подходит** (раскрывает `$VAR`, пустая строка для отсутствующих).

---

## 5. Валидация и defaults (замена zod)

`go-playground/validator/v10` (v10.30.5, требует Go 1.26):
- не задаёт defaults, не делает preprocess/trim;
- сообщения вида `Key: 'Config.Runtime.Schedule' Error:Field validation for 'Schedule' failed on the 'required' tag` — совсем не zod;
- работает поверх уже типизированной структуры → теряем различие «отсутствует» vs «неверный тип»;
- кросс-полевые правила (≈30 `superRefine`-правил scd) всё равно пишутся руками.

**Рекомендация:** без validator; маленький внутренний пакет `internal/common/schema` в стиле zod: узлы `Object(fields...).Strict()`, `String().Trim().Min(1)`, `Int().Positive()`, `Enum(...)`, `Array(...).Min(1)`, `Optional()`, `Default(v)`, `Preprocess(fn)`, `URL()`, `Refine(fn)`, обходящие `any`-дерево и собирающие `[]Issue{Path []any; Message string}`. После успешного парса — заполнение типизированных структур (напрямую из нормализованного `map[string]any` или JSON round-trip).

Тексты zod v4, которые надо повторить (сняты с zod 4.1.5):

| Ситуация | Сообщение |
|---|---|
| нет обязательного поля / не тот тип | `Invalid input: expected string, received undefined` / `expected number, received string` |
| не целое | `Invalid input: expected int, received number` |
| enum | `Invalid option: expected one of "json"\|"pretty"` |
| url | `Invalid URL` |
| min/max | `Too small: expected array to have >=1 items`, `Too small: expected number to be >=1`, `Too small: expected number to be >0` (positive), `Too big: expected number to be <=65535`, `Too small: expected string to have >=1 characters` |
| strict | `Unrecognized key: "y"` (путь — объект, не ключ) |

Тонкости поведения zod, которые нужно повторить:
- `superRefine` на объекте **не выполняется**, если внутри были структурные ошибки — проверки бизнес-правил идут только после успешного структурного парса;
- порядок issues: в порядке обхода схемы; `Unrecognized key` для объекта выдаётся после ошибок его полей;
- `z.string().url()` = успешный `new URL(x)` (WHATWG). `net/url.Parse` гораздо мягче → проверять `u.Scheme != "" && (u.Host != "" || u.Opaque != "")` плюс табличный тест против Node;
- `print-config` печатает **результат** zod (с defaults, обрезанными строками, без `undefined`-полей) в порядке ключей схемы → Go-структуры объявлять в том же порядке полей, опциональные — указатели/`omitempty`; вывод `json.Encoder` с `SetIndent("", "  ")` и **`SetEscapeHTML(false)`** (иначе `<>&` станут `<`), плюс `\n`.
- форматтеры ошибок: scd — `Invalid config at <abs path>: <path.join('.')|<root>>: <msg>; ...`; sfc — `messages.join("\n")`; gtl — `z.prettifyError`: issues отсортированы по длине пути, формат `✖ <msg>` + при непустом пути строка `  → at <a.b>`.

Отдельно: `labelIncludeRegex` — JS-литерал `/pattern/flags` → `new RegExp`. Go `regexp` (RE2) не поддерживает lookaround/backreferences и имеет другой синтаксис части классов. Для точной совместимости — `github.com/dlclark/regexp2` v1.12.0 с `regexp2.ECMAScript` (+ маппинг флагов `i`, `m`, `s`, `u`); иначе — `regexp` с явным списком неподдерживаемых конструкций в docs.

---

## 6. Логирование

Формат pino (снято с pino 10):
```
{"level":30,"time":1792225884906,"event":"config_loaded","configPath":"/x","msg":"Config loaded."}      # scd/sfc → stderr
{"level":40,"time":1792225884907,"pid":6344,"hostname":"vm","msg":"hi"}                               # gtl → stdout
```
Числовые уровни: trace 10, debug 20, info 30, warn 40, error 50, fatal 60; `time` — epoch ms; поля объекта в порядке вставки; `msg` последним.

pino-pretty (`translateTime: 'SYS:standard'`, локальное время):
```
[2026-10-17 08:31:49.002 +0000] INFO: Config loaded.
    event: "config_loaded"
    configPath: "/x"
    causes: [
      "a",
      "b"
    ]
[2026-10-17 08:31:49.007 +0000] WARN (8898): hi          # gtl: pid (hostname) не игнорируются
```
(с `colorize: true` — ANSI-цвета уровня и сообщения).

| | `log/slog` (stdlib) | `github.com/rs/zerolog` v1.35.1 | `go.uber.org/zap` v1.28.0 |
|---|---|---|---|
| Числовой `level` | свой Handler | нужен хак `LevelFieldMarshalFunc` (строкой) | свой `LevelEncoder` (можно int) |
| `time` epoch ms | свой Handler / `ReplaceAttr` | `TimeFieldFormat = TimeFormatUnixMs` | `EpochMillisTimeEncoder` |
| `msg` последним | только свой Handler (`JSONHandler` пишет `time,level,msg,attrs`) | да | нет (msg раньше полей) |
| Pretty | свой Handler | `ConsoleWriter` (другой формат) | console encoder (другой формат) |
| Зависимости | 0 | 0 | 2 |

**Рекомендация:** `log/slog` как API + собственные handler'ы в `internal/common/logging` (~200 строк):
- `PinoJSONHandler{w, level, base map}` — пишет `{"level":N,"time":ms,[pid,hostname,]...attrs,"msg":...}`, для `slog.Group` — вложенные объекты; маппинг уровней: `slog.LevelDebug-4` = trace, `slog.LevelError+4` = fatal;
- `PinoPrettyHandler` — формат выше (`[YYYY-MM-DD HH:MM:SS.mmm ±hhmm] LEVEL (pid): msg`, затем `    key: <JSON.stringify(v, null, 2)>` с отступом 4), ANSI-цвета pino-pretty (`isatty` не проверяется — pino-pretty с `colorize: true` красит всегда);
- `silent` → `slog.DiscardHandler`;
- конфиг `logger.Options{Stream: os.Stderr|os.Stdout, IncludePidHostname bool}` — сохраняет различие gtl vs scd/sfc.

Тест: golden-сравнение JSON-строк с подменой `time`. Порядок ключей в атрибутах — порядок вызова `logger.Info("...", "event", ..., ...)`; в TS поля шли из объекта первым аргументом — сохранять тот же порядок при портировании.

---

## 7. SOCKS5 и HTTP-проверки

Семантика текущего `requestViaSocks`, которую надо сохранить:
- таймауты — **idle**-таймауты на фазу (`socket.setTimeout`): connect, SOCKS-handshake, TLS-handshake, ожидание ответа; каждая со своим текстом (`SOCKS5 proxy connection timed out after Nms.`, `SOCKS5 handshake timed out after Nms.`, `SOCKS HTTPS TLS handshake timed out after Nms.`, `SOCKS HTTP request timed out after Nms.`);
- `latencyMs = round(now − requestStartedAt)`, где старт — **момент записи запроса** (после SOCKS и TLS handshake), конец — получение `\r\n\r\n` (или EOF). Т.е. это TTFB-заголовков, без handshake;
- `bodyBytes` — байты после заголовка, случайно пришедшие в тех же чанках (не полный body);
- запрос: `METHOD path HTTP/1.1`, `Host`, `Connection: close`, для POST `Content-Length: 0`; никаких `User-Agent`/`Accept-Encoding`;
- статус парсится regex `^HTTP/1\.[01]\s+(\d{3})`; TLS — с проверкой сертификата (дефолт Node);
- ATYP: IPv4 → 1, всё остальное (включая IPv6 в скобках из `URL.hostname`!) → 3 (domain). Ответ сервера с ATYP 4 обрабатывается.

Варианты в Go:
1. `golang.org/x/net/proxy.SOCKS5(...)` → `Dialer`. Делает тот же handshake, но: ошибки вида `proxy: ...`/`socks connect ...`, IPv6 отправляет как ATYP 4, таймауты — через ctx/deadline одним куском. Плюс тянет `x/net` (новые версии требуют Go 1.26).
2. `net/http.Transport{Proxy: http.ProxyURL("socks5://h:p")}` — встроенная поддержка socks5 в net/http. Удобно, но добавляет заголовки (`User-Agent: Go-http-client/1.1`, `Accept-Encoding: gzip`), может переиспользовать соединения, latency придётся мерить `httptrace` (`WroteRequest` → `GotFirstResponseByte`), тексты ошибок другие.
3. **Ручной клиент** на `net.Dialer` + `conn.SetDeadline` перед каждой фазой (≈ idle-таймаут) + `tls.Client(conn, &tls.Config{ServerName: host})` + `bufio.Reader` для чтения до `\r\n\r\n`; latency — `time.Since(start)` (monotonic clock) с `math.Round` до мс.

**Рекомендация:** вариант 3 — это ~150 строк, прямой перенос TS-логики, побайтно тот же запрос и те же ошибки, без зависимостей. Тесты через `net.Pipe()`/локальный `net.Listener` с фейковым SOCKS-сервером и `httptest.NewTLSServer`.

Прямые HTTP-проверки и загрузка подписок (`fetch` в `monitoring.ts`, `load-input.ts`) — `net/http.Client` с явным `Timeout`/контекстом. Отличия от Node `fetch` (undici), о которых стоит помнить: заголовки по умолчанию (`User-Agent: node` vs `Go-http-client/1.1`, `Accept: */*`, `Accept-Encoding: gzip, deflate, br` vs `gzip`), лимит редиректов 20 vs 10, HTTP/2 по ALPN в Go включён по умолчанию для https. Если upstream-подписки фильтруют по UA — выставлять заголовки явно.

---

## 8. gRPC к Xray

Варианты:

**A. `google.golang.org/grpc` + импорт пакетов `github.com/xtls/xray-core`** (`app/proxyman/command`, `app/router/command`, `proxy/vless/outbound`, `common/serial` и т.д.) — типизированные структуры, `serial.ToTypedMessage`.
- требует **Go 1.26** (v1.260327.0); версия модуля жёстко связана с релизом Xray (`v1.YYMMDD.0` ↔ `YY.M.D`);
- в `*.pb.go` у xray нет отдельных пакетов — импорт `command` тянет реализацию и её init-регистрации: замер (linux/amd64, `-trimpath -ldflags="-s -w"`, Go 1.26.9): только `app/proxyman/command` + `app/router/command` → **16.3 МБ**, 255 пакетов, 83 модуля (quic-go, utls, gvisor и пр.); + `proxy/vless/outbound`, `proxy/socks`, `reality`, `websocket` → **18.0 МБ**; baseline (grpc + protobuf + cobra + yaml + net/http + html/template + slog) → **13.1 МБ**, 48 модулей;
- риск: каждое обновление Xray = обновление огромного графа зависимостей и возможные breaking changes Go API (у xray нет гарантий стабильности Go API).

**B. Собственные стабы**: завендорить нужные `.proto` из xray-core (`app/proxyman/command/command.proto`, `app/router/config.proto`, `proxy/vless/...`, `transport/internet/...`, `common/serial/typed_message.proto`, `common/net/...`), сгенерировать `protoc-gen-go` + `protoc-gen-go-grpc` (через `buf`) со своим `go_package` (`internal/scd/xrayapi/pb/...`). proto-package имена остаются `xray.*`, поэтому `type_url` в TypedMessage совпадут. Меньше зависимостей, но нужен пайплайн генерации и синхронизация proto с версиями Xray.

**C. Порт текущего ручного энкодера** на `google.golang.org/protobuf/encoding/protowire` + `grpc-go` с raw-bytes codec (`encoding.Codec` для `[]byte`) или вовсе `golang.org/x/net/http2` клиент. Это 1:1 с TS (те же поля, тот же порядок байт), тесты переносятся напрямую.

**Рекомендация:** C для продакшн-кода (поведение не меняется, бинарь ~13 МБ, нет привязки к релизу Xray), а `xray-core` подключить **только в тестах** (`_test.go`, test-only зависимость не попадает в бинарь): `bufconn`-сервер, реализующий настоящий `HandlerServiceServer`/`RoutingServiceServer` из xray-core, декодирует наши байты в реальные xray-типы → проверка совместимости wire-формата. Если позже захочется типобезопасности — перейти на B. Замечание: TS открывает новое HTTP/2-соединение на каждый вызов; в Go естественно держать один `*grpc.ClientConn` (`grpc.NewClient`, `insecure.NewCredentials()`) — функционально эквивалентно, но маппинг ошибок (`ECONNREFUSED` → `Unable to connect to Xray API at host:port. Start the Xray service first, ...`) делать по `status.Code(err) == codes.Unavailable` + проверке вложенной `syscall.ECONNREFUSED`/`EPERM`, а дедлайн — `context.WithTimeout(timeoutMs)` на вызов.

Версии: `google.golang.org/grpc` v1.84.0 (Go ≥1.25), `google.golang.org/protobuf` v1.36.12.

---

## 9. HTML-дашборд

`html/template` + `//go:embed templates/*.html static/*` (`embed.FS`), `template.Must(template.ParseFS(...))` один раз при старте. Inline `<style>`/`<script>` — как `template.CSS`/`template.JS` или просто литералы в шаблоне (контекстное экранирование их не трогает, если внутри нет `{{ }}`).

Отличие: `html/template` экранирует `"` как `&#34;` и `'` как `&#39;`, а TS `escapeHtml` — `"` → `&quot;`, `'` не трогает. В браузере результат идентичен, но байтовые golden-тесты разойдутся. Варианты: (а) `html/template` и сравнение в тестах через нормализацию/парсинг (`golang.org/x/net/html`) — **рекомендуется** (безопаснее, контекстное экранирование в атрибутах и `<script>`); (б) если нужна побайтная идентичность — `text/template` + функция `escapeHtml`, портированная 1:1. JSON-эндпоинты — `encoding/json` с `SetEscapeHTML(false)`, чтобы совпадать с `JSON.stringify`.

HTTP-сервер — `net/http` + `http.ServeMux` (паттерны методов `GET /api/status` с Go 1.22), `http.Server.Shutdown(ctx)` на SIGINT/SIGTERM через `signal.NotifyContext`.

---

## 10. Тестирование

- stdlib `testing` + table-driven; `github.com/google/go-cmp` v0.7.0 для diff структур (опционально).
- `net/http/httptest` — status-server, sfc/gtl HTTP, подписки; `httptest.NewTLSServer` — HTTPS через SOCKS.
- `google.golang.org/grpc/test/bufconn` — in-memory gRPC (фейковый Xray, см. §8).
- `testing/synctest` (Go ≥1.25) — cron-циклы, таймауты, overrun без реального ожидания.
- golden-файлы в `testdata/`: cron (генерируются из `cron-parser`), ошибки конфигов (из zod), JSON-логи, `print-config`, manifest/summary для `scd generate`. Скрипт генерации на Node держать в `tools/golden/` до окончания миграции, затем удалить.
- `go test -race ./...`, `go vet ./...`, `golangci-lint` v2.15.0.
- Перенос существующих `node --test` кейсов (`*/test/**/*.test.ts`) — 1:1 по названиям, чтобы был виден прогресс паритета.

---

## 11. Сборка, Docker, CI

### Бинарь
```
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/blanc ./cmd/blanc
```
Статический бинарь; кросс-компиляция без QEMU: `GOOS=linux GOARCH=arm64`.

### Dockerfile (один на репозиторий, три target)
```dockerfile
# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/blanc ./cmd/blanc \
 && mkdir -p /out/bin && ln -s blanc /out/bin/scd && ln -s blanc /out/bin/sfc && ln -s blanc /out/bin/gtl

FROM gcr.io/distroless/static-debian12 AS base   # ca-certificates + tzdata; пинить по digest
COPY --from=build /out/blanc /usr/local/bin/blanc
COPY --from=build /out/bin/ /usr/local/bin/
WORKDIR /app

FROM base AS scd
ENTRYPOINT ["/usr/local/bin/scd"]
CMD ["daemon", "--config", "/app/config/config.yml"]

FROM base AS sfc
ENTRYPOINT ["/usr/local/bin/sfc"]
CMD ["serve", "--config", "/app/config/config.yml"]

FROM base AS gtl
ENTRYPOINT ["/usr/local/bin/gtl"]
CMD ["serve", "--config", "/app/config/config.yml"]
```
(симлинки создаются в build-стадии, т.к. в distroless нет shell.)

Выбор базы:
- `distroless/static` — CA-сертификаты (нужны для https-подписок и мониторинга), tzdata, `/etc/passwd`; без shell. **Рекомендуется.** Тег по умолчанию — root (как сейчас `node:24-alpine`), что важно для `sfc`-кэша в смонтированном `/app/config/.sfc-cache`; `:nonroot` (UID 65532) сломает запись в существующие volume — переходить отдельно.
- `scratch` — минимум, но CA и tzdata копировать руками.
- `alpine` — если нужен shell для отладки (`docker exec`).

Совместимость образов: `ghcr.io/a1ekseev/{scd,sfc,gtl}` продолжают публиковаться, с теми же путями (`/app/config/config.yml`), портами и CMD-аргументами. **Breaking:** если кто-то переопределял `command:` как `node dist/cli.js ...` — это перестанет работать (новое: `command: ["daemon", "--config", ...]` при ENTRYPOINT `scd`). Отметить в release notes. Опционально публиковать и `ghcr.io/a1ekseev/blanc` (target `base` с `ENTRYPOINT ["/usr/local/bin/blanc"]`).

Почему три target, а не один образ + три тега: теги одного образа не могут иметь разный ENTRYPOINT/CMD, а существующие `docker run ghcr.io/a1ekseev/sfc` без аргументов должны продолжать запускать `serve`. Слои `base` общие → в registry это почти бесплатно.

`docker-compose.yml`: `build: { context: ., target: scd }` и т.д.

### GitHub Actions (`.github/workflows/docker.yml`)
```yaml
verify:
  steps:
    - uses: actions/checkout@v5
    - uses: actions/setup-go@v7
      with: { go-version-file: go.mod, cache: true }
    - run: go mod verify
    - run: go vet ./...
    - uses: golangci/golangci-lint-action@v9   # v9.3.0
      with: { version: v2.15.0 }
    - run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...
    - run: go test -race -count=1 ./...
    - run: go build ./cmd/blanc && ./blanc scd validate-config --config ./testdata/scd.config.yml   # smoke
    - uses: docker/setup-buildx-action@v4
    - uses: docker/build-push-action@v7   # x3: target scd/sfc/gtl, platforms linux/amd64,linux/arm64
```
QEMU (`setup-qemu-action`) больше не нужен — кросс-компиляция в Go через `$BUILDPLATFORM`. Шаги `setup-node`/`npm ci`/`typecheck` удаляются после полного перехода (на переходный период оставить TS-джобы параллельно). `publish`: три `metadata-action` остаются как есть, в `build-push-action` добавить `context: .`, `file: ./Dockerfile`, `target: <svc>`, `build-args: VERSION=${{ github.ref_name }}`.

`.golangci.yml` (v2): `linters: default: standard` + `errorlint`, `gocritic`, `revive`, `bodyclose`, `contextcheck`, `noctx`, `modernize` (новые идиомы Go 1.2x: `min/max`, `slices`, `maps`, `for range n`, `wg.Go`), `usestdlibvars`, `perfsprint`; `formatters: gofumpt, goimports`. Линтер — блокирующий шаг CI, `issues.new: false` (с первого дня чистый код, без baseline).

---

## 12. Предлагаемая раскладка модуля

```
go.mod                         module github.com/a1ekseev/blanc   (go 1.27.0, toolchain go1.27.2)
cmd/blanc/main.go              multicall по argv[0] + cobra root
internal/cli/                  сборка дерева команд, версия, формат ошибок/exit codes
internal/common/
  cron/                        парсер подмножества cron-parser v5 + Next()        (сейчас дублируется в scd и sfc)
  cronloop/                    runCronLoop / runLoggedCronLoop                     (побайтно одинаков в scd и sfc)
  logging/                     pino-совместимые slog-handler'ы JSON/pretty          (create-logger.ts идентичен в scd и sfc)
  configload/                  чтение файла, ${ENV}, YAML → any, абсолютные пути
  schema/                      zod-подобная валидация + 3 форматтера ошибок
  jsregex/                     парсинг "/pattern/flags" (scd и sfc)
  subscription/                scan-lines (идентичен); parse-subscription-line — проверить, расходится (94 строки diff)
  listenaddr/                  разбор/валидация "host:port"
internal/scd/
  config/  api/xray/ (protowire + grpc)  builders/  apply/  normalize/  topology/
  targets/  flagcountry/  manifest/  generate/
  runtime/ (daemon, sync, monitoring, socks, status-aggregation)
  status/  (server + templates/*.html через embed)
internal/sfc/
  config/  input/  refresh/  cache/  server/
internal/gtl/
  config/  server/
testdata/ , tools/golden/ (Node-скрипты генерации эталонов, временно)
Dockerfile, .golangci.yml
```

Правило: `internal/scd`, `internal/sfc`, `internal/gtl` не импортируют друг друга; общий код — только в `internal/common/*`, и только то, что действительно идентично (см. пометки). Модули, где TS-копии разошлись (`parse-subscription-line`, `load-input`, `errors`), сначала оставлять раздельными в пакетах сервисов, объединять после паритетных тестов.

**AGENTS.md:** корневой `AGENTS.md` сейчас требует «Do not introduce cross-service shared libraries unless explicitly requested», «Each service is a standalone Node 24.14.0+ / TypeScript package…», команды `npm run …`, раздельные Dockerfile. Запрос на единый бинарь — это явное разрешение, но файл нужно обновить: один Go-модуль, разрешённые shared-пакеты (`internal/common/*`), запрет импортов между `internal/{scd,sfc,gtl}`, новые команды проверки (`go vet`, `go test -race ./...`, `go build ./cmd/blanc`, `blanc <svc> validate-config --config …`), единый Dockerfile с target'ами; плюс обновить `scd/AGENTS.md`, `sfc/AGENTS.md`, `gtl/AGENTS.md`.

---

## 13. Итоговый рекомендуемый стек

| Область | Выбор | Модуль / версия | Комментарий |
|---|---|---|---|
| Язык / toolchain | Go 1.27 (latest) | `go 1.27.0` + `toolchain go1.27.2` в go.mod, CI через `go-version-file` | 1.24.7 не хватает (grpc ≥1.81, x/net ≥0.59, xray-core) |
| CLI | cobra + multicall по argv[0] | `github.com/spf13/cobra` v1.10.2 | `blanc scd daemon` ≡ `scd daemon` (симлинк) |
| Cron | свой парсер-подмножество cron-parser v5 | `internal/common/cron` | golden-тесты из cron-parser 5.4.0; fallback — `github.com/robfig/cron/v3` v3.0.1 (только Parser) |
| Cron-цикл | порт `runCronLoop` | stdlib (`time`, `context`) | без планировщика robfig |
| YAML | декодирование в `any` | `go.yaml.in/yaml/v3` v3.0.5 | gopkg.in/yaml.v3 архивирован; sigs.k8s.io/yaml не брать (YAML 1.1 bools) |
| `${ENV}` | regex по сырому тексту | stdlib `regexp`, `os.LookupEnv` | не `os.ExpandEnv` |
| Валидация | zod-подобный мини-пакет | `internal/common/schema` | тексты zod v4 + 3 формата ошибок; без go-playground/validator |
| JS-regex фильтры | ECMAScript-совместимый движок | `github.com/dlclark/regexp2` v1.12.0 | либо RE2 с задокументированными отличиями |
| Логи | slog + свои pino-handler'ы | stdlib `log/slog` | JSON 1:1 с pino (`level` числом, `time` ms, `msg` последним), pretty как pino-pretty |
| SOCKS5 + HTTP-проба | ручной клиент | stdlib `net`, `crypto/tls`, `bufio` | та же latency (запись запроса → конец заголовков) и те же ошибки |
| HTTP-клиент | net/http | stdlib | явные UA/таймауты |
| gRPC | grpc-go + порт ручного protowire-энкодера | `google.golang.org/grpc` v1.84.0, `google.golang.org/protobuf` v1.36.12 | xray-core v1.260327.0 — только в тестах (+~3–5 МБ и 35 модулей, если тащить в бинарь) |
| HTML | html/template + embed | stdlib | или text/template + escapeHtml для побайтной идентичности |
| HTTP-сервер | net/http + ServeMux | stdlib | graceful shutdown через `signal.NotifyContext` |
| Тесты | testing, httptest, bufconn, synctest | stdlib, `google.golang.org/grpc/test/bufconn`, опц. `github.com/google/go-cmp` v0.7.0 | `go test -race` |
| Линт | golangci-lint v2 (обязателен в CI) | `github.com/golangci/golangci-lint/v2` v2.15.0, `golangci-lint-action@v9` | + `go vet`, `govulncheck`; конфиг `.golangci.yml` v2 |
| Сборка | `CGO_ENABLED=0`, `-trimpath -s -w`, amd64+arm64 кросс-компиляцией | — | без QEMU |
| Образы | один Dockerfile, target'ы `scd`/`sfc`/`gtl` на общей базе | `gcr.io/distroless/static-debian12` (root, по digest) | `ghcr.io/a1ekseev/{scd,sfc,gtl}` сохраняются; опц. `ghcr.io/a1ekseev/blanc` |
