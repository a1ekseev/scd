# Миграция конфигов на Go-версию

Живой документ (D32): каждая задача, меняющая ключи конфига, значения по умолчанию или видимое через конфиг поведение, дописывает свой пункт в том же коммите. Финальная вычитка — задача 7.3.

## Общее: загрузка и валидация (все сервисы)

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `${NAME}` | scd — по сырому тексту до YAML (в т.ч. в комментариях), нет переменной → ошибка; sfc — только в строковых значениях после YAML, нет переменной → `""`; gtl — без подстановки | sfc и gtl — без изменений. **scd — только в значениях**: комментарии и ключи больше не раскрываются (`# ${NOPE}` не ошибка), значение переменной не может изменить структуру YAML (`a: ${V}` с `V="x: y"` — строка `x: y`). Незакавыченное значение типизируется после подстановки (`enabled: ${E}` с `E=true` — bool, `fetchTimeoutMs: ${T}` с `T=7000` — целое), закавыченное (`"${E}"`) — всегда строка. Внутри flow-коллекций (`{…}`, `[…]`) `${NAME}` нужно закавычить (`{address: "${XRAY}"}`), иначе ошибка YAML — `{` в незакавыченном значении там спецсимвол; в блочной форме (`address: ${XRAY}`) кавычки не нужны. Нет переменной — ошибка `missing environment variable "NAME" required by config` (первая по порядку значений) | D9 |
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
| `subscriptions[].filters.labelIncludeRegex` | строка, JS-регулярка (`/pattern/flags`, lookahead допускался); пустая строка = ключа нет | строка **или список** RE2-выражений (AND); lookahead/lookbehind/backreferences — ошибка (см. «Фильтры меток»); пустая строка/пустой список — ошибка `must not be empty`; в `print-config` — всегда список | D8, D29 |
| `subscriptions[].filters.labelExcludeRegex` | — | **новый**, необязательный: строка или список RE2-выражений (OR — метка отбрасывается при совпадении с любым) | D29 |
| `subscriptions[].filters.countryAllowlist` | trim + верхний регистр, длина 2, один из 45 кодов; ошибка на весь список | без изменений по смыслу; ошибка — по пути элемента: `subscriptions.N.filters.countryAllowlist.M: must be a 2-letter ISO2 country code` / `unsupported ISO2 country code "ZZ"`. `[]` по-прежнему допустим (фильтр по стране не применяется), но в `print-config` не печатается (было `[]`) | D9 |
| `subscriptions[].allowInsecure` | — (ссылки с `allowInsecure=1` принимались, TLS без проверки) | **новый**: `deny` (по умолчанию) — ссылка с `allowInsecure=1` без `pcs`/`vcn` пропускается с причиной; `allow` — outbound создаётся (в Xray v26.10.10 поля нет — сертификат всё равно проверяется), warning в лог. Ссылки с `pcs`/`vcn` — штатно. Иные значения — `must be one of deny\|allow` | D15, O6 |
| `subscriptions[].fetchUserAgent` | — (заголовок `User-Agent: node`) | **новый**, необязательный: непустая строка (trim) — `User-Agent` загрузки подписки по http(s); по умолчанию `scd/<версия>`; пустая строка — `must not be empty`; без ключа в `print-config` его нет | D27 O9, D9 |
| `subscriptions[].fetchHeaders` | — | **новый**, необязательный: объект `имя: значение` — доп. заголовки HTTP-запроса подписки (напр. `x-hwid: "${SCD_HWID}"`), только для `http(s)`-input; правила и ошибки — как у `scd sfc` (`use fetchUserAgent instead`, `header is not allowed`, `invalid header name`, `expected string, got …`, путь `subscriptions.N.fetchHeaders.<имя>`). **Значения — секрет**: в `print-config` и логах — `<redacted>` (исключение из правила «`print-config` печатает подставленные секреты как есть»). `${NAME}` подставляется только в значениях (комментарии больше не раскрываются — отличие от TS, см. «Общее»); переменная обязана существовать | D33 |
| `subscriptions[].target.balancerSelection` | — | **новый** объект (strict), по умолчанию `{enabled: false, observatory: true}`: `enabled`, `balancerTag` (= `routing.balancers[].tag` в Xray, не ruleTag), `schedule` (cron, обычно 6 полей: `"*/15 * * * * *"`), `observatory` (опрашивать `ObservatoryService`), `strategy` (необязательная подсказка для UI: `leastPing\|leastLoad\|random\|roundRobin`). При `enabled: true` обязательны `balancerTag` и `schedule` (`required when balancerSelection.enabled is true`); без `observatorySubjectSelectorPrefix` — не ошибка, а предупреждение `validate-config` (см. ниже) | D22, `10` §6.5 |
| `logging.format` | `json\|pretty` (pino), по умолчанию `json` | значения и умолчание прежние; смысл — см. «Логи» (slog, stderr) | D27 O10 |
| `target.inboundSocks.listen` | любая непустая строка; домен (кроме `localhost`) принимался и падал только на `AddInbound` (`domain address is not allowed for listening`) | проверяется в `validate-config`: IP-адрес (v4/v6, напр. `0.0.0.0`, `"::"`) или ровно `localhost`; иначе `subscriptions.N.target.inboundSocks.listen: must be an IP address or "localhost" (Xray cannot listen on a domain)`. IPv6 теперь кодируется как IP (было — домен, отвергался Xray) | 02 §1.5, D9 |
| `target.inboundSocks.portRange.start/end` | целые `> 0`, верхняя граница не проверялась | целые `1..65535` (`must be at most 65535`); `end < start` — `subscriptions.N.target.inboundSocks.portRange.end: must be greater than or equal to start` | D9 |
| URL-поля: `target.monitor.request.url`, `target.balancerMonitor.request.url`, `target.balancerMonitor.remotePing.url`, `statusServer.remoteInstances[].apiUrl` | любой URL, разбираемый WHATWG (`mailto:a@b`, `ftp://x` проходили) | `net/url` + непустые схема и хост (`mailto:a@b` — `invalid URL`); значение не нормализуется | D9 |
| `runtime.schedule`, `target.monitor.schedule`, `target.balancerMonitor.schedule`, `statusServer.remoteInstances[].schedule` | cron-parser; `""` в `print-config` печаталось как `""` | `internal/common/cron` (см. «Cron»); пустая строка/пробелы у необязательных расписаний = ключа нет (не печатается); **заданное** расписание проверяется всегда, даже если функция выключена (`monitor.enabled: false` + `schedule: "61 * * * *"` — ошибка; было — не проверялось); где расписание обязательно — ошибка `required when …` (у `remoteInstances[].schedule` — `required` / `must not be empty`) | D24, D28 |
| `fixedOutbounds`, `fixedInbounds`, `fixedRouting` | список непустых строк, по умолчанию `[]` | без изменений (`[]` допустим, пустой элемент — `subscriptions.N.target.fixedOutbounds.M: must not be empty`) | — |
| Неизвестные ключи | strict (ошибка): подписка, `target`, `monitor`, `retry`, `balancerMonitor`, `remotePing`, `remoteInstances[]`; остальные объекты — молча игнорируются | без изменений + strict `balancerSelection`; текст `subscriptions.N.target: unknown key "speedtest"` (было `Unrecognized key: "speedtest"`). Старые `targets:`, `target.speedtest`, `balancerMonitor.successGet`, `monitor.remotePing` — по-прежнему ошибка | D9, D31 |
| Кросс-полевые правила | тексты с путём `subscriptions` и точкой в конце; при `daemon` без `schedule` остальные правила (4–9) не проверялись | выполняются **все** правила сразу; путь — конкретное поле. Сообщения: `subscriptions: at least one subscription must be enabled`, `subscriptions: duplicate target address "<адрес>"`, `runtime.schedule: required when runtime.mode is "daemon"`, `resources.inbounds.enabled: requires resources.outbounds.enabled`, `resources.routing.enabled: requires resources.outbounds.enabled and resources.inbounds.enabled`, `subscriptions.N.target.inboundSocks: required when resources.inbounds.enabled or resources.routing.enabled is true` / `required when monitor.enabled is true`, `subscriptions.N.target.monitor.{schedule,request}: required when monitor.enabled is true`, `subscriptions.N.target.monitor.enabled: requires resources.outbounds.enabled, resources.inbounds.enabled and resources.routing.enabled`, `subscriptions.N.target.balancerMonitor.{schedule,socks5,request}: required when balancerMonitor.enabled is true`, `subscriptions.N.target.balancerMonitor.remotePing.url: required when remotePing.enabled is true`, `subscriptions.N.target.balancerMonitor.remotePing.enabled: requires balancerMonitor.enabled`, `statusServer.listen: required when statusServer.enabled is true`, `statusServer.remoteInstances.N.id: duplicate remote instance id "<id>"`; ошибка cron — текст парсера по пути расписания | D9 |
| Ошибки валидации | `Invalid config at <путь>: a: …; b: …` (zod) | единый формат `invalid config <файл>:` + `  - <путь>: <сообщение>` (см. «Общее»); ошибка чтения — `failed to read config <файл>: …` (было `ENOENT: …` без префикса) | D9 |

Команды `scd validate-config --config <файл>` (stdout `{"configPath": "<абсолютный путь>", "valid": true}` с отступом 2; **новое** — предупреждения пишутся в stderr строками `warning: <путь>: <текст>`, код выхода 0; сейчас единственное — `warning: subscriptions.N.target.balancerSelection: balancerSelection is enabled without observatorySubjectSelectorPrefix; scd tunnels are unlikely to match the balancer selector`) и `scd print-config --config <файл>` (нормализованный JSON; порядок ключей подписки `id, input, enabled, format, fetchTimeoutMs, allowInsecure, fetchUserAgent?, fetchHeaders?, filters?, target`; у `filters` — `countryAllowlist?, labelIncludeRegex?, labelExcludeRegex?`; у target — `address, timeoutMs, fixedOutbounds, fixedInbounds, fixedRouting, visionUdp443Override, inboundSocks?, monitor, balancerMonitor, balancerSelection, observatorySubjectSelectorPrefix?`) работают как раньше; `--config` обязателен.

`scd sync --config <файл>` (JSON-отчёт в stdout, логи в stderr, код 1 при `failed > 0`) — тот же отчёт и те же события, с отличиями (D23, D28, D33):
- `skipped` в отчёте и в `targets[]` — пропущенные строки манифеста **один раз** (было: повторялись по числу включённых `resources.*`);
- нехватка портов в `inboundSocks.portRange` (`target "<адрес>" inboundSocks.portRange can allocate only N ports, but M tunnels were generated: …`) — failed-отчёт по каждому kind **только этого target**, остальные подписки применяются (было: падал весь sync с `sync_failed`, отчёт не печатался);
- URL подписки в логах (`subscription_fetched.source`, `subscription_fetch_failed.source`/`error`) и в тексте ошибки загрузки в отчёте — `<scheme>://<host>/<redacted>` (было: полный URL с токеном);
- `manifest_built.managedCount` — число управляемых ресурсов своего kind (было: `0` для inbound/routing);
- новое событие `manifest_entry_warning` (`subscriptionId, targetAddress, tag, code, message`): `flow_cleared`, `allow_insecure_ignored` — `warn`, `param_ignored` — `debug`.

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

Что изменилось относительно cron-parser (D24, D28). Касается всех расписаний: scd `runtime.schedule`, `subscriptions[].monitor.schedule`, `subscriptions[].balancerMonitor.schedule`, `subscriptions[].target.balancerSelection.schedule`, `statusServer.remoteInstances[].schedule`; sfc — расписание обновления. Один парсер (`internal/common/cron`, библиотека `github.com/robfig/cron/v3` как есть) и для `validate-config`, и для планировщика; неподдерживаемое выражение — ошибка `validate-config` с текстом библиотеки.

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

### scd: политика `allowInsecure`

Новый ключ `subscriptions[].allowInsecure` (D15), по умолчанию `deny`: ссылка с `allowInsecure=1` и без `pcs`/`vcn` пропускается (в отчёте — причина), т.к. в Xray v26.10.10 отключить проверку сертификата нельзя и такой outbound всё равно не подключился бы к серверу с невалидным сертификатом. `allow` — ссылка принимается, параметр игнорируется с warning `allow_insecure_ignored`. Ссылки с `pcs` (`pinnedPeerCertSha256`) или `vcn` (`verifyPeerCertByName`) обрабатываются штатно при любом значении.

Ссылка `allowInsecure=1` проверяется только при `security=tls`: у REALITY и у ML-KEM без TLS параметр неприменим и молча игнорируется (O16), политика на такие ссылки не влияет. Значение, кроме `0|1|true|false` (без учёта регистра), по-прежнему пропускает ссылку при любой политике.

### scd: какие ссылки становятся outbound'ами

Ключи конфига не менялись; меняется, какие строки подписки дают outbound'ы и с какими тегами (D26, `11-xray-vless-matrix.md` §3). Проверить на своей подписке: `scd generate` — `skipped` с причинами и `warnings` у entries.

| Ссылка | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| неизвестный параметр (`foo=1`) | skip `unsupported_param` | entry, параметр отбрасывается, warning `param_ignored` «Ignored unknown VLESS parameter(s): foo.» (в логе sync — уровень `debug`) | D28 |
| параметр, неприменимый к transport/security (`path=/` у tcp без `headerType=http`, `alpn`/`pcs` у REALITY, `pbk`/`sid` у TLS, `serviceName`, `support-x25519mlkem768`, `x_padding_bytes` …) | параметры из старого списка TS (`path`, `host`, `alpn`, `pbk`, `sid` …) молча игнорировались, прочие — skip `unsupported_param` | молча игнорируется, без warning; виден только в `normalized.query` | O16, O13, D28 |
| `type=xhttp`, REALITY без `flow`, VLESS Encryption (`encryption=mlkem768x25519plus.*`, в т.ч. с `security=none`) | skip `unsupported_combo`/`unsupported_value` | entry с новым профилем (ниже) | D26 |
| `type=grpc`, `httpupgrade`, `kcp` и прочие транспорты; `ws`+`reality`; `security=none` (или без `security`) без ML-KEM; `security=xtls` | skip `unsupported_combo` | без изменений: skip `unsupported_combo` | D26 (вне scope) |
| ключ ML-KEM правильной длины, но не ключ | — | skip `unsupported_value` «Invalid VLESS encryption key.» (иначе Xray отверг бы outbound при `AddOutbound`) | D25 |
| `flow=xtls-rprx-vision` на ws/xhttp без ML-KEM | ws+tls: outbound с `flow` (Xray v26.10.10 рвал бы каждое соединение) | `flow` сбрасывается, warning `flow_cleared` «flow xtls-rprx-vision is not supported on ws+tls; flow cleared.» | D16 |
| значение `flow` | tcp+tls и ws+tls — любое значение как есть; REALITY — только `xtls-rprx-vision`, иначе skip `unsupported_combo` | `""`, `xtls-rprx-vision`, `xtls-rprx-vision-udp443` (регистр важен); иное — skip `unsupported_value` «Unsupported VLESS flow value.» (Xray его не принимает) | D16, D26 |
| id не UUID (`vless://my-user@…`), UUID v6+ | skip | принимается; id приводится как в Xray (`uuid.ParseString`: строка ≤30 символов → UUIDv5, UUID — в нижний регистр; тег от этого не зависит). Skip `unsupported_value` «The VLESS user id is not accepted by Xray.» — только если Xray его не примет (31 символ, 37+ символов, не-hex UUID) | D18, O4 |
| ws без `path`; REALITY без `sni`/`fp`/`sid` | skip `missing_required_field` | принимается (Xray принимает); у REALITY обязателен только `pbk` | O15 |

Профили и теги. Старые профили и **теги их outbound'ов не меняются** (`vless-<cc>-<город>-<профиль>-<sha8(строки)>`): `tcp-tls` (в т.ч. tcp+tls+vision), `ws-tls`, `tcp-reality-vision`. Новые профили: `tcp-reality` (REALITY без flow), `xhttp-tls`, `xhttp-reality`, `xhttp-reality-vision` (только с ML-KEM), `tcp-mlkem`, `ws-mlkem`, `xhttp-mlkem` (`security=none` + ML-KEM). ML-KEM поверх tls/reality профиль не меняет. Если фильтры или правила маршрутизации завязаны на профиль в теге, их нужно дополнить новыми именами.

Пример: `vless://…@h:443?type=tcp&security=reality&pbk=…&sid=ab#🇩🇪 Berlin` — было skip «Unsupported VLESS type/security/flow combination.», стало outbound `vless-de-berlin-tcp-reality-<hash>`.

### scd: `/api/runtime-state` (`statusServer.runtimeState`)

Ключи `statusServer.runtimeState.includeRaw`/`includeSecrets` и их умолчания (`false`) не менялись; меняется содержимое `runtime.*[].parsed` (03 §6.4, D6, D33).

| Поле `parsed` | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| outbound `path` (ws, xhttp, raw/tcp с `headerType=http`) | ws `path` не выводился (декодер читал не то поле), остальные — не выводились | выводится; при `includeSecrets: false` — `"<redacted>"` (пустой path — ключа нет), при `true` — как есть (у raw `headerType=http` — список путей через запятую). При `includeRaw: true` поле `rawBase64` содержит нередактированный protobuf (uuid, path, host, ключи) независимо от `includeSecrets`. | D33: xhttp-путь реальных подписок — секретоподобная строка, как URL подписки |
| outbound `host` | ws `host` не выводился | ws — как есть; xhttp — при `includeSecrets: false` `"<redacted>"` | D6, D33 |
| outbound `encryption` у ML-KEM | — (такие ссылки пропускались) | при `includeSecrets: false` — `"mlkem768x25519plus"` вместо ключа; при `true` — значение Xray (без префикса `mlkem768x25519plus.<режим>.<rtt>.`) | D26, D33 |
| outbound `fingerprint` (tls) | не выводился (читалось поле 10) | значение `fp` из ссылки | D6 |
| outbound `network` | `tcp`/`websocket` | плюс `splithttp` (xhttp) и новое поле `xhttpMode` | D26 |
| outbound `security`/`sni`/`fingerprint`/`alpn` при `security=none` (ML-KEM) | — | ключей нет | D26 |
| inbound `listen` | IPv6-адрес не выводился | IPv6 в записи Go (`::`, `::1`) | D9 |
| routing `inboundTags` | `[]` на реальном Xray | `[]` всегда (ListRule не возвращает inbound-теги) | 02 §1.5 |

Пример (`includeSecrets: false`, xhttp+tls+ML-KEM): `{"protocol":"vless","address":"srv.example.com","port":443,"encryption":"mlkem768x25519plus","network":"splithttp","security":"tls","sni":"…","alpn":["h2"],"fingerprint":"chrome","path":"<redacted>","xhttpMode":"stream-up"}`.

Остальные части ответа runtime-state (03 §6.4–§6.5, D9, D15, D31, D33):

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `config.subscription.fetchHeaders` | — | новый ключ (если заданы `fetchHeaders`): имена заголовков со значением `"<redacted>"` при `includeSecrets: false`, реальные значения при `true` | D33 |
| `config.subscription.target` (копия target внутри подписки) | URL мониторинга печатались **без** редактирования | редактируются так же, как `config.target` | D33: URL мониторинга/push могут содержать токены |
| редактирование URL (`input`, `monitor.request.url`, `balancerMonitor.request.url`, `remotePing.url`) | `<origin>/<redacted>` (WHATWG: порт по умолчанию опускался, у схем без origin — `null/<redacted>`) | `<scheme>://<host>/<redacted>`, хост и порт — как в исходной строке; без схемы/хоста или при ошибке разбора — `<redacted>` | D9 (`net/url`) |
| `expected` для файла | `buildManifest` + фильтры + validate | то же, что в sync (`manifest.FromSubscription`): с политикой `allowInsecure` подписки, тем же trim/фильтрами/validate; текст ошибки — Go | D9, D15 |
| набор ключей `config.subscription`/`config.target` | схема TS | схема Go (D31): напр. `allowInsecure`, `fetchUserAgent`, `target.balancerSelection`; все ключи TS сохранены | D31 |
| routing `rawBase64` | сырой protobuf правила | protobuf элемента `ListRuleItem` (outbound-тег и `ruleTag` — всё, что отдаёт Xray) | 02 §1.5 |
| `rawBase64` (любой kind) | ключ при `includeRaw: true` всегда | ключ опускается, если значение пустое | D3 (аддитивно) |
| сортировки списков `runtime`/`diff` | `localeCompare` (ICU) | побайтно (`strings.Compare`) | D23 |

### scd: status server и JSON API (`statusServer`)

Ключи `statusServer.*` не менялись. Схемы `/api/status`, `/api/instance-status`, `/api/instances` и `version: 1` сохранены (D3); отличия — в поведении:

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| маршрутизация | точное сравнение URL **с query**: `/api/status?x=1` отдавал HTML | сравнивается только путь: `/api/status?x=1` → JSON; `/api/runtime-state*` — по префиксу пути, как раньше | O8 |
| сортировка подписок, target'ов, туннелей (при равной latency), remote-инстансов | `localeCompare` (ICU) | побайтно (`strings.Compare`) | D23 |
| порядок ключей JSON | порядок вставки JS (у local и remote в `/api/instances` разный) | порядок полей Go-структур; набор ключей и значения те же | D23 |
| исключение в обработчике (напр. странный remote snapshot) | падение процесса | ответ 500 и лог `http_handler_panic`, сервер работает | D23 |
| ошибки сбора remote (`lastError`) | тексты Node (`fetch failed`, `The operation was aborted due to timeout`) | тексты Go (`net/http`, `context deadline exceeded`, ошибки разбора JSON); `Remote status API returned HTTP <код>.` и тексты проверки схемы — без изменений | D9 |
| `instanceId` без `statusServer.instanceId` | `os.hostname()` | `os.Hostname()`, при ошибке — `localhost` | D9 |

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
- `serve` при старте обновляет выходы **один** раз (до запуска HTTP, как раньше), следующее обновление — в первый слот `refreshSchedule` с логом `refresh_tick`; раньше сразу за начальным обновлением шёл второй, немедленный тик cron (04 §12 п.1, O12). Это исключение из «первый запуск цикла — сразу при старте» выше.
- `serve` по SIGINT/SIGTERM останавливается сразу (D7): цикл обновления не ждёт следующего слота, идущее обновление прерывается отменой загрузки и дожидается завершения, HTTP-запросы дорабатывают до 10 с; второй сигнал завершает процесс немедленно. Раньше процесс жил до следующего слота cron (до 10 минут при `*/10`), и `docker stop` заканчивался SIGKILL. Ошибка bind `server.listen`, как и раньше, — после начального обновления.
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

### scd: `docker/scd/config.real-shape.yml` → `docker/scd/config.real-shape.go.yml`

TS-форма не проходит `scd validate-config`: фильтр меток обеих подписок построен на lookahead —

```
invalid config docker/scd/config.real-shape.yml:
  - subscriptions.0.filters.labelIncludeRegex: lookahead, lookbehind and backreferences are not supported (RE2 syntax): error parsing regexp: invalid or unsupported Perl syntax: `(?!`
  - subscriptions.1.filters.labelIncludeRegex: lookahead, … `(?!`
```

Шаг 1 — `blanc-en` и `blanc-ru` (всё, кроме Extra и WhiteList): «нет ни Extra, ни WhiteList» = исключение по любому из них (строка D29 №1). `countryAllowlist` не меняется.

```diff
     filters:
       countryAllowlist: [AT, BE, BG, CH, CZ, DE, DK, ES, FI, FR, GB, GR, HR, HU, IE, IT, LT, NL, "NO", PL, PT, RO, SK, TR]
-      labelIncludeRegex: '/^(?!.*Extra)(?!.*WhiteList).*$/i'
+      labelExcludeRegex: '/Extra|WhiteList/i'
```

Шаг 2 — остальное без изменений. Все прочие ключи (`target.*` с `inboundSocks`, `monitor`, `balancerMonitor.socks5`, `remotePing.viaSocks: true`, `observatorySubjectSelectorPrefix`, `visionUdp443Override`; `runtime: daemon` с `"*/5 * * * *"`; `statusServer` с `instanceId`, `listen: 172.31.0.8:8080`, `runtimeState`) валидны в Go-схеме как есть. Расписания `*/1 * * * *` и `*/5 * * * *` — обычный crontab.

Шаг 3 (необязательно) — новые ключи, все со значениями по умолчанию, которые сохраняют прежнее поведение, кроме двух мест, где оно меняется само:
- `allowInsecure` — по умолчанию `deny`: ссылки с `allowInsecure=1` без `pcs`/`vcn` теперь пропускаются (см. «Подписки»). Если провайдер отдаёт такие ссылки и они нужны — `allowInsecure: allow`.
- `fetchUserAgent` — без ключа панель увидит `User-Agent: scd/<версия>` вместо `node`; если панель выдаёт разный контент по User-Agent, задайте его явно.
- `fetchHeaders` (например, `x-hwid: "${SCD_HWID}"` — переменная должна быть задана), `balancerSelection` (для панели выбора балансера на дашборде, напр. `{enabled: true, balancerTag: x-balancer, schedule: "*/15 * * * * *"}`; префикс `x-observe-` уже задан, поэтому предупреждения не будет).

Проверка: `scd validate-config --config docker/scd/config.real-shape.go.yml` → `"valid": true`, stderr пуст (тест `TestSCDRealShapeGoFormValid`).

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
