# Миграция конфигов на Go-версию

Живой документ (D32): каждая задача, меняющая ключи конфига, значения по умолчанию или видимое через конфиг поведение, дописывает свой пункт в том же коммите. Финальная вычитка — задача 7.3.

## Общее: загрузка и валидация (все сервисы)

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `${NAME}` | scd — по сырому тексту до YAML (в т.ч. в комментариях), нет переменной → ошибка; sfc — только в строковых значениях после YAML, нет переменной → `""`; gtl — без подстановки | без изменений; текст ошибки scd: `missing environment variable "NAME" required by config` | D9 |
| YAML | `yaml@2` (YAML 1.2 core) | `go.yaml.in/yaml/v3`; дубликат ключа и несколько документов (`---`) — ошибка (`config must contain a single YAML document`); пустой файл — пустой объект; `yes`/`on` — строки; целое с ведущим `0` читается как восьмеричное (`017` → 15, было 17) — пишите без ведущего нуля | D9 |
| Пустое значение ключа (`key:` / `~`) | `null` → ошибка типа | `null` = ключ отсутствует (действует значение по умолчанию; для обязательного — `required`) | идиоматичный YAML |
| Целые числа | `5000.0` принимался (JS-число с целым значением) | только YAML-целые: `5000.0` → `expected integer, got float` | D9, D31 |
| Булевы | только YAML bool | без изменений: `yes` → `expected boolean, got string` | — |
| Ошибки валидации | тексты zod, у sfc без путей | все issues с путём поля: `invalid config <файл>:` и по строке `  - <путь>: <сообщение>` (`<root>` для корня); сообщения `required`, `expected <тип>, got <тип>`, `must be at least N`, `must be at most N`, `must be at least N characters long`, `must have at least N items`, `integer out of range`, `must be one of a\|b`, `must not be empty`, `expected string or array, got <тип>`, `invalid URL`, `unknown key "x"` | D9 |
| URL-поля | WHATWG `new URL` | `net/url.Parse` + непустые схема и хост | D9 |
| Относительный `subscriptions[].input` | от каталога конфига; `-` и `http(s)://` как есть | без изменений (`ftp://x/y` — относительный путь `<каталог>/ftp:/x/y`) | — |

## scd

Таблица «было → стало»: ключ, причина, значение по умолчанию.

| Ключ | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `statusServer.listen` | `:80` невалиден (host обязателен) | `:8080` допустим — все интерфейсы; IPv6 только в скобках (`[::]:8080`) | `net.SplitHostPort`, идиоматичный Go |

## scd sfc

Таблица «было → стало»: ключ, причина, значение по умолчанию.

| Ключ | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `server.listen` | `:80` невалиден (host обязателен) | `:8080` допустим — все интерфейсы; IPv6 только в скобках (`[::]:8080`) | `net.SplitHostPort`, идиоматичный Go |
| `subscriptions[].outputs[].labelIncludeRegex` | обязательная строка, JS-регулярка (`/pattern/flags`, lookahead допускался) | необязательна; строка **или список** RE2-выражений (AND); lookahead/lookbehind/backreferences — ошибка (см. «Фильтры меток»); в `print-config` — всегда список | D8, D29 |
| `subscriptions[].outputs[].labelExcludeRegex` | — | **новый**, необязательный: строка или список RE2-выражений (OR — метка отбрасывается при совпадении с любым) | D29 |
| фильтр output'а | `labelIncludeRegex` обязателен | обязателен **хотя бы один** из `labelIncludeRegex`/`labelExcludeRegex`, иначе `subscriptions.N.outputs.M: labelIncludeRegex or labelExcludeRegex is required` | D29 |
| `subscriptions[].fetchUserAgent` | — (заголовок `User-Agent: node`) | **новый**, необязательный: непустая строка (trim) — `User-Agent` загрузки подписки; по умолчанию `scd/<версия>`; без ключа в `print-config` его нет | D27 O9, D9 |
| `subscriptions[].fetchHeaders` | — | **новый**, необязательный: объект `имя: значение` — доп. заголовки HTTP-запроса подписки (напр. `x-hwid: "${SFC_HWID}"`), только для `http(s)`-input. Значения — строки (после `${NAME}`-подстановки), непустые, без управляющих символов; имена — валидные имена заголовков, без повторов без учёта регистра. Запрещены `User-Agent` (ошибка `use fetchUserAgent instead`), `Host`, `Content-Length`, `Transfer-Encoding`, `Connection` (`header is not allowed`). **Значения — секрет**: в `print-config` и логах — `<redacted>`. Пустой объект `{}` = ключ отсутствует. Путь ошибки — `subscriptions.N.fetchHeaders.<имя>` | D33 |
| `runtime.refreshSchedule` | cron-parser; `""` = каждую минуту | `internal/common/cron` (см. «Cron»): пустая строка/строка из пробелов — ошибка `cron expression must not be empty`; `L`/`W`/`#`/`H`, день недели `7` — ошибка; `?`, `@every`, `CRON_TZ=` — допустимы | D23, D24, D28 |
| `subscriptions[].fetchTimeoutMs` | целое `> 0`, `5000.0` допускался | целое `>= 1` (по умолчанию `5000`), `5000.0` — `expected integer, got float`; `${TM}` из env — строка, ошибка `expected integer, got string` (как и раньше, числа из env не задать) | D9 |
| `outputs[].name`, `outputs[].profileTitle` | пустая/пробельная строка → ключ исчезает | без изменений | — |
| прочие ключи (`id`, `input`, `enabled`, `format`, `pathRoute`, `outputs`, `userAgent`, `profileUpdateInterval`, `logging.*`) | — | ключи, значения по умолчанию (`enabled: true`, `format: auto`, `logging: {level: info, format: json}`) и правила без изменений: `pathRoute` начинается с `/`, не равен `/`, не оканчивается на `/`; `userAgent` — непустой список непустых строк; `profileUpdateInterval` — `1..24`; id подписок и id выходов уникальны (выходов — глобально), включая выключенные; нужна хотя бы одна включённая подписка с хотя бы одним включённым выходом. Неизвестные ключи, как и раньше, молча игнорируются (в отличие от gtl) | — |
| Ошибки валидации | тексты zod без путей, app-level проверки не выполнялись при ошибках типов | единый формат `invalid config <файл>:` + `  - <путь>: <сообщение>` (см. «Общее»), все проблемы сразу. Сообщения app-level: `subscriptions: at least one subscription must be enabled`, `subscriptions: at least one output must be enabled`, `subscriptions.N.id: duplicate subscription id "x"`, `subscriptions.N.outputs.M.id: duplicate output id "x"`, `subscriptions.N.pathRoute: must start with /` / `must not equal /` / `must not end with /`, `server.listen: listen address must be in "host:port" format` | D9 |

Команды `scd sfc validate-config --config <файл>` (вывод `{"configPath": "<абсолютный путь>", "valid": true}` с отступом 2) и `scd sfc print-config --config <файл>` (нормализованный JSON после `${NAME}`-подстановки, значений по умолчанию и резолва `input` в абсолютный путь; порядок ключей `subscriptions`, `runtime`, `server`, `logging`; у подписки `id, input, enabled, format, fetchTimeoutMs, fetchUserAgent?, fetchHeaders?, pathRoute, outputs`; у выхода `id, enabled, name?, labelIncludeRegex?, labelExcludeRegex?, userAgent?, profileTitle?, profileUpdateInterval?`) работают как раньше; `--config` обязателен. Ошибка чтения файла — `failed to read config <файл>: …`.

## scd gtl

Таблица «было → стало»: ключ, причина, значение по умолчанию.

| Ключ | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `server.listen` | `:80` невалиден (host обязателен) | `:8080` допустим — все интерфейсы; IPv6 только в скобках (`[::]:8080`) | `net.SplitHostPort`, идиоматичный Go |
| `logging.*`, `load.*` | — | ключи, значения по умолчанию (`info`/`json`, `maxSizeKb: 10240`) и проверки `load.path` без изменений; `maxSizeKb` — от 1 до `9007199254740991` (`MaxInt64/1024`, было `8796093022207`) | D9 |
| Ошибки валидации | `✖ …` / `→ at path` (zod) | единый формат `invalid config <файл>:` + `  - <путь>: <сообщение>` (см. «Общее»); неизвестный ключ — `unknown key "x"` | D9 |
| Куда пишутся логи | pino в **stdout** | **stderr**; `logging.format: pretty` — `slog.TextHandler` (см. «Логи») | D27 |
| `--version` | `gtl --version` → `1.0.2` | у `scd gtl` флага нет, версия только у корневой `scd --version` | D27 O10 |
| Запуск | `node dist/cli.js serve --config …` | `scd gtl serve --config …` (в compose: `command: ["gtl", "serve", "--config", …]`) | D10 |

Команды `scd gtl validate-config --config <файл>` (вывод `Config is valid`) и `scd gtl print-config --config <файл>` (нормализованный JSON; порядок ключей `server`, `logging`, `load`) работают как раньше; `--config` обязателен.

Разобранный пример: боевой `docker/gtl/config.real-shape.yml` → `docker/gtl/config.real-shape.go.yml`. Ключи gtl не менялись, содержимое совпадает (меняется только комментарий-шапка); `server.listen: 172.31.3.5:8080` остаётся валидным.

## Фильтры меток

Как переписать lookahead-регулярки в `labelIncludeRegex`/`labelExcludeRegex` (D29), с примерами.

Одинаково для scd (`subscriptions[].filters`) и sfc (`outputs[]`), код — `internal/common/labelfilter`:

- `labelIncludeRegex` — строка **или список**; метка проходит, только если совпала со **всеми** элементами (AND).
- `labelExcludeRegex` (новый ключ) — строка **или список**; метка отбрасывается, если совпала **хотя бы с одним** (OR).
- Оба ключа необязательны; пустая строка, строка из пробелов и пустой список — ошибка `must not be empty`. Ошибка выражения указывает путь элемента: `….labelIncludeRegex.1` (для строки — без индекса).
- `print-config` печатает оба ключа списками.

| Было (TS) | Стало (Go) |
|---|---|
| `labelIncludeRegex: '/^(?!.*Extra)(?!.*WhiteList).*$/i'` | `labelExcludeRegex: '/Extra\|WhiteList/i'` |
| `labelIncludeRegex: '/^(?=.*Extra)(?!.*WhiteList).*$/i'` | `labelIncludeRegex: '/Extra/i'` + `labelExcludeRegex: '/WhiteList/i'` |
| `labelIncludeRegex: '/(?=.*Extra)(?=.*WhiteList)/i'` | `labelIncludeRegex: ['/Extra/i', '/WhiteList/i']` |

Эквивалентность проверена на корпусе меток (`testdata/reference/common/label-filter.json`, тест `TestLabelFilterRealShapeEquivalence`). Единственное отличие — метки с настоящим переводом строки `\n` (в реальных подписках их нет): в JS `.` не матчит `\n`, поэтому TS-регулярки видели только первую строку метки, а Go-фильтр — всю метку (например, `Extra\nWhiteList` теперь проходит `['/Extra/i', '/WhiteList/i']`).

Регулярные выражения (D8) — RE2 (`regexp`), без lookahead/lookbehind/backreferences (`(?=`, `(?!`, `(?<=`, `(?<!`, `\1`…`\9`) — они дают ошибку `validate-config`. Формат значения: голый RE2-паттерн (`Extra`) или литерал `/pattern/flags`, флаги только `i`, `m`, `s` без повторов; `g`, `y`, `u`, `d`, `v` — ошибка. Сопоставление — поиск подстроки, как JS `test`. Пример: `/,\s*Extra\s*$/i`.

## Cron

Что изменилось относительно cron-parser (D24, D28). Касается всех расписаний: scd `runtime.schedule`, `subscriptions[].monitor.schedule`, `subscriptions[].balancerMonitor.schedule`, `statusServer.remoteInstances[].schedule`; sfc — расписание обновления. Один парсер (`internal/common/cron`, библиотека `github.com/robfig/cron/v3` как есть) и для `validate-config`, и для планировщика; неподдерживаемое выражение — ошибка `validate-config` с текстом библиотеки.

Синтаксис — классический Linux crontab: 5 полей `мин час день месяц день_недели` или 6 с ведущими секундами (`*/15 * * * * *` — каждые 15 с); `*`, списки `,`, диапазоны `-`, шаги `/`, имена месяцев/дней без учёта регистра (`JAN`, `mon-fri`); если ограничены и день месяца, и день недели — срабатывает по любому из них (OR), как раньше. Часовой пояс — локальный процесса (`TZ`; в образе — UTC), как раньше.

| Форма | Было (cron-parser) | Стало (Go) |
|---|---|---|
| `@yearly @monthly @weekly @daily @hourly` | да | да |
| `@annually` | да | да |
| `@midnight` | ошибка | да (= `@daily`) |
| `@minutely`, `@secondly`, `@weekdays`, `@weekends` | да | ошибка — пишите `* * * * *`, `* * * * * *`, `0 0 * * mon-fri`, `0 0 * * sat,sun` |
| `?` (= `*` в дне месяца/недели) | да | да |
| `@every <длительность>` (`@every 5m`) | ошибка | да (интервал от предыдущего слота) |
| префикс `TZ=<зона>` / `CRON_TZ=<зона>` | ошибка | да (зона только для этого выражения) |
| день недели `7` (воскресенье) | да | ошибка `end of range (7) above maximum (6)` — используйте `0` или `sun` |
| `L`, `W`, `#` (`0 0 L * *`, `1W`, `1#2`, `5L`) | `L`, `#` — да; `W` — ошибка | ошибка |
| `H` (случайное значение) | да | ошибка |
| 7 полей | ошибка | ошибка |
| пустая строка `""` | каждую минуту | ошибка `cron expression must not be empty` |
| никогда не срабатывающее (`0 0 31 2 *`, `0 0 30 2 *`) | ошибка | ошибка `cron expression never matches` |

Как и раньше, первый запуск цикла — сразу при старте, следующие — по слотам расписания без дрейфа; слот, пришедший во время выполнения предыдущего запуска, пропускается с warning `*_skipped_overrun`. Новое (D7): по SIGINT/SIGTERM циклы останавливаются сразу, не дожидаясь следующего слота (дожидаются только идущего запуска).

## Подписки

Поведение ссылок: политика allowInsecure (D15), неизвестные параметры (D28), новые профили (D26).

### sfc: какие строки попадают в выходы

Ключи конфига не менялись, но **содержимое выходов** `scd sfc` может стать больше (12 §6.2, D27 O7, D28, D18, D32).

| Строка подписки | Было (TS-sfc) | Стало (Go) | Причина |
|---|---|---|---|
| параметры вне старого списка `type, path, security, alpn, encryption, fp, sni, flow, host, allowInsecure, headerType, pbk, sid` — xhttp `mode`/`extra`/`fm`, REALITY `spx`/`pqv`/`pcs`, `vcn`, `ech`, `cs`, `support-x25519mlkem768` и т.п. | строка **молча выбрасывалась** | строка попадает в выход без изменений | общий с scd белый список параметров `vless.AllowedParams` (D27 O7) |
| неизвестный параметр (`foo=1`) | строка выбрасывалась | строка попадает в выход без изменений; лог `debug` `subscription_line_param_ignored` (`subscriptionId`, `line` — номер строки, `params`) | D28 |
| id пользователя — не UUID v1–5 (`vless://my-user@…`, UUID v6+) | строка выбрасывалась | строка попадает в выход | общий парсер `vless.Parse` (D18) |
| битая метка (`#100% fast`) | падала вся подписка (`URI malformed`), в т.ч. fallback из кэша | пропускается только эта строка | O11 |
| не-`vless://` строки, нет адреса/порта, неразбираемый URI | выбрасывались молча | без изменений | — |

sfc **не проверяет** применимость параметров к транспорту/шифрованию (это делает только scd при сборке outbound'ов): ссылки отдаются клиентам как есть. Строки `\r\n` и `\n` равнозначны; крайние пробелы контента обрезаются, как раньше.

Пример — обезличенная фикстура `docker/fixtures/real-shapes/remnawave.txt` (4 xhttp/ML-KEM-ссылки Remnawave с `mode`/`extra`) с конфигом `docker/sfc/config.real-shape.go.yml`: выход `en-server` — **0 строк → 4** (было `Filtered output is empty.`, выход отдавал 502; стало 4 строки). На `provider2` выходы не меняются (50/0/0). Если клиенты владельца не поддерживают xhttp/ML-KEM, такие строки нужно отсечь фильтром меток (`labelExcludeRegex`) — sfc фильтрует только по метке.

Прочее видимое в `scd sfc refresh`/`serve`:
- `User-Agent` загрузки — `fetchUserAgent` или `scd/<версия>` (было `node`), см. таблицу «scd sfc».
- Отчёт `refresh`: `outputs` сортируются побайтно по `id` (`strings.Compare`: `Z1` < `_a` < `a`; было ICU `localeCompare`: `_a` < `a` < `Z1`) (D23). Тексты ошибок загрузки — Go (`unable to download input from <url>: …`, `open <путь>: no such file or directory`) (D9).
- Файлы кэша источника — прежние `<каталог конфига>/.sfc-cache/<encodeURIComponent(id)>.subscription`: volume `.sfc-cache` после миграции подхватывается как есть. Новые файлы создаются с правами `0600` (было `0666 & umask`).

## Запуск

Одна команда `scd …`, один образ, `command:` в docker-compose для sfc/gtl (D10).

## Логи

Формат и поля логов (D27, D9, D32).

- **Куда пишем.** Все три сервиса (`scd`, `sfc`, `gtl`) пишут логи только в **stderr** процесса; stdout занят JSON-выводом команд. Было: pino в stdout (scd/sfc/gtl по-разному). Причина: единое поведение, `docker logs` и пайпы JSON-вывода не смешиваются.
- **Формат** — `logging.format`: `json` или `pretty`; любое другое значение — ошибка `unsupported log format "<f>" (expected json or pretty)`.
  - `json` = `slog` JSON: `time` в RFC3339 (было: pino `time` в миллисекундах), `level` строкой `INFO`/`WARN`/`ERROR`/`DEBUG`/`TRACE`/`FATAL` (было: числовой pino `level`), `msg`, далее `event` и остальные поля. Имена событий (`event`) и поля сохранены.
  - `pretty` = `slog.TextHandler`: строка `time=… level=INFO msg="…" event=… key=value`.
- **Уровни** прежние: `fatal`, `error`, `warn`, `info`, `debug`, `trace`, `silent` (`silent` — ничего не пишется).
- Пример строки `json`: `{"time":"2026-10-17T12:00:00.123456789Z","level":"INFO","msg":"Sync finished.","event":"sync_finished"}`.
- Если логи разбирались по числовому `level` или `time` в мс (Loki/jq-фильтры), их нужно переписать на строковый `level` и RFC3339 `time`.

## Разобранные примеры

Боевые конфиги `docker/{scd,sfc,gtl}/config.real-shape.yml` → Go-формы `config.real-shape.go.yml` (diff по шагам).

### sfc: `docker/sfc/config.real-shape.yml` → `docker/sfc/config.real-shape.go.yml`

TS-форма не проходит `scd sfc validate-config`: все три фильтра построены на lookahead —

```
invalid config docker/sfc/config.real-shape.yml:
  - subscriptions.0.outputs.0.labelIncludeRegex: lookahead, lookbehind and backreferences are not supported (RE2 syntax): error parsing regexp: invalid or unsupported Perl syntax: `(?!`
  - subscriptions.0.outputs.1.labelIncludeRegex: lookahead, … `(?=`
  - subscriptions.0.outputs.2.labelIncludeRegex: lookahead, … `(?=`
```

Шаг 1 — `en-server` (всё, кроме Extra и WhiteList): «нет ни Extra, ни WhiteList» = исключение по любому из них.

```diff
-        labelIncludeRegex: '/^(?!.*Extra)(?!.*WhiteList).*$/i'
+        labelExcludeRegex: '/Extra|WhiteList/i'
```

Шаг 2 — `extra-server` (Extra, но не WhiteList): положительная часть — в include, отрицательная — в exclude.

```diff
-        labelIncludeRegex: '/^(?=.*Extra)(?!.*WhiteList).*$/i'
+        labelIncludeRegex: '/Extra/i'
+        labelExcludeRegex: '/WhiteList/i'
```

Шаг 3 — `extra-whitelist` (и Extra, и WhiteList, в любом порядке): список include — метка должна совпасть со всеми.

```diff
-        labelIncludeRegex: '/(?=.*Extra)(?=.*WhiteList)/i'
+        labelIncludeRegex: ['/Extra/i', '/WhiteList/i']
```

Остальные ключи (`input`, `format`, `fetchTimeoutMs: 15000`, `pathRoute`, `userAgent: [Happ]`, `profileTitle`, `profileUpdateInterval: 1`, `refreshSchedule: "15 * * * *"`, `server.listen: 172.31.3.4:8080`, `logging`) не меняются. Новые ключи необязательны: при необходимости добавьте у подписки `fetchUserAgent` (иначе панель увидит `User-Agent: scd/<версия>` вместо `node` — если панель выдаёт разный контент по User-Agent, задайте нужный явно) и `fetchHeaders` (например, `x-hwid: "${SFC_HWID}"`). Проверка: `scd sfc validate-config --config docker/sfc/config.real-shape.go.yml` → `"valid": true` (тест `TestSFCRealShapeGoFormValid`).

## Чек-лист

- `scd validate-config --config …`
- `scd sfc validate-config --config …`
- `scd gtl validate-config --config …`
