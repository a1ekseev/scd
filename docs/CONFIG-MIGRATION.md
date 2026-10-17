# Миграция конфигов на Go-версию

Инструкция владельца по переводу конфигов `scd`, `sfc` и `gtl` (TS-версия) на единый Go-бинарь `scd` (D32). Обратной совместимости нет (D31): старые конфиги с lookahead-фильтрами не проходят `validate-config`, алиасов старых ключей нет. Документ поддерживается вместе с кодом: изменение ключей конфига, значений по умолчанию или видимого через конфиг поведения дописывается сюда в том же коммите; тест `TestConfigMigrationDocCoversKeys` (`internal/cli/configdoc_test.go`) проверяет, что каждый ключ, которого нет в одной из схем (TS — `testdata/reference/<svc>/config-keys.json`, Go — типы конфигов), упомянут полным путём в разделе своего сервиса.

Общая документация — [`README.md`](../README.md).

## Порядок перехода

1. Переписать фильтры меток с lookahead на `labelIncludeRegex`/`labelExcludeRegex` — «[Фильтры меток](#фильтры-меток)». Это единственное, без чего боевые конфиги не проходят валидацию.
2. Решить, нужен ли `allowInsecure: allow` (scd) — «[Подписки](#подписки)».
3. Проверить User-Agent загрузки подписок (`fetchUserAgent`, по умолчанию теперь `scd/<версия>`) и при необходимости добавить `fetchHeaders` — «[scd](#scd)», «[Подписки](#подписки)».
4. Заменить формы cron, которых больше нет (`@minutely`, день недели `7`, `L`/`#`/`H`, пустое расписание sfc) — «[Cron](#cron)».
5. Перевести compose на один образ `ghcr.io/a1ekseev/scd` с `command:` для sfc/gtl — «[Запуск](#запуск)».
6. Переписать разбор логов (stderr, строковый `level`, RFC3339 `time`) — «[Логи](#логи)».
7. Прогнать «[Чек-лист](#чек-лист)».

Полные примеры на боевых конфигах — «[Разобранные примеры](#разобранные-примеры)».

## Общее: загрузка и валидация (все сервисы)

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `${NAME}` | scd — по сырому тексту до YAML (в т.ч. в комментариях), нет переменной → ошибка; sfc — только в строковых значениях после YAML, нет переменной → `""`; gtl — без подстановки | sfc и gtl — без изменений. **scd — только в значениях**: комментарии и ключи не раскрываются (`# ${NOPE}` не ошибка), значение переменной не меняет структуру YAML (`a: ${V}` с `V="x: y"` — строка `x: y`). Незакавыченное значение типизируется после подстановки (`enabled: ${E}` с `E=true` — bool, `fetchTimeoutMs: ${T}` с `T=7000` — целое), закавыченное (`"${E}"`) — всегда строка. Внутри flow-коллекций (`{…}`, `[…]`) `${NAME}` нужно закавычить, в блочной форме кавычки не нужны. Нет переменной — `missing environment variable "NAME" required by config` | D9 |
| YAML | `yaml@2` (YAML 1.2 core) | `go.yaml.in/yaml/v3`; дубликат ключа и несколько документов (`---`) — ошибка (`config must contain a single YAML document`); пустой файл — пустой объект; `yes`/`on` — строки; целое с ведущим `0` — восьмеричное (`017` → 15, было 17) | D9 |
| Пустое значение ключа (`key:` / `~`) | `null` → ошибка типа | `null` = ключ отсутствует (значение по умолчанию; для обязательного — `required`) | идиоматичный YAML |
| Целые числа | `5000.0` принимался | только YAML-целые: `5000.0` → `expected integer, got float` | D9, D31 |
| Булевы | только YAML bool | без изменений: `yes` → `expected boolean, got string` | — |
| Ошибки валидации | тексты zod, у sfc без путей | все проблемы сразу, с путём поля: `invalid config <файл>:` и по строке `  - <путь>: <сообщение>` (`<root>` для корня). Сообщения: `required`, `expected <тип>, got <тип>`, `must be at least N`, `must be at most N`, `must be at least N characters long`, `must have at least N items`, `integer out of range`, `must be one of a\|b`, `must not be empty`, `expected string or array, got <тип>`, `invalid URL`, `unknown key "x"`. Ошибка чтения — `failed to read config <файл>: …` | D9 |
| URL-поля | WHATWG `new URL` (`mailto:a@b`, `ftp://x` проходили) | `net/url.Parse` + непустые схема и хост (`mailto:a@b` — `invalid URL`); значение не нормализуется | D9 |
| `listen`-адреса (`statusServer.listen`, sfc/gtl `server.listen`) | `:80` невалиден (host обязателен) | `:8080` допустим — все интерфейсы; IPv6 только в скобках (`[::]:8080`); иначе `listen address must be in "host:port" format` | `net.SplitHostPort` |
| Относительный `subscriptions[].input` | от каталога конфига; `-` и `http(s)://` как есть | без изменений (`ftp://x/y` — относительный путь `<каталог>/ftp:/x/y`) | — |
| `--config` | обязателен | без изменений | — |

Примеры:

```yaml
# было: подставлялось и в комментарии (нет переменной → ошибка даже здесь)
# input: ${OLD_URL}
input: ${SCD_SUB_URL}            # стало: только значения
target: {address: "${XRAY}"}     # внутри {…} — в кавычках
fetchTimeoutMs: 15000            # было допустимо 15000.0 — теперь только целое
statusServer:
  listen: ":9090"                # было: ошибка; стало: все интерфейсы
```

## scd

Таблица «было → стало» по ключам `scd` (значение по умолчанию — в колонке «Стало»).

| Ключ | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `subscriptions[].filters.labelIncludeRegex` | строка, JS-регулярка (`/pattern/flags`, lookahead допускался); пустая строка = ключа нет | строка **или список** RE2-выражений (AND); lookahead/lookbehind/backreferences — ошибка; пустая строка/пустой список — `must not be empty`; в `print-config` — всегда список | D8, D29 |
| `subscriptions[].filters.labelExcludeRegex` | — | **новый**, необязательный: строка или список RE2-выражений (OR — метка отбрасывается при совпадении с любым) | D29 |
| `subscriptions[].filters.countryAllowlist` | trim + верхний регистр, длина 2, один из 45 кодов; ошибка на весь список | смысл тот же; ошибка — по пути элемента: `subscriptions.N.filters.countryAllowlist.M: must be a 2-letter ISO2 country code` / `unsupported ISO2 country code "ZZ"`. `[]` допустим (фильтра нет), в `print-config` не печатается (было `[]`) | D9 |
| `subscriptions[].allowInsecure` | — (ссылки с `allowInsecure=1` принимались, TLS без проверки) | **новый**: `deny` (по умолчанию) — ссылка с `allowInsecure=1` без `pcs`/`vcn` пропускается с причиной; `allow` — outbound создаётся (в Xray v26.10.10 поля нет — сертификат проверяется), warning `allow_insecure_ignored`. Иное — `must be one of deny\|allow` | D15, O6 |
| `subscriptions[].fetchUserAgent` | — (заголовок `User-Agent: node`) | **новый**, необязательный: непустая строка (trim) — `User-Agent` загрузки по http(s); по умолчанию `scd/<версия>`; пустая строка — `must not be empty`; без ключа в `print-config` его нет | D27 O9 |
| `subscriptions[].fetchHeaders` | — | **новый**, необязательный: объект `имя: значение` — доп. заголовки HTTP-запроса подписки, только для `http(s)`-input. Значения — непустые строки без управляющих символов; имена — валидные, без повторов без учёта регистра; `User-Agent` — `use fetchUserAgent instead`; `Host`, `Content-Length`, `Transfer-Encoding`, `Connection` — `header is not allowed`; путь ошибки `subscriptions.N.fetchHeaders.<имя>`; `{}` = ключа нет. **Значения — секрет**: в `print-config` (без `--include-secrets`), логах и runtime-state (без `includeSecrets`) — `<redacted>` | D33 |
| `subscriptions[].target.balancerSelection` | — | **новый** объект (strict), по умолчанию `{enabled: false, observatory: true}`; подробно — «[Выбор балансера Xray](#scd-выбор-балансера-xray-targetbalancerselection)» | D22, `10` §6.5 |
| `subscriptions[].target.balancerSelection.enabled` | — | включает опрос выбора балансера; по умолчанию `false` | D22 |
| `subscriptions[].target.balancerSelection.balancerTag` | — | `routing.balancers[].tag` в Xray (**не** ruleTag); обязателен при `enabled: true` (`required when balancerSelection.enabled is true`) | D22 |
| `subscriptions[].target.balancerSelection.schedule` | — | cron, обычно 6 полей с секундами (`"*/15 * * * * *"`); обязателен при `enabled: true` | D22, D24 |
| `subscriptions[].target.balancerSelection.observatory` | — | опрашивать `ObservatoryService.GetOutboundStatus`; по умолчанию `true` | D22 |
| `subscriptions[].target.balancerSelection.strategy` | — | необязательная подсказка для UI: `leastPing\|leastLoad\|random\|roundRobin` | D22 |
| `subscriptions[].target.inboundSocks.listen` | любая непустая строка; домен падал только на `AddInbound` (`domain address is not allowed for listening`) | проверяется в `validate-config`: IP (v4/v6, `0.0.0.0`, `"::"`) или `localhost`; иначе `must be an IP address or "localhost" (Xray cannot listen on a domain)`. IPv6 кодируется как IP (было — как домен, Xray отвергал) | 02 §1.5, D9 |
| `subscriptions[].target.inboundSocks.portRange.start/end` | целые `> 0`, верх не проверялся | `1..65535` (`must be at most 65535`); `end < start` — `must be greater than or equal to start` | D9 |
| URL-поля: `target.monitor.request.url`, `target.balancerMonitor.request.url`, `target.balancerMonitor.remotePing.url`, `statusServer.remoteInstances[].apiUrl` | любой URL WHATWG | `net/url` + непустые схема и хост (см. «Общее») | D9 |
| расписания: `runtime.schedule`, `target.monitor.schedule`, `target.balancerMonitor.schedule`, `statusServer.remoteInstances[].schedule` | cron-parser; `""` печаталось как `""` | `internal/common/cron` (см. «[Cron](#cron)»); пустая строка у необязательного расписания = ключа нет; **заданное** расписание проверяется всегда, даже при выключенной функции (`monitor.enabled: false` + `schedule: "61 * * * *"` — ошибка; было — нет) | D24, D28 |
| `statusServer.listen` | см. «Общее» | `:9090` допустим | — |
| `logging.format` | `json\|pretty` (pino), по умолчанию `json` | значения и умолчание прежние; смысл — «[Логи](#логи)» (slog, stderr) | D27 O10 |
| `fixedOutbounds`, `fixedInbounds`, `fixedRouting` | список непустых строк, по умолчанию `[]` | без изменений (пустой элемент — `…fixedOutbounds.M: must not be empty`) | — |
| `runtime`, `logging`, `resources` | обязательные объекты (допустимо `{}`) | без изменений | — |
| Неизвестные ключи | ошибка в подписке, `target`, `monitor`, `retry`, `balancerMonitor`, `remotePing`, `remoteInstances[]`; в остальных объектах — молча игнорируются | ошибка **в каждом объекте**: корень, подписка, `filters`, `target`, `inboundSocks`, `portRange`, `monitor`, `retry`, `request`, `balancerMonitor`, `socks5`, `remotePing`, `balancerSelection`, `runtime`, `logging`, `resources` (и `outbounds`/`inbounds`/`routing`), `statusServer`, `runtimeState`, `remoteInstances[]`; текст `subscriptions.N.target: unknown key "speedtest"` (было `Unrecognized key: "speedtest"`), у корня — `<root>: unknown key "x"`. Опечатка вроде `statusServer: {enable: true}` теперь ошибка, а не молча выключенный дашборд. Старые `targets:`, `target.speedtest`, `balancerMonitor.successGet`, `monitor.remotePing` — по-прежнему ошибка | D9, D31 |
| Кросс-полевые правила | путь `subscriptions`, точка в конце; при `daemon` без `schedule` остальные правила не проверялись | проверяются **все** сразу, путь — конкретное поле: `subscriptions: at least one subscription must be enabled`, `subscriptions: duplicate target address "<адрес>"`, `runtime.schedule: required when runtime.mode is "daemon"`, `resources.inbounds.enabled: requires resources.outbounds.enabled`, `resources.routing.enabled: requires resources.outbounds.enabled and resources.inbounds.enabled`, `subscriptions.N.target.inboundSocks: required when resources.inbounds.enabled or resources.routing.enabled is true` / `required when monitor.enabled is true`, `subscriptions.N.target.monitor.{schedule,request}: required when monitor.enabled is true`, `subscriptions.N.target.monitor.enabled: requires resources.outbounds.enabled, resources.inbounds.enabled and resources.routing.enabled`, `subscriptions.N.target.balancerMonitor.{schedule,socks5,request}: required when balancerMonitor.enabled is true`, `subscriptions.N.target.balancerMonitor.remotePing.url: required when remotePing.enabled is true`, `subscriptions.N.target.balancerMonitor.remotePing.enabled: requires balancerMonitor.enabled`, `statusServer.listen: required when statusServer.enabled is true`, `statusServer.remoteInstances.N.id: duplicate remote instance id "<id>"` | D9 |

### scd: примеры было → стало

Фильтр меток (подробно — «[Фильтры меток](#фильтры-меток)»):

```diff
     filters:
       countryAllowlist: [DE, NL]
-      labelIncludeRegex: '/^(?=.*Extra)(?!.*WhiteList).*$/i'
+      labelIncludeRegex: '/Extra/i'
+      labelExcludeRegex: '/WhiteList/i'
```

Новые ключи подписки (все необязательны; без них — `deny`, `User-Agent: scd/<версия>`, без доп. заголовков):

```diff
   - id: main
     input: https://panel.example.com/sub/<token>
+    allowInsecure: allow              # было: allowInsecure=1 принимался молча; стало по умолчанию: deny (skip)
+    fetchUserAgent: v2rayN/7.0        # было: User-Agent: node; стало по умолчанию: scd/<версия>
+    fetchHeaders:
+      x-hwid: "${SCD_HWID}"           # значение — секрет, в print-config: "<redacted>"
```

`print-config` для этой подписки: `"allowInsecure": "allow", "fetchUserAgent": "v2rayN/7.0", "fetchHeaders": {"x-hwid": "<redacted>"}`.

Выбор балансера (было — функции нет):

```diff
     target:
       observatorySubjectSelectorPrefix: x-observe-
+      balancerSelection:
+        enabled: true
+        balancerTag: x-balancer        # routing.balancers[].tag, не ruleTag
+        schedule: "*/15 * * * * *"
+        # observatory: true            # по умолчанию
+        # strategy: leastPing          # подсказка для UI
```

`inboundSocks` (было — валидировалось только Xray при apply):

```diff
       inboundSocks:
-        listen: xray.internal          # домен: было — ошибка AddInbound при sync
+        listen: 0.0.0.0                # стало: IP или localhost, иначе ошибка validate-config
         portRange:
           start: 20000
-          end: 70000                   # было: принималось; стало: must be at most 65535
+          end: 20099
```

Расписание выключенного монитора (было — не проверялось):

```diff
       monitor:
         enabled: false
-        schedule: "@minutely"          # стало: unrecognized descriptor: @minutely
+        schedule: "* * * * *"
```

### scd: команды

`scd validate-config --config <файл>` — stdout `{"configPath": "<абсолютный путь>", "valid": true}` (отступ 2); **новое** — предупреждения в stderr строками `warning: <путь>: <текст>`, код 0. Сейчас одно: `warning: subscriptions.N.target.balancerSelection: balancerSelection is enabled without observatorySubjectSelectorPrefix; scd tunnels are unlikely to match the balancer selector`.

`scd print-config --config <файл> [--include-secrets]` — нормализованный JSON. **Новое (D33):** по умолчанию секреты скрыты — http(s)-`input` подписки и `target.monitor.request.url`, `target.balancerMonitor.request.url`, `target.balancerMonitor.remotePing.url` печатаются как `<scheme>://<host>/<redacted>`, значения `fetchHeaders` — `<redacted>` (было: `input` и URL — полностью, с токенами). `--include-secrets` печатает всё как есть. Порядок ключей подписки `id, input, enabled, format, fetchTimeoutMs, allowInsecure, fetchUserAgent?, fetchHeaders?, filters?, target`; у `filters` — `countryAllowlist?, labelIncludeRegex?, labelExcludeRegex?`; у `target` — `address, timeoutMs, fixedOutbounds, fixedInbounds, fixedRouting, visionUdp443Override, inboundSocks?, monitor, balancerMonitor, balancerSelection, observatorySubjectSelectorPrefix?`.

`scd sync --config <файл>` (JSON-отчёт в stdout, логи в stderr, код 1 при `failed > 0`) — тот же отчёт и те же события, с отличиями (D23, D28, D33):
- `skipped` в отчёте и в `targets[]` — пропущенные строки манифеста **один раз** (было: повторялись по числу включённых `resources.*`);
- нехватка портов в `inboundSocks.portRange` (`target "<адрес>" inboundSocks.portRange can allocate only N ports, but M tunnels were generated: …`) — failed по каждому kind **только этого target**, остальные применяются (было: падал весь sync с `sync_failed`, отчёт не печатался);
- URL подписки в логах (`subscription_fetched.source`, `subscription_fetch_failed.source`/`error`) и в тексте ошибки загрузки в отчёте — `<scheme>://<host>/<redacted>` (было: полный URL с токеном);
- `manifest_built.managedCount` — число управляемых ресурсов своего kind (было: `0` для inbound/routing);
- новое событие `manifest_entry_warning` (см. «[Логи](#логи)»).

`scd generate --input … --output … --log …` — без изменений по флагам; содержимое манифеста — по новым правилам ссылок («[Подписки](#подписки)»), `jsonOutbound` — форма Xray v26.10.10.

## scd sfc

Таблица «было → стало» по ключам `scd sfc` (бывший `sfc`).

| Ключ | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `subscriptions[].outputs[].labelIncludeRegex` | обязательная строка, JS-регулярка (lookahead допускался) | необязательна; строка **или список** RE2-выражений (AND); lookahead — ошибка; в `print-config` — всегда список | D8, D29 |
| `subscriptions[].outputs[].labelExcludeRegex` | — | **новый**, необязательный: строка или список RE2-выражений (OR) | D29 |
| фильтр output'а | `labelIncludeRegex` обязателен | обязателен **хотя бы один** из `labelIncludeRegex`/`labelExcludeRegex`, иначе `subscriptions.N.outputs.M: labelIncludeRegex or labelExcludeRegex is required` | D29 |
| `subscriptions[].fetchUserAgent` | — (заголовок `User-Agent: node`) | **новый**, необязательный: непустая строка (trim); по умолчанию `scd/<версия>`; без ключа в `print-config` его нет | D27 O9 |
| `subscriptions[].fetchHeaders` | — | **новый**, необязательный: правила, ошибки и редактирование — как у `scd` (`subscriptions[].fetchHeaders` выше), напр. `x-hwid: "${SFC_HWID}"` | D33 |
| `runtime.refreshSchedule` | cron-parser; `""` = каждую минуту | `internal/common/cron`: пустая строка/пробелы — `cron expression must not be empty`; `L`/`W`/`#`/`H`, день недели `7` — ошибка; `?`, `@every`, `CRON_TZ=` — допустимы | D23, D24, D28 |
| `subscriptions[].fetchTimeoutMs` | целое `> 0`, `5000.0` допускался | целое `>= 1` (по умолчанию `5000`); `${TM}` из env — строка, `expected integer, got string` (как и раньше) | D9 |
| `server.listen` | `:80` невалиден | `:8080` допустим (см. «Общее») | — |
| `outputs[].name`, `outputs[].profileTitle` | пустая/пробельная строка → ключ исчезает | без изменений | — |
| прочие (`id`, `input`, `enabled`, `format`, `pathRoute`, `outputs`, `userAgent`, `profileUpdateInterval`, `logging.*`) | — | без изменений: умолчания `enabled: true`, `format: auto`, `logging: {level: info, format: json}`; `pathRoute` начинается с `/`, не равен `/`, не оканчивается на `/`; **новое**: путь должен быть «чистым» — без пустых, `.` и `..` сегментов и без символов, требующих экранирования (пробел, `%`, `?`, не-ASCII), иначе `subscriptions.N.pathRoute: must be a clean URL path (…)` (раньше такой маршрут проходил проверку, но никогда не совпадал с запросом — вечный 404); `userAgent` — непустой список непустых строк; `profileUpdateInterval` — `1..24`; id подписок и выходов уникальны (выходов — глобально), включая выключенные; нужна включённая подписка с включённым выходом | — |
| Неизвестные ключи | молча игнорировались | ошибка в каждом объекте (корень, подписка, выход, `runtime`, `server`, `logging`), как у scd и gtl: `subscriptions.N.outputs.M: unknown key "x"`, `<root>: unknown key "x"` | D9, D31 |
| Ошибки валидации | тексты zod без путей; app-level проверки не выполнялись при ошибках типов | формат «Общее», все проблемы сразу: `subscriptions: at least one subscription must be enabled`, `subscriptions: at least one output must be enabled`, `subscriptions.N.id: duplicate subscription id "x"`, `subscriptions.N.outputs.M.id: duplicate output id "x"`, `subscriptions.N.pathRoute: must start with /` / `must not equal /` / `must not end with /` | D9 |

### scd sfc: примеры было → стало

```diff
 subscriptions:
   - id: main
     input: https://panel.example.com/sub/<token>
+    fetchUserAgent: v2rayN/7.0        # было: User-Agent: node; по умолчанию теперь scd/<версия>
+    fetchHeaders:
+      x-hwid: "${SFC_HWID}"
     pathRoute: /s
     outputs:
       - id: 11111111-2222-4333-8444-555555555555
-        labelIncludeRegex: '/^(?!.*Extra)(?!.*WhiteList).*$/i'
+        labelExcludeRegex: '/Extra|WhiteList/i'
 runtime:
-  refreshSchedule: ""                 # было: каждую минуту; стало: ошибка
+  refreshSchedule: "* * * * *"
```

### scd sfc: команды

`scd sfc validate-config --config <файл>` (`{"configPath": "<абсолютный путь>", "valid": true}`), `scd sfc print-config [--include-secrets]` (JSON после `${NAME}`, умолчаний и резолва `input`; http(s)-`input` — `<scheme>://<host>/<redacted>` и значения `fetchHeaders` — `<redacted>`, если нет `--include-secrets` (было: `input` полностью); порядок `subscriptions`, `runtime`, `server`, `logging`; у подписки `id, input, enabled, format, fetchTimeoutMs, fetchUserAgent?, fetchHeaders?, pathRoute, outputs`; у выхода `id, enabled, name?, labelIncludeRegex?, labelExcludeRegex?, userAgent?, profileTitle?, profileUpdateInterval?`), `scd sfc refresh`, `scd sfc serve`. Отличия `refresh`/`serve` — «[sfc: какие строки попадают в выходы](#sfc-какие-строки-попадают-в-выходы)».

URL подписки в логах `refresh`/`serve` (`subscription_refreshed.source`, `error` у `subscription_source_cache_used`/`subscription_source_cache_missed`/`subscription_refresh_failed`) и в `outputs[].error` отчёта `refresh` — `<scheme>://<host>/<redacted>` (было: полный URL с токеном), как у scd (D33).

## scd gtl

Ключи `scd gtl` (бывший `gtl`) не менялись.

| Ключ | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `server.listen` | `:80` невалиден | `:8080` допустим (см. «Общее») | — |
| `load.path`, `load.maxSizeKb`, `logging.*` | — | ключи, умолчания (`maxSizeKb: 10240`, `info`/`json`) и проверки `load.path` прежние; `maxSizeKb` — от 1 до `9007199254740991` (было до `8796093022207`) | D9 |
| Ошибки валидации | `✖ …` / `→ at path` (zod) | формат «Общее»; неизвестный ключ — `unknown key "x"` | D9 |

Пример — без изменений ключей, меняется только запуск:

```diff
 # docker-compose.yml
   gtl:
-    image: ghcr.io/a1ekseev/gtl:latest
+    image: ghcr.io/a1ekseev/scd:latest
+    command: ["scd", "gtl", "serve", "--config", "/app/config/config.yml"]
```

Команды: `scd gtl validate-config` (вывод `Config is valid`), `scd gtl print-config` (порядок `server`, `logging`, `load`), `scd gtl serve`. `--version` у `scd gtl` нет — только `scd --version` (D27 O10). Логи — в stderr (было — stdout), см. «[Логи](#логи)». Поведение HTTP прежнее (`200` на валидный размер, `400`/`404`/`405`), payload побайтно тот же; при обрыве клиента генерация останавливается (D7).

## Фильтры меток

Как переписать lookahead-регулярки в `labelIncludeRegex`/`labelExcludeRegex` (D29). Одинаково для scd (`subscriptions[].filters`) и sfc (`outputs[]`), код — `internal/common/labelfilter`:

- `labelIncludeRegex` — строка **или список**; метка проходит, только если совпала со **всеми** (AND).
- `labelExcludeRegex` (новый) — строка **или список**; метка отбрасывается, если совпала **хотя бы с одним** (OR).
- Оба необязательны (в sfc у выхода нужен хотя бы один); пустая строка, строка из пробелов и пустой список — `must not be empty`. Ошибка выражения указывает путь элемента: `….labelIncludeRegex.1` (для строки — без индекса).
- Синтаксис — RE2 (Go `regexp`, D8): голый паттерн (`Extra`) или литерал `/pattern/flags`, флаги только `i`, `m`, `s` без повторов (`g`, `y`, `u`, `d`, `v` — ошибка). Lookahead/lookbehind/backreferences (`(?=`, `(?!`, `(?<=`, `(?<!`, `\1`…`\9`) — ошибка `validate-config`: `lookahead, lookbehind and backreferences are not supported (RE2 syntax): …`. Сопоставление — поиск подстроки, как JS `test`.
- `print-config` печатает оба ключа списками.

| Было (TS) | Стало (Go) |
|---|---|
| `labelIncludeRegex: '/^(?!.*Extra)(?!.*WhiteList).*$/i'` | `labelExcludeRegex: '/Extra\|WhiteList/i'` |
| `labelIncludeRegex: '/^(?=.*Extra)(?!.*WhiteList).*$/i'` | `labelIncludeRegex: '/Extra/i'` + `labelExcludeRegex: '/WhiteList/i'` |
| `labelIncludeRegex: '/(?=.*Extra)(?=.*WhiteList)/i'` | `labelIncludeRegex: ['/Extra/i', '/WhiteList/i']` |
| `labelIncludeRegex: '/,\s*Extra(?!\s*Whitelist)\s*$/i'` | `labelIncludeRegex: '/,\s*Extra\s*$/i'` (`$` после `Extra` уже исключает `Extra Whitelist`) |
| `labelIncludeRegex: '/\b(?:Extra\|Premium)\b(?!.*\bWhitelist\b)/i'` | `labelIncludeRegex: '/\b(?:Extra\|Premium)\b/i'` + `labelExcludeRegex: '/\bWhitelist\b/i'` |
| `labelIncludeRegex: '/\b(?:DE\|NL)\b(?!.*\bBackup\b)/i'` | `labelIncludeRegex: '/\b(?:DE\|NL)\b/i'` + `labelExcludeRegex: '/\bBackup\b/i'` |

Правило: каждое «не содержит X» (`(?!.*X)`) уходит в `labelExcludeRegex`, каждое «содержит X» (`(?=.*X)`) — отдельным элементом `labelIncludeRegex`. Отрицательный lookahead **после** конкретного места (`Extra(?!\s*Whitelist)`) эквивалентен исключению только если `X` больше нигде в метке не встречается — проверьте на своих метках (`scd generate` показывает `filtered`).

Эквивалентность трёх боевых фильтров проверена на корпусе меток (`testdata/reference/common/label-filter.json`, `TestLabelFilterRealShapeEquivalence`). Единственное отличие — метки с настоящим `\n` (в реальных подписках их нет): в JS `.` не матчит `\n`, и TS-регулярки видели только первую строку метки, Go-фильтр — всю метку (`Extra\nWhiteList` проходит `['/Extra/i', '/WhiteList/i']`).

Метки scd не интерпретирует сверх страны (флаг) и города (текст до первой запятой) (D33): суффиксы вроде `-X`, слова `Extra`/`WhiteList` значат что-то только для ваших фильтров.

## Cron

Что изменилось относительно cron-parser (D24, D28). Касается всех расписаний: scd `runtime.schedule`, `subscriptions[].target.monitor.schedule`, `subscriptions[].target.balancerMonitor.schedule`, `subscriptions[].target.balancerSelection.schedule`, `statusServer.remoteInstances[].schedule`; sfc `runtime.refreshSchedule`. Один парсер (`internal/common/cron`, `github.com/robfig/cron/v3` как есть) и для `validate-config`, и для планировщика; неподдерживаемое выражение — ошибка `validate-config` с текстом библиотеки.

Синтаксис — классический Linux crontab: 5 полей `мин час день месяц день_недели` или 6 с ведущими секундами (`*/15 * * * * *` — каждые 15 с); `*`, списки `,`, диапазоны `-`, шаги `/`, имена месяцев/дней без учёта регистра (`JAN`, `mon-fri`); если ограничены и день месяца, и день недели — срабатывает по любому (OR), как раньше. Часовой пояс — локальный процесса (`TZ`; в образе — UTC).

| Форма | Было (cron-parser) | Стало (Go) |
|---|---|---|
| `@yearly @annually @monthly @weekly @daily @hourly` | да | да |
| `@midnight` | ошибка | да (= `@daily`) |
| `@minutely`, `@secondly`, `@weekdays`, `@weekends` | да | ошибка (`unrecognized descriptor`) — пишите `* * * * *`, `* * * * * *`, `0 0 * * mon-fri`, `0 0 * * sat,sun` |
| `?` (= `*` в дне месяца/недели) | да | да |
| `@every <длительность>` (`@every 5m`) | ошибка | да (интервал от предыдущего слота) |
| `TZ=<зона> …` / `CRON_TZ=<зона> …` | ошибка | да (зона только для этого выражения) |
| день недели `7` (воскресенье) | да | ошибка `end of range (7) above maximum (6)` — `0` или `sun` |
| `L`, `W`, `#` (`0 0 L * *`, `1W`, `1#2`, `5L`) | `L`, `#` — да; `W` — ошибка | ошибка |
| `H` | да | ошибка |
| 7 полей | ошибка | ошибка |
| пустая строка `""` | каждую минуту | ошибка `cron expression must not be empty` (у необязательных расписаний scd пустая строка = ключа нет) |
| никогда не срабатывает (`0 0 31 2 *`) | ошибка | ошибка `cron expression never matches` |

```diff
-  schedule: "@minutely"
+  schedule: "* * * * *"
-  schedule: "0 3 * * 7"
+  schedule: "0 3 * * sun"
-  schedule: "0 0 L * *"               # последний день месяца — точной замены нет,
+  schedule: "0 0 1 * *"               # например, перенести на первое число
```

Как и раньше: первый запуск цикла — сразу при старте (исключение — `scd sfc serve`, см. ниже), следующие — по слотам без дрейфа; слот, пришедший во время предыдущего запуска, пропускается с warning `*_tick_skipped_overrun`. Новое: слоты, пропущенные из-за сна хоста или скачка часов, не догоняются пачкой — один warning `*_tick_skipped_overrun`, дальше со следующего слота после текущего времени. Новое (D7): по SIGINT/SIGTERM циклы останавливаются сразу, не дожидаясь следующего слота.

## Подписки

Поведение ссылок: политика `allowInsecure` (D15), неизвестные параметры (D28), новые профили (D26), User-Agent и заголовки загрузки (D27 O9, D33). Ключи — в таблицах сервисов выше; здесь — что меняется в результатах.

### scd: политика `allowInsecure`

`subscriptions[].allowInsecure: deny` (по умолчанию): ссылка с `allowInsecure=1` и без `pcs`/`vcn` пропускается с причиной «allowInsecure=1 requires pcs or vcn: Xray v26.10.10 no longer supports allowInsecure (set subscriptions[].allowInsecure: allow to keep such links).» — в Xray v26.10.10 отключить проверку сертификата нельзя, такой outbound не подключился бы к серверу с невалидным сертификатом. `allow` — ссылка принимается, параметр игнорируется с warning `allow_insecure_ignored`. Ссылки с `pcs` (`pinnedPeerCertSha256`) или `vcn` (`verifyPeerCertByName`) обрабатываются штатно при любой политике.

`allowInsecure=1` проверяется только при `security=tls`: у REALITY и у ML-KEM без TLS параметр неприменим и молча игнорируется (O16). Значение, кроме `0|1|true|false` (без учёта регистра), пропускает ссылку при любой политике.

### scd: какие ссылки становятся outbound'ами

Меняется, какие строки подписки дают outbound'ы и с какими тегами (D26, `go-migration/11-xray-vless-matrix.md` §3). Проверить на своей подписке: `scd generate --input <файл> --output m.json --log log.json` — `skipped` с причинами и `warnings` у entries.

| Ссылка | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| неизвестный параметр (`foo=1`) | skip `unsupported_param` | entry, параметр отбрасывается, warning `param_ignored` «Ignored unknown VLESS parameter(s): foo.» (в логе sync — `debug`) | D28 |
| параметр, неприменимый к transport/security (`path=/` у tcp без `headerType=http`, `alpn`/`pcs` у REALITY, `pbk`/`sid` у TLS, `serviceName`, `support-x25519mlkem768`, `x_padding_bytes` …) | из старого списка TS (`path`, `host`, `alpn`, `pbk`, `sid` …) — молча игнорировались, прочие — skip `unsupported_param` | молча игнорируется, без warning; виден только в `normalized.query` | O16, O13, D28 |
| `type=xhttp`, REALITY без `flow`, VLESS Encryption (`encryption=mlkem768x25519plus.*`, в т.ч. с `security=none`), параметры `spx`, `pqv`, `pcs`, `vcn`, `ech`, `fm`, `mode`, `extra`, `cs`, ws `heartbeatPeriod` | skip `unsupported_combo`/`unsupported_value`/`unsupported_param` | entry с новым профилем (ниже), параметры доходят до Xray (`cs` → `tlsSettings.cipherSuites`, `heartbeatPeriod` → `wsSettings.heartbeatPeriod`) | D26, D33 |
| `type=grpc`, `httpupgrade`, `kcp` и прочие транспорты; `ws`+`reality`; `security=none` (или без `security`) без ML-KEM; `security=xtls` | skip `unsupported_combo` | без изменений: skip `unsupported_combo` | D26 (вне scope) |
| ключ ML-KEM правильной длины, но не ключ | — | skip `unsupported_value` «Invalid VLESS encryption key.» (иначе Xray отверг бы outbound при `AddOutbound`) | D25 |
| `flow=xtls-rprx-vision` на ws/xhttp без ML-KEM | ws+tls: outbound с `flow` (Xray v26.10.10 рвал бы каждое соединение) | `flow` сбрасывается, warning `flow_cleared` «flow xtls-rprx-vision is not supported on ws+tls; flow cleared.» | D16 |
| значение `flow` | tcp+tls и ws+tls — любое как есть; REALITY — только `xtls-rprx-vision`, иначе skip | `""`, `xtls-rprx-vision`, `xtls-rprx-vision-udp443` (регистр важен); иное — skip `unsupported_value` «Unsupported VLESS flow value.» | D16, D26 |
| id не UUID (`vless://my-user@…`), UUID v6+ | skip | принимается; id приводится как в Xray (`uuid.ParseString`: строка ≤30 символов → UUIDv5, UUID — в нижний регистр; тег не зависит). Skip `unsupported_value` «The VLESS user id is not accepted by Xray.» — только если Xray не примет (31 символ, 37+, не-hex UUID) | D18, O4 |
| ws без `path`; REALITY без `sni`/`fp`/`sid` | skip `missing_required_field` | принимается; у REALITY обязателен только `pbk` | O15 |
| битая метка (`#100% fast`) | `URI malformed` — падала вся подписка | skip только этой строки: `invalid_uri` «The line label is not valid percent-encoding.» | O11 |
| всё, что отвергает `conf.Build` Xray | — | skip `unsupported_value` «Xray rejected the outbound configuration.», текст Xray — в `details` | D19, D26 |

Профили и теги. Старые профили и **теги их outbound'ов не меняются** (`vless-<cc>-<город>-<профиль>-<sha8(строки)>`): `tcp-tls` (в т.ч. tcp+tls+vision), `ws-tls`, `tcp-reality-vision`. Новые: `tcp-reality` (REALITY без flow), `xhttp-tls`, `xhttp-reality`, `xhttp-reality-vision` (только с ML-KEM), `tcp-mlkem`, `ws-mlkem`, `xhttp-mlkem` (`security=none` + ML-KEM). ML-KEM поверх tls/reality профиль не меняет. Если фильтры или правила маршрутизации завязаны на профиль в теге, дополните их новыми именами.

Пример: `vless://…@h:443?type=tcp&security=reality&pbk=…&sid=ab#🇩🇪 Berlin` — было skip «Unsupported VLESS type/security/flow combination.», стало outbound `vless-de-berlin-tcp-reality-<hash>`.

После обновления первый sync **полностью** переприменяет каждый target один раз (D11): `jsonOutbound` в манифесте — форма Xray v26.10.10, хэши меняются (теги старых профилей — нет). SOCKS-порты туннелей могут перераспределиться (D21) — они внутренние для scd; проверьте только ёмкость `portRange`.

### scd и sfc: User-Agent и заголовки загрузки

Без `fetchUserAgent` панель видит `User-Agent: scd/<версия>` (было `node`). Remnawave с правилами по умолчанию отдаёт на него base64 share-ссылок, как и раньше (`go-migration/12-real-subscriptions.md` §1, §10). Если задаёте `fetchUserAgent` явно:
- `Happ/…`, `INCY/…` и подобные «extended»-клиенты получают к меткам суффикс `?serverDescription=<base64>` — он попадает в город и тег outbound'а (и в метки, по которым фильтрует sfc);
- `clash*`, `mihomo`, `stash`, `sfa…`/`singbox…` получают YAML/JSON вместо share-ссылок — подписка даст 0 строк (scd: `produced no valid outbound entries`, sfc: выходы пустые).

`fetchHeaders` нужен, если панель включила лимит устройств (Remnawave HWID: без `x-hwid` отдаёт заглушки). Значение берите из окружения (`x-hwid: "${SCD_HWID}"`); переменная обязана существовать.

### sfc: какие строки попадают в выходы

Ключи (кроме фильтров) не менялись, но **содержимое выходов** `scd sfc` может стать больше (12 §6.2, D27 O7, D28, D18).

| Строка подписки | Было (TS-sfc) | Стало (Go) | Причина |
|---|---|---|---|
| параметры вне старого списка `type, path, security, alpn, encryption, fp, sni, flow, host, allowInsecure, headerType, pbk, sid` — xhttp `mode`/`extra`/`fm`, REALITY `spx`/`pqv`/`pcs`, `vcn`, `ech`, `cs`, `support-x25519mlkem768` и т.п. | строка **молча выбрасывалась** | строка попадает в выход без изменений | общий с scd список параметров `vless.AllowedParams` (D27 O7) |
| неизвестный параметр (`foo=1`) | строка выбрасывалась | попадает в выход; лог `debug` `subscription_line_param_ignored` | D28 |
| id — не UUID v1–5 (`vless://my-user@…`, UUID v6+) | выбрасывалась | попадает в выход | D18 |
| битая метка (`#100% fast`) | падала вся подписка (`URI malformed`), в т.ч. fallback из кэша | пропускается только эта строка | O11 |
| не-`vless://` строки, нет адреса/порта, неразбираемый URI | выбрасывались молча | без изменений | — |

sfc **не проверяет** применимость параметров к транспорту/шифрованию (это делает только scd): ссылки отдаются клиентам как есть. Строки `\r\n` и `\n` равнозначны; крайние пробелы контента обрезаются, как раньше.

Пример — обезличенная фикстура `docker/fixtures/real-shapes/remnawave.txt` (4 xhttp/ML-KEM-ссылки с `mode`/`extra`) с `docker/sfc/config.real-shape.go.yml`: выход `en-server` — **0 строк → 4** (было `Filtered output is empty.` и 502). На `provider2` выходы не меняются (50/0/0). Если клиенты не поддерживают xhttp/ML-KEM, отсеките такие строки `labelExcludeRegex` — sfc фильтрует только по метке.

Прочее видимое в `scd sfc refresh`/`serve`:
- отчёт `refresh`: `outputs` сортируются побайтно по `id` (`Z1` < `_a` < `a`; было ICU: `_a` < `a` < `Z1`) (D23); тексты ошибок загрузки — Go (`unable to download input from <url>: …`, `open <путь>: no such file or directory`) (D9);
- `serve` при старте обновляет выходы **один** раз (до запуска HTTP), следующее обновление — в первый слот `refreshSchedule` с логом `refresh_tick`; раньше сразу за начальным шёл второй, немедленный тик (O12);
- `serve` по SIGINT/SIGTERM останавливается сразу (D7): идущее обновление прерывается отменой загрузки, HTTP-запросы дорабатывают до 10 с; второй сигнал завершает процесс немедленно. Раньше процесс жил до следующего слота cron, и `docker stop` заканчивался SIGKILL. Ошибка bind `server.listen`, как и раньше, — после начального обновления;
- кэш источника — прежние файлы `<каталог конфига>/.sfc-cache/<encodeURIComponent(id)>.subscription`: volume подхватывается как есть; новые файлы — с правами `0600` (было `0666 & umask`).

## scd: status server, runtime-state и выбор балансера

Ключи `statusServer.*` (кроме `listen`, см. «Общее») не менялись. Схемы `/api/status`, `/api/instance-status`, `/api/instances` и `version: 1` сохранены (D3); новые поля — только аддитивно.

### scd: status server и JSON API (`statusServer`)

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| маршрутизация | точное сравнение URL **с query**: `/api/status?x=1` отдавал HTML | сравнивается только путь: `/api/status?x=1` → JSON; `/api/runtime-state*` — по префиксу пути | O8 |
| сортировка подписок, target'ов, туннелей (при равной latency), remote-инстансов | `localeCompare` (ICU) | побайтно (`strings.Compare`) | D23 |
| порядок ключей JSON | порядок вставки JS | порядок полей Go-структур; набор ключей и значения те же | D23 |
| исключение в обработчике | падение процесса | ответ 500 и лог `http_handler_panic`, сервер работает (так же в sfc и gtl) | D23 |
| ошибки сбора remote (`lastError`) | тексты Node (`fetch failed`, `The operation was aborted due to timeout`) | тексты Go (`net/http`, `context deadline exceeded`, ошибки JSON); `Remote status API returned HTTP <код>.` и тексты проверки схемы — без изменений | D9 |
| `instanceId` без `statusServer.instanceId` | `os.hostname()` | `os.Hostname()`, при ошибке — `localhost` | D9 |
| дашборд | — | визуально тот же (проверяется visual regression); при включённом `balancerSelection` — новая карточка (ниже) | D3 |

### scd: `/api/runtime-state` (`statusServer.runtimeState`)

Ключи `statusServer.runtimeState.includeRaw`/`includeSecrets` и умолчания (`false`) не менялись; меняется содержимое `runtime.*[].parsed` (03 §6.4, D6, D33).

| Поле `parsed` | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| outbound `path` (ws, xhttp, raw/tcp с `headerType=http`) | ws `path` не выводился (декодер читал не то поле), остальные — тоже | выводится; при `includeSecrets: false` — `"<redacted>"` (пустой path — ключа нет), при `true` — как есть (у raw `headerType=http` — пути через запятую) | D33: xhttp-путь реальных подписок — секрет, как URL подписки |
| outbound `host` | ws `host` не выводился | ws — как есть; xhttp — при `includeSecrets: false` `"<redacted>"` | D6, D33 |
| outbound `encryption` у ML-KEM | — (ссылки пропускались) | при `includeSecrets: false` — `"mlkem768x25519plus"` вместо ключа; при `true` — значение Xray (без префикса `mlkem768x25519plus.<режим>.<rtt>.`) | D26, D33 |
| outbound `fingerprint` (tls) | не выводился (читалось поле 10) | значение `fp` из ссылки | D6 |
| outbound `network` | `tcp`/`websocket` | плюс `splithttp` (xhttp) и новое поле `xhttpMode` | D26 |
| outbound `security`/`sni`/`fingerprint`/`alpn` при `security=none` (ML-KEM) | — | ключей нет | D26 |
| inbound `listen` | IPv6-адрес не выводился | IPv6 в записи Go (`::`, `::1`) | D9 |
| routing `inboundTags` | `[]` на реальном Xray | `[]` всегда (ListRule не возвращает inbound-теги) | 02 §1.5 |

Пример (`includeSecrets: false`, xhttp+tls+ML-KEM): `{"protocol":"vless","address":"srv.example.com","port":443,"encryption":"mlkem768x25519plus","network":"splithttp","security":"tls","sni":"…","alpn":["h2"],"fingerprint":"chrome","path":"<redacted>","xhttpMode":"stream-up"}`.

Остальные части ответа (03 §6.4–§6.5, D9, D15, D31, D33):

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `config.subscription.fetchHeaders` | — | имена заголовков со значением `"<redacted>"` при `includeSecrets: false`, реальные значения при `true` | D33 |
| `config.subscription.target` (копия target внутри подписки) | URL мониторинга печатались **без** редактирования | редактируются так же, как `config.target` | D33 |
| редактирование URL (`input`, `monitor.request.url`, `balancerMonitor.request.url`, `remotePing.url`) | `<origin>/<redacted>` (WHATWG: порт по умолчанию опускался, у схем без origin — `null/<redacted>`) | `<scheme>://<host>/<redacted>`, хост и порт как в исходной строке; без схемы/хоста — `<redacted>` | D9 |
| `rawBase64` при `includeRaw: true` | ключ всегда | ключ опускается, если значение пустое. **Внимание:** `rawBase64` — нередактированный protobuf (uuid, path, host, ключи) **независимо** от `includeSecrets` | D3, D33 |
| `expected` для файла | `buildManifest` + фильтры + validate | то же, что в sync: с политикой `allowInsecure`, тем же trim/фильтрами/validate; текст ошибки — Go | D9, D15 |
| набор ключей `config.subscription`/`config.target` | схема TS | схема Go (D31): + `allowInsecure`, `fetchUserAgent`, `target.balancerSelection`; ключи TS сохранены | D31 |
| routing `rawBase64` | сырой protobuf правила | protobuf `ListRuleItem` (outbound-тег и `ruleTag`) | 02 §1.5 |
| сортировки `runtime`/`diff` | `localeCompare` (ICU) | побайтно | D23 |

### scd: выбор балансера Xray (`target.balancerSelection`)

Новая функция (D22, `go-migration/10-balancer-selection.md` §6): scd показывает, какой outbound **сейчас** выбирает балансер Xray. При `enabled: false` (по умолчанию) JSON API и дашборд прежние (D3). Ключи — в таблице «[scd](#scd)».

| Что | Стало (Go) |
|---|---|
| Xray `api.services` | + `"ObservatoryService"` (`docker/xray/config/01_api.json`). ⚠️ `ObservatoryService` без секции `observatory`/`burstObservatory` **роняет старт Xray** (`core: not all dependencies are resolved.`); у нас она в `05_observatory.json`. Без сервиса функция работает, но живость кандидатов не видна (`observatoryError`: `Xray API service ObservatoryService is not enabled (add it to api.services).`) |
| опрос | отдельный cron-цикл на target: read-only `GetBalancerInfo` + `GetOutboundStatus` параллельно, **без** target-lock; работает и до первого sync (сопоставление с туннелями — после sync). `OverrideBalancerTarget` scd не вызывает |
| `/api/instance-status` `targets[]` | новый ключ `balancerSelection` (после `balancerMonitor`): `balancerTag, state (idle\|ok\|error), mode (override\|single\|pool\|none\|unknown), strategy?, selectedOutboundTag?, selectedTunnelId?, selectedDisplayName?, selectedCountryIso2?, selectedManaged?, selectedDelayMs?, overrideTarget?, principleTargets?, selectedSince?, lastCheckedAt?, lastSuccessAt?, lastFailureAt?, lastError?, observatoryError?`; при ошибке сохраняется выбор прошлого успеха |
| туннели `/api/status`, `/api/instance-status` | после `balancerMonitor*`: `outboundTag, balancerSelected, balancerCandidate, observatoryStatus (alive\|dead\|awaiting\|n/a), observatoryAlive?, observatoryDelayMs?, observatoryLastError?, observatoryLastSeenAt?, observatoryLastTryAt?, observatoryHealthPing{All,Fail,AverageMs,DeviationMs}?` (после первого опроса; только по текущему тегу туннеля) |
| `/api/runtime-state` | `serviceState.balancerSelection`: то же + `previousSelectedTag`, `consecutiveFailures`, `observatory[]`, `observatoryCheckedAt` |
| `version` снапшота | `1` (поля аддитивные; старые агрегаторы пропускают их, новые принимают снапшоты без них) |
| дашборд | карточка «Xray selection · {balancerTag}» после «External SOCKS check»: `Selected` (туннель / сырой тег / `override → tag` / `pool (n) · strategy` / `none (fallback/default outbound)`), `Latency`, `Since`, `Observatory`, `Error`; degraded — ошибка, `none`, override вне туннелей target'а, выбранный dead. У туннелей — рамка и бейдж `Selected`, бейдж `Pool`, метрика `Observatory`. Repaired-туннели балансер выбрать не может (без префикса они вне selector'а) |

Пример — «[scd: примеры было → стало](#scd-примеры-было--стало)» и `docker/scd/config.example.yml`.

## Запуск

Одна команда `scd …`, один образ, `command:` в docker-compose для sfc/gtl (D10).

### Образ и docker-compose

Ключи конфигов не меняются; меняются бинарь, образ и команда запуска.

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| команды | три CLI: `node dist/cli.js …` в каждом образе | один бинарь: `scd sync\|daemon\|generate\|validate-config\|print-config`, `scd sfc serve\|refresh\|validate-config\|print-config`, `scd gtl serve\|validate-config\|print-config` | D10 |
| образы | `ghcr.io/a1ekseev/scd`, `…/sfc`, `…/gtl` | только `ghcr.io/a1ekseev/scd` (amd64+arm64); `sfc`/`gtl` **больше не публикуются** — их последние теги остаются TS-версией | D10 |
| `CMD` образа | `["node","dist/cli.js","daemon","--config","/app/config/config.yml"]` (`sfc`/`gtl`: `… "serve" …`) | `["scd","daemon","--config","/app/config/config.yml"]` | D10 |
| `command:` sfc / gtl | не задавался (CMD своего образа) | `["scd","sfc","serve","--config","/app/config/config.yml"]` / `["scd","gtl","serve","--config","/app/config/config.yml"]` на образе `ghcr.io/a1ekseev/scd` | D10 |
| свой `command:` вида `node dist/cli.js …` | работал | **не работает** (в образе нет Node) — заменить на `scd …`/`scd sfc …`/`scd gtl …` с теми же аргументами | D10 |
| база образа | `node:24-alpine` (есть shell) | `gcr.io/distroless/static-debian13` (CA-сертификаты, tzdata; без shell — `docker exec … sh` недоступен), пользователь root | D1 |
| путь конфига, порт, кэш sfc | `/app/config/config.yml`, `8080`, volume `/app/config/.sfc-cache` | без изменений | — |
| тестовый Xray в compose | `ghcr.io/xtls/xray-core:26.3.27` | `ghcr.io/xtls/xray-core:26.10.10` (версия в `.xray-version`, сверяется с `go.mod`: `scripts/check-xray-version.sh`) | D4 |

```diff
   sfc:
-    image: ghcr.io/a1ekseev/sfc:latest
+    image: ghcr.io/a1ekseev/scd:latest
+    command: ["scd", "sfc", "serve", "--config", "/app/config/config.yml"]
     volumes:
       - ./docker/sfc/config.yml:/app/config/config.yml:ro
       - ./docker/sfc/.sfc-cache:/app/config/.sfc-cache
```

У `scd` достаточно сменить тег; volumes и порты не трогать. Перед переключением: `docker run --rm -v "$PWD/docker/sfc/config.yml":/app/config/config.yml:ro ghcr.io/a1ekseev/scd:<версия> scd sfc validate-config --config /app/config/config.yml` (аналогично `scd validate-config` и `scd gtl validate-config`).

### scd: `scd daemon`

Ключи не менялись (`runtime.mode: daemon` + `runtime.schedule`, расписания `monitor`/`balancerMonitor`/`balancerSelection`/`remoteInstances[]`); отличия в поведении процесса:

| Что | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| остановка по SIGINT/SIGTERM | каждый цикл ждал свой следующий слот; status server закрывался после выхода *первого* цикла; на практике `docker stop` добивал процесс SIGKILL через 10 с | все циклы выходят сразу; идущие тики (в т.ч. apply/repair/rejoin в Xray) доводятся до конца, затем дожидаются отправленные remote ping (не дольше их `timeoutMs`), затем status server завершает запросы (до 10 с), выход с кодом 0; пока идёт дренаж, status server отвечает | D7 |
| второй сигнал во время остановки | немедленное завершение | так же | D7 |
| тик monitor/balancerMonitor/balancerSelection, прерванный остановкой | — | ничего не записывает (ни degraded, ни repair, ни push) и не логирует `*_tick_failed`; sync, прерванный во время загрузки подписки, отмечает её failed (apply в Xray не прерывается) | D7 |
| цикл `balancerSelection` | — | на каждую подписку с `balancerSelection.enabled` и `schedule` (как и monitor-циклы — **включая** `enabled: false` подписки, как в TS) | D22 |
| tunnel monitor и `balancerMonitor` до первого sync | не работали (состояния target'а ещё не было) | так же: тики ничего не делают, пока sync не дошёл до target'а (даже неуспешный); `balancerSelection` работает и до sync | 03 §1, 10 §6.3 |
| `remotePing` без `viaSocks` | прямой запрос без прокси | прямой запрос учитывает `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` окружения (стандартный транспорт Go) — если в окружении контейнера задан прокси, push пойдёт через него | D9 |
| SOCKS5-проба (monitor, balancerMonitor, `viaSocks`) к IPv6-адресу | IPv6-литерал уходил как домен (ATYP 3, `[::1]`) | ATYP 4 + 16 байт (RFC 1928); домен длиннее 255 байт — ошибка `invalid URL`; один дедлайн `request.timeoutMs` на всю проверку, latency — до заголовков ответа | D9, D23 |
| конфиг не в режиме daemon | `Config at <path> is not in daemon mode.` | `config at <path> is not in daemon mode`, код 1 | D9 |
| ошибка bind status server | исключение, выход 1 | `listen on <addr>: …`, код 1; циклы не запускаются | — |

Известное ограничение, сохранённое намеренно (O5, D2): статическое правило маршрутизации **без** `ruleTag` нельзя перечислить в `fixedRouting`, scd удаляет его вызовом `RemoveRule("")`, Xray отвечает `app/router: empty tag name!` — routing-apply этого target'а падает, как и в TS. Дайте каждому статическому правилу `ruleTag` и добавьте его в `fixedRouting`.

## Логи

Формат и поля логов (D27, D9).

- **Куда.** Все три сервиса пишут логи только в **stderr**; stdout занят JSON-выводом команд. Было: pino в stdout (по-разному у scd/sfc/gtl).
- **Формат** — `logging.format`: `json` или `pretty`.
  - `json` = `slog` JSON: `time` в RFC3339 (было: pino `time` в мс), `level` строкой `INFO`/`WARN`/`ERROR`/`DEBUG`/`TRACE`/`FATAL` (было: числовой), `msg`, далее `event` и остальные поля. Имена событий (`event`) и поля сохранены.
  - `pretty` = `slog.TextHandler`: `time=… level=INFO msg="…" event=… key=value`; вложенные значения (`subscriptions`, `failedItems`, `errorMessages`, `causes`) — JSON-строкой, напр. `causes="[\"first\",\"second\"]"` (было: многострочный блок pino-pretty).
- **Уровни** прежние: `fatal`, `error`, `warn`, `info`, `debug`, `trace`, `silent`.
- Пример `json`: `{"time":"2026-10-17T12:00:00.123456789Z","level":"INFO","msg":"Sync finished.","event":"sync_finished"}`.
- Фильтры Loki/jq по числовому `level` или `time` в мс нужно переписать на строковый `level` и RFC3339 `time`.
- URL подписок в логах — `<scheme>://<host>/<redacted>`, значения `fetchHeaders` — `<redacted>` (D33).

Все события TS сохранены. Новые события Go-версии:

| `event` | Уровень | Сервис | Когда, поля |
|---|---|---|---|
| `manifest_entry_warning` | `warn` (`flow_cleared`, `allow_insecure_ignored`), `debug` (`param_ignored`) | scd sync | предупреждение у entry манифеста: `subscriptionId, targetAddress, tag, code, message` |
| `balancer_selection_changed` | `info` | scd daemon | сменился выбор балансера: `from`, `to`, `mode` |
| `balancer_selection_failed` | `warn` | scd daemon | ошибка `GetBalancerInfo` (только при новом тексте ошибки): `error` |
| `balancer_selection_recovered` | `info` | scd daemon | опрос снова успешен |
| `balancer_observatory_unavailable` | `warn` | scd daemon | ошибка `GetOutboundStatus` (один раз на текст) |
| `balancer_selection_tick_skipped_overrun` | `warn` | scd daemon | «Balancer selection tick skipped because previous run is still in progress.» |
| `balancer_selection_tick_failed` | `error` | scd daemon | «Balancer selection tick failed.» |
| `daemon_stopping` | `info` | scd daemon | «Daemon stopping.», `signal`: `SIGINT`/`SIGTERM` |
| `http_handler_panic` | `error` | scd, sfc, gtl | паника в HTTP-обработчике (ответ 500) |
| `subscription_line_param_ignored` | `debug` | sfc | неизвестные параметры строки: `subscriptionId`, `line` (номер), `params` |

У событий `balancer_selection_*` и `balancer_observatory_unavailable` — `subscriptionId`, `targetAddress`, `balancerTag`.

## Разобранные примеры

Боевые конфиги `docker/{scd,sfc,gtl}/config.real-shape.yml` (TS-форма, обезличены) → Go-формы `config.real-shape.go.yml`. Go-формы проверяются в CI-тестах (`TestSCDRealShapeGoFormValid`, `TestSFCRealShapeGoFormValid`) и в чек-листе ниже; TS-формы Go-бинарь не принимает (они нужны только как «было»).

### scd: `docker/scd/config.real-shape.yml` → `docker/scd/config.real-shape.go.yml`

TS-форма не проходит `scd validate-config`: фильтр меток обеих подписок построен на lookahead —

```
invalid config docker/scd/config.real-shape.yml:
  - subscriptions.0.filters.labelIncludeRegex: lookahead, lookbehind and backreferences are not supported (RE2 syntax): error parsing regexp: invalid or unsupported Perl syntax: `(?!`
  - subscriptions.1.filters.labelIncludeRegex: lookahead, … `(?!`
```

Шаг 1 — `blanc-en` и `blanc-ru` (всё, кроме Extra и WhiteList): «нет ни Extra, ни WhiteList» = исключение по любому из них (строка 1 таблицы «Фильтры меток»). `countryAllowlist` не меняется.

```diff
     filters:
       countryAllowlist: [AT, BE, BG, CH, CZ, DE, DK, ES, FI, FR, GB, GR, HR, HU, IE, IT, LT, NL, "NO", PL, PT, RO, SK, TR]
-      labelIncludeRegex: '/^(?!.*Extra)(?!.*WhiteList).*$/i'
+      labelExcludeRegex: '/Extra|WhiteList/i'
```

Шаг 2 — остальное без изменений. Прочие ключи (`target.*` с `inboundSocks`, `monitor`, `balancerMonitor.socks5`, `remotePing.viaSocks: true`, `observatorySubjectSelectorPrefix`, `visionUdp443Override`; `runtime: daemon` с `"*/5 * * * *"`; `statusServer` с `instanceId`, `listen: 172.31.0.8:8080`, `runtimeState`) валидны как есть. Расписания `*/1 * * * *` и `*/5 * * * *` — обычный crontab.

Шаг 3 (необязательно) — новые ключи; их умолчания сохраняют прежнее поведение, кроме двух мест:
- `allowInsecure` — по умолчанию `deny`: ссылки с `allowInsecure=1` без `pcs`/`vcn` теперь пропускаются. Если провайдер отдаёт такие ссылки и они нужны — `allowInsecure: allow`.
- `fetchUserAgent` — без ключа панель увидит `User-Agent: scd/<версия>` вместо `node`; если панель выдаёт разный контент по User-Agent, задайте его явно (см. предупреждение в «Подписки»).
- `fetchHeaders` (например, `x-hwid: "${SCD_HWID}"`), `balancerSelection` (например `{enabled: true, balancerTag: x-balancer, schedule: "*/15 * * * * *"}`; префикс `x-observe-` уже задан, предупреждения не будет).

Проверка: `scd validate-config --config docker/scd/config.real-shape.go.yml` → `"valid": true`, stderr пуст.

### sfc: `docker/sfc/config.real-shape.yml` → `docker/sfc/config.real-shape.go.yml`

TS-форма не проходит `scd sfc validate-config`: все три фильтра построены на lookahead —

```
invalid config docker/sfc/config.real-shape.yml:
  - subscriptions.0.outputs.0.labelIncludeRegex: lookahead, lookbehind and backreferences are not supported (RE2 syntax): error parsing regexp: invalid or unsupported Perl syntax: `(?!`
  - subscriptions.0.outputs.1.labelIncludeRegex: lookahead, … `(?=`
  - subscriptions.0.outputs.2.labelIncludeRegex: lookahead, … `(?=`
```

Шаг 1 — `en-server` (всё, кроме Extra и WhiteList):

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

Шаг 3 — `extra-whitelist` (и Extra, и WhiteList, в любом порядке): список include.

```diff
-        labelIncludeRegex: '/(?=.*Extra)(?=.*WhiteList)/i'
+        labelIncludeRegex: ['/Extra/i', '/WhiteList/i']
```

Остальные ключи (`input`, `format`, `fetchTimeoutMs: 15000`, `pathRoute`, `userAgent: [Happ]`, `profileTitle`, `profileUpdateInterval: 1`, `refreshSchedule: "15 * * * *"`, `server.listen: 172.31.3.4:8080`, `logging`) не меняются. Новые ключи необязательны: `fetchUserAgent` (иначе панель увидит `scd/<версия>` вместо `node`) и `fetchHeaders` (например, `x-hwid: "${SFC_HWID}"`). `userAgent: [Happ]` у выхода — это User-Agent **клиентов sfc** для заголовков профиля, он не связан с `fetchUserAgent`. Проверка: `scd sfc validate-config --config docker/sfc/config.real-shape.go.yml` → `"valid": true`.

### gtl: `docker/gtl/config.real-shape.yml` → `docker/gtl/config.real-shape.go.yml`

Ключи gtl не менялись, содержимое совпадает (отличается только комментарий-шапка); `server.listen: 172.31.3.5:8080` валиден. Меняется только запуск — `command:` из «[Запуск](#запуск)». Проверка: `scd gtl validate-config --config docker/gtl/config.real-shape.go.yml` → `Config is valid`.

## Чек-лист

1. Собрать бинарь (`go build -o bin/scd ./cmd/scd`) или взять образ `ghcr.io/a1ekseev/scd:<версия>`.
2. На **новом** конфиге каждого сервиса (код выхода 0):
   - `scd validate-config --config docker/scd/config.yml` — `"valid": true`; прочитать `warning:` в stderr, если есть;
   - `scd sfc validate-config --config docker/sfc/config.yml` — `"valid": true`;
   - `scd gtl validate-config --config docker/gtl/config.yml` — `Config is valid`.
3. `scd print-config` / `scd sfc print-config` — проверить умолчания новых ключей (`allowInsecure`, `balancerSelection`) и что `input`, URL мониторов/`remotePing` и `fetchHeaders` скрыты (`<redacted>`); сырой вид — `--include-secrets`.
4. `scd generate --input <снимок подписки> --output m.json --log log.json` — сравнить `skipped`/`warnings` и число entries с ожидаемым; `scd sfc refresh --config …` — число строк в выходах.
5. Обновить compose (`image:` + `command:`), Xray-образ — `ghcr.io/xtls/xray-core:26.10.10` с `ObservatoryService` только при наличии секции `observatory`.
6. После запуска: первый sync переприменяет все target'ы (ожидаемо), логи — в stderr в новом формате.
