# 04 — sfc (Subscription Filter Control): спецификация для переноса на Go

Источник: `sfc/` на ветке `move-to-go` (package.json `version: 1.0.11`). Все ссылки вида `file:line` даны относительно `sfc/`, если не указано иное.
Поведение, помеченное **[проверено]**, подтверждено прогоном оригинального TS-кода (Node 22 + те же зависимости из `package-lock.json`) в scratch-окружении.

---

## 1. Назначение

`sfc` — самостоятельный сервис (не общается с Xray):

1. Загружает upstream-подписки (файл, `http(s)` URL или stdin) по cron-расписанию.
2. Декодирует base64 при необходимости, разбивает на строки, оставляет только **валидные VLESS-строки** (тот же парсер, что в `scd`).
3. Для каждого `output` фильтрует строки по regex на `label` (фрагмент URI после `#`), сохраняет **исходные raw-строки в исходном порядке**.
4. Склеивает их через `\n`, кодирует в base64 и держит в памяти «последнее хорошее» значение.
5. Отдаёт его по HTTP `GET <pathRoute>/<output-id>`; никогда не ходит в upstream на запрос.
6. Сохраняет последний успешно загруженный (уже декодированный) source в `.sfc-cache/` рядом с конфигом и использует его как fallback при ошибке загрузки.

Нет: админки, авторизации, country-фильтра, статус-эндпоинта (`AGENTS.md:34-38`).

---

## 2. CLI

Реализация: `src/cli.ts` (commander v14). Бинарь `sfc` (`package.json:9-11`). В Go предлагается `blanc sfc <cmd>`.

| Команда | Флаги | Поведение | stdout | Exit code |
|---|---|---|---|---|
| `serve` | `--config <config>` (обяз.) | `runServerCommand` (`cli.ts:16-21`, `runtime/run-server.ts:139-171`) | — (логи в stderr) | 0 при штатном завершении; 1 при ошибке (конфиг, bind) |
| `refresh` | `--config <config>` (обяз.) | Один цикл refresh (`cli.ts:23-33`), с записью/чтением disk-cache | `JSON.stringify(report, null, 2) + "\n"` | `1`, если `report.failed > 0`, иначе `0` (`cli.ts:32`) |
| `validate-config` | `--config <config>` | `loadConfig` (`cli.ts:35-50`) | `{"configPath": "<abs>", "valid": true}` (2-space indent) | 0 / 1 |
| `print-config` | `--config <config>` | `cli.ts:52-58` | нормализованный конфиг (после env-интерполяции, zod-дефолтов и резолва путей), 2-space indent | 0 / 1 |

Детали:
- Флаг принимается как `--config path` и `--config=path` (commander).
- Ошибки, выброшенные из action: в stderr пишется **только `error.message`** (без stack) + `\n`, `exitCode = 1` (`cli.ts:60-63`).
- Ошибки самого commander (нет `--config`, неизвестная команда) → `error: required option '--config <config>' not specified` / `error: unknown command 'nope'`, затем (из-за `.showHelpAfterError()`, `cli.ts:14`) пустая строка и help соответствующей команды, exit 1 **[проверено]**.
- Без команды → help в stderr, exit 1. `--help`/`-h` → help, exit 0. Опции `--version` **нет** (в отличие от gtl).
- Help-текст: `Usage: sfc [options] [command]` / описание `Filter upstream subscriptions and serve them by public URL.` / команды без описаний.
- В `refresh` логи (pino JSON) идут в **stderr**, отчёт — в **stdout**; порядок строк в терминале может перемешиваться.

### 2.1 Формат отчёта `refresh` (`types.ts:99-117`, `runtime/refresh.ts:266-273`)

Порядок ключей (важно для байтовой совместимости JSON):
```json
{
  "refreshedAt": "2026-10-17T08:33:35.167Z",
  "sourceCount": 2,
  "outputCount": 3,
  "successful": 1,
  "failed": 2,
  "outputs": [
    { "id": "bb", "subscriptionId": "broken", "matchedLines": 0, "ok": false, "usedCachedValue": false, "error": "fetch failed" },
    { "id": "de", "subscriptionId": "main", "matchedLines": 1, "ok": true, "usedCachedValue": false },
    { "id": "none", "subscriptionId": "main", "name": "nothing", "matchedLines": 0, "ok": false, "usedCachedValue": false, "error": "Filtered output is empty." }
  ]
}
```
- Порядок ключей output-report: `id, subscriptionId, name?, matchedLines, ok, usedCachedValue, usedSourceCache?, error?`. Поля со значением `undefined` опускаются (`name` — если не задан; `usedSourceCache` — присутствует **только** когда `true`; `error` — только при `ok:false`).
- `refreshedAt` = `lastRefreshFinishedAt`, ISO-8601 UTC с миллисекундами и `Z` (`Date.toISOString()`). Go: `t.UTC().Format("2006-01-02T15:04:05.000Z")`.
- `sourceCount` = число **enabled** subscriptions; `outputCount` = число отчётов (enabled outputs enabled subscriptions).
- `outputs` отсортированы `left.id.localeCompare(right.id)` (`refresh.ts:272`) — это **ICU-коллация**, не байтовый порядок. **[проверено]** порядок: `["_a","-a","10","9","a","A","b","B","e","é","f","out_x","out-europe","out-germany","out.x","Z1","z10","z2"]`. Go: `golang.org/x/text/collate` (`language.Und`) — проверить на этом векторе.
- Go JSON: `json.Encoder` с `SetEscapeHTML(false)` и `SetIndent("", "  ")`; иначе `<>&` будут экранированы, в отличие от `JSON.stringify`. (U+2028/2029 Go экранирует всегда — расхождение только для экзотики.)

---

## 3. Конфигурация

Загрузка: `src/config/load-config.ts:221-235`. Порядок:
1. `resolve(configPath)` → абсолютный путь относительно CWD (`:222`).
2. `readFile(utf8)`; ошибка ENOENT пробрасывается как есть: `ENOENT: no such file or directory, open '/abs/path'` **[проверено]**.
3. `yaml.parse` (eemeli/yaml v2, YAML 1.2 core schema). Ошибки YAML пробрасываются как есть (например `Map keys must be unique at line 2, column 1:\n\n...`) — дубликаты ключей = ошибка.
4. `interpolateDeep` — env-подстановка **после** парсинга YAML, только в **строковых значениях** (рекурсивно по массивам/объектам; ключи не трогаются) (`:189-205`).
5. zod-валидация + дефолты; ошибка → `ConfigError` с сообщениями issue, соединёнными `\n` (без путей!) (`:226-229`).
6. `resolveConfigPaths` — резолв `subscriptions[].input` (`:207-219`).

### 3.1 Env-интерполяция (`load-config.ts:185-187`)
- Регекс `/\$\{([A-Z0-9_]+)\}/gi` — имя переменной из `[A-Za-z0-9_]+` (флаг `i`!).
- Отсутствующая переменная → **пустая строка** (не ошибка). Отличие от scd, где отсутствующая переменная — `ConfigError` и подстановка идёт по сырому тексту YAML.
- Т.к. подстановка после парсинга, `fetchTimeoutMs: ${TM}` → строка → ошибка `Invalid input: expected number, received string` **[проверено]**. Числа/булевы из env задать нельзя.
- Синтаксиса `$VAR`, `${VAR:-default}`, экранирования нет.

### 3.2 Схема

Неизвестные ключи **молча отбрасываются** (zod `z.object` без `.strict()`), в отличие от gtl.

| Путь | Тип | Обяз. | Default | Нормализация / валидация | Ссылка |
|---|---|---|---|---|---|
| `subscriptions` | array | да | — | `min(1)` | `:92` |
| `subscriptions[].id` | string | да | — | trim, `min(1)`; уникален среди **всех** subscriptions (включая disabled) | `:82`, `:122-134` |
| `subscriptions[].input` | string | да | — | trim, `min(1)`; `-` = stdin; `^https?://` (case-insens.) = URL; иначе путь (см. 3.3) | `:83` |
| `subscriptions[].enabled` | boolean | нет | `true` | строго YAML-bool (`yes` → ошибка `expected boolean, received string`) | `:84` |
| `subscriptions[].format` | enum `auto\|plain\|base64` | нет | `auto` | | `:85` |
| `subscriptions[].fetchTimeoutMs` | number | нет | `5000` | `int`, `>0`. `5000.0` проходит (JS-число целое) | `:86` |
| `subscriptions[].pathRoute` | string | да | — | trim, `min(1)`; начинается с `/`, не равен `/`, не заканчивается на `/` (проверка в app-level superRefine) | `:87`, `:49-59`, `:136-146` |
| `subscriptions[].outputs` | array | да | — | `min(1)` | `:88` |
| `outputs[].id` | string | да | — | trim, `min(1)`; **глобально** уникален (по всем subscriptions, включая disabled) | `:62`, `:148-162` |
| `outputs[].enabled` | boolean | нет | `true` | | `:63` |
| `outputs[].name` | string | нет | — | trim; пустая/пробельная строка → `undefined` (ключ исчезает) | `:11-20`, `:64` |
| `outputs[].labelIncludeRegex` | string | да | — | trim, `min(1)`; JS regex literal `/pattern/flags` (см. 4.4) | `:65`, `:69-79` |
| `outputs[].userAgent` | string[] | нет | — | каждый элемент trim+`min(1)`; массив `min(1)` | `:66` |
| `outputs[].profileTitle` | string | нет | — | trim; пустая → `undefined` | `:67` |
| `outputs[].profileUpdateInterval` | number | нет | — | `int`, `1..24` | `:68` |
| `runtime.refreshSchedule` | string | да | — | trim (без `min(1)`!); должен парситься `cron-parser` | `:94`, `:164-172` |
| `server.listen` | string | да | — | trim, `min(1)`; `host:port` (см. ниже) | `:97`, `:37-47`, `:174-182` |
| `logging` | object | нет | `{level:info, format:json}` | | `:99-102` |
| `logging.level` | enum `fatal\|error\|warn\|info\|debug\|trace\|silent` | нет | `info` | | `:100` |
| `logging.format` | enum `json\|pretty` | нет | `json` | | `:101` |

App-level правила (`:103-183`), порядок выдачи сообщений:
1. `At least one subscription must be enabled.`
2. `At least one output must be enabled.` (enabled outputs внутри enabled subscriptions)
3. `Duplicate subscription id "<id>" is not allowed.`
4. для каждой subscription: `pathRoute must start with "/".` / `pathRoute must not be "/".` / `pathRoute must not end with "/".` (первая сработавшая)
5. `Duplicate output id "<id>" is not allowed.`
6. ошибка cron-parser (текст исключения, например `Constraint error, got value 61 expected range 0-59`)
7. `Listen address must be in "host:port" format.` / `Listen address port must be an integer between 1 and 65535.`

**Семантика zod v4, которую надо повторить** **[проверено]**: app-level `superRefine` **не выполняется**, если есть «структурные» ошибки (тип/обязательность/min/max/enum) в дочерних полях; но выполняется, если дочерние ошибки — только custom-issues от output-level `superRefine` (невалидный regex). Пример: `/broken` + дубль id → `Regex literal must end with "/flags".\nDuplicate output id "dup" is not allowed.`; отсутствие `pathRoute` → только `Invalid input: expected string, received undefined`.

Тексты zod v4, встречающиеся в тестах и на практике:
- `Invalid input: expected string, received undefined` / `... expected object, received null` (пустой файл) / `... expected number, received string` / `... expected boolean, received string`
- `Too small: expected string to have >=1 characters`, `Too small: expected array to have >=1 items`, `Too big: expected number to be <=24`, `Too small: expected number to be >=1`
- enum: `Invalid option: expected one of "auto"|"plain"|"base64"`

`server.listen` (`:37-47`): `sep = lastIndexOf(':')`; ошибка если `sep <= 0` или `sep == len-1`; порт = `Number(rest)` — **JS `Number()`**: принимает `0x50`, `1e3`, `+80`, ` 80`, `080` **[проверено: `h:0x50` валиден]**; должен быть целым `1..65535`. Хост не валидируется. Go: либо эмулировать `Number()`, либо осознанно сузить до десятичного (зафиксировать как изменение).

### 3.3 Резолв путей (`load-config.ts:207-219`, `input/load-input.ts:125`)
- `input == "-"` или `/^https?:\/\//i` или абсолютный путь → как есть; иначе `path.resolve(dirname(absConfigPath), input)`.
- `print-config` показывает уже абсолютный путь.
- `file:///x` трактуется как относительный путь (`<configDir>/file:/x`).
- Disk-cache: `<dirname(absConfigPath)>/.sfc-cache/` (см. 5.4).

### 3.4 Cron (`cron-parser` v5) **[проверено]**
Валидация: `CronExpressionParser.parse(schedule)`; вычисление следующего срабатывания: `parse(schedule, {currentDate}).next()` (`run-server.ts:15-17`) — **строго после** `currentDate`.
- 5 полей (секунда = 0) или 6 полей (первое — секунды). 7 полей → `Invalid cron expression, too many fields`.
- Пустая строка `""` **валидна** и означает «каждую минуту» (`08:22:33 → 08:23:00, 08:24:00`).
- Алиасы: `@yearly @annually @monthly @weekly @daily @hourly @minutely @secondly`; `@midnight` — ошибка.
- Поддерживаются `L` (DOM и `5L` в DOW), `?`, `#` (`1#2`), имена месяцев/дней (`JAN`, `MON`), `7` = воскресенье, лишние пробелы. `W` — ошибка. `H` — принимается и даёт **случайное** значение при каждом parse (каждый `getNextCronDate` перепарсивает!).
- Если заданы и DOM, и DOW — OR-семантика (`0 0 1 * 1` = 1-е число ИЛИ понедельник).
- Невалидные: `*/0` → `Constraint error, cannot repeat at every 0 time.`; `5-3` → `Invalid range: 5-3, min(5) > max(3)`; `0 0 31 2 *` → `Invalid explicit day of month definition`; `hello` → `Constraint error, got value 0 expected range 1-12`.
- Часовой пояс — локальный TZ процесса (в `node:24-alpine` = UTC). `TZ=Europe/Moscow` + `0 3 * * *` → `00:00Z`.
- Go: `robfig/cron/v3` с `cron.NewParser(SecondOptional|Minute|Hour|Dom|Month|Dow|Descriptor)` покрывает основное, но **не** `L`, `#`, `?`-как-у-cron-parser, `@minutely/@secondly`, `""`. Нужна обёртка: `"" → "* * * * *"`, явное отклонение/реализация `L`/`#`, либо собственный парсер. Тексты ошибок будут отличаться — решить, важно ли.

### 3.5 Пример конфига (README.md:70-102)
```yaml
subscriptions:
  - id: main
    input: ../vpn
    enabled: true
    format: auto
    fetchTimeoutMs: 5000
    pathRoute: /s
    outputs:
      - id: a8f3c9d2
        enabled: true
        name: germany-extra
        labelIncludeRegex: '/Германия.*Extra$/i'
        userAgent: [Clash, Stash]
        profileTitle: Germany Extra
        profileUpdateInterval: 6
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 0.0.0.0:8081
logging:
  level: info
  format: json
```

`print-config` порядок ключей (порядок схемы, не входного файла): top: `subscriptions, runtime, server, logging`; subscription: `id, input, enabled, format, fetchTimeoutMs, pathRoute, outputs`; output: `id, enabled, name?, labelIncludeRegex, userAgent?, profileTitle?, profileUpdateInterval?` **[проверено]**.

---

## 4. Алгоритмы

### 4.1 Загрузка input (`src/input/load-input.ts:104-133`)
Три ветки, общий шаблон: `raw → ensureNonEmpty(raw) → applyInputFormat → ensureNonEmpty(content)`.
- `ensureNonEmptyContent` (`:12-17`): если `content.trim()` пуст → `InputError('EMPTY_INPUT', "Input source <source> returned empty content.")`. `<source>` = `stdin` / URL / абсолютный путь.
- **stdin** (`-`): читает `process.stdin` до EOF, UTF-8 (`:44-50`). `source: 'stdin'`. При повторном refresh в `serve` stdin уже исчерпан → `EMPTY_INPUT` → fallback на disk-cache.
- **HTTP** (`:19-42`): `fetch(url, {redirect:'follow', signal})`; таймаут через `AbortController` на `fetchTimeoutMs` (default 5000), покрывает **и заголовки, и чтение тела**. Ошибки:
  - не-2xx: `Unable to download input from <url>: <status> <statusText>` — `statusText` = reason-phrase сервера как есть (`404 Nope Custom`) **[проверено]**.
  - таймаут: `Input source <url> timed out after <ms>ms.`
  - сетевые ошибки: undici бросает `TypeError("fetch failed")` — в лог/отчёт попадает **только** `fetch failed` (cause теряется) **[проверено]**.
  - Заголовки запроса undici **[проверено]**: `accept: */*`, `accept-language: *`, `sec-fetch-mode: cors`, `user-agent: node`, `accept-encoding: gzip, deflate`, `connection: keep-alive`. Многие панели подписок выдают разный контент по User-Agent — в Go **явно ставить `User-Agent: node`** (и `Accept: */*`, `Accept-Language: *`), иначе будет `Go-http-client/1.1`.
  - `response.text()` — всегда UTF-8 (charset игнорируется), **BOM срезается**, невалидные байты → U+FFFD.
  - редиректы: follow (undici максимум 20). Go `http.Client` — 10 по умолчанию.
- **Файл**: `readFile(resolve(input), 'utf8')` — BOM **не** срезается (позже его съест `trim()`), ENOENT-сообщение Node: `ENOENT: no such file or directory, open '<path>'`.

### 4.2 Декодирование (`load-input.ts:56-102`)
`normalizeBase64(v)` = удалить все JS-`\s`, trim, `-`→`+`, `_`→`/`.

`applyInputFormat(raw, format)`:
- `plain` → как есть.
- `base64` → `Buffer.from(normalizeBase64(raw), 'base64').toString('utf8')` — без проверки результата.
- `auto` → `decodeSubscriptionContent(raw)` (`:60-81`):
  1. `t = raw.trim()`; если пусто **или содержит `://`** → вернуть `raw` (plain).
  2. `n = normalizeBase64(t)`; если пусто или есть символ вне `[A-Za-z0-9+/=]` → `raw`.
  3. `d = base64decode(n)` как UTF-8; если `d` содержит `://` → `d`, иначе `raw`.
  - `encoding` = `content === raw ? 'plain' : 'base64'` (в sfc нигде не используется).

**Семантика `Buffer.from(s,'base64')` Node, которую нужно воспроизвести байт-в-байт** **[проверено]** (Go `encoding/base64` строгий — нужен свой декодер):
- принимает оба алфавита (`+/` и `-_`);
- **пропускает** любые символы вне алфавита (пробелы, `*`, …): `"YW*Jj"→"abc"`;
- **останавливается на первом `=`**: `"YWJj=ZGVm"→"abc"`, `"YQ==YQ"→"a"`, `"=YQ"→""`, `"YW=JjZGVm"→"a"`;
- паддинг не обязателен: `"YQ"→"a"`, `"YWJjZA"→"abcd"`;
- хвост из 1 символа отбрасывается: `"Y"→""`, `"YWJjZGVmZ"→"abcdef"`.
- Результат → UTF-8 с заменой невалидных последовательностей на U+FFFD (`[0xff]+"://a"` → `"�://a"`). Go: `strings.ToValidUTF8` склеивает подряд идущие невалидные байты в один U+FFFD, а WHATWG-декодер — по одному на maximal subpart; для точности — свой цикл с `utf8.DecodeRune`.

### 4.3 Разбиение на строки (`subscription/scan-lines.ts:3-12`)
- Вход — **`content.trim()`** (`refresh.ts:207`, `:223`): первая строка теряет ведущие пробелы, последняя — хвостовые; внутренние строки сохраняют свои пробелы в `raw` **[проверено]**.
- `split(/\r?\n/)`; одиночный `\r` — не разделитель.
- `line` = 1-based индекс **после** split (пустые строки учитываются в нумерации, но затем отфильтровываются).
- `trimmed = raw.trim()`; строки с пустым `trimmed` удаляются.
- **JS `trim()`/`\s`**: пробельные = `\t \n \v \f \r space U+00A0 U+1680 U+2000–U+200A U+2028 U+2029 U+202F U+205F U+3000 U+FEFF`. Go `strings.TrimSpace`: **не** трогает U+FEFF (BOM) и **трогает** U+0085 (NEL), которого нет в JS. Нужна своя функция `jsTrim`/`isJSSpace` — используется в трёх местах (trim контента, trim строк, `\s` в normalizeBase64, а также zod `.trim()` в конфиге).

### 4.4 Парсинг VLESS-строки (`subscription/parse-subscription-line.ts:24-64`)
Возвращает `{line, raw, label}` или `undefined` (skip). Алгоритм:
1. `trimmed` пуст или не содержит `://` → skip.
2. `new URL(trimmed)` (WHATWG URL); исключение → skip.
3. `protocol !== 'vless:'` → skip (схема регистронезависима: `VLESS://` ок).
4. `label = decodeURIComponent(hash без '#')`; `uuid = decodeURIComponent(username)`.
   - **`decodeURIComponent` бросает `URIError: URI malformed`** на `%` без двух hex или невалидном UTF-8 (`#100% fast`, `#a%E0%A4%A`). Исключение **не ловится** в парсере → валит весь refresh этой subscription (см. 4.6) **[проверено]**.
5. uuid должен матчить `/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i` (версия 1–5, variant 8/9/a/b) → иначе skip. `%41` в username → декодируется → не UUID → skip.
6. `hostname` непустой; `port` = `Number(uri.port)` если есть, иначе 0; нужно `1..65535` целое. Без порта → skip. WHATWG нормализует `0443`→`443`; `65536`/`+443` → парс-ошибка → skip.
7. Ключи `searchParams` (form-urlencoded: `+`→пробел, `%xx` декодируется: `ty%70e`→`type`) приводятся к lower-case; все должны быть из набора `type, path, security, alpn, encryption, fp, sni, flow, host, allowinsecure, headertype, pbk, sid` (`:4-18`). Пустой ключ (`?=x`) или `a+b` → skip. Пустой query `?`, `?&&` → ок. Дубли (`type=tcp&type=ws`) → ок.
8. Успех.

**[проверено]** таблица:

| Хвост строки | Результат |
|---|---|
| `#Hello%20World` | label `Hello World` |
| `#🇩🇪 Берлин` (сырые unicode/пробелы) | label `🇩🇪 Берлин` (URL кодирует, decodeURIComponent возвращает) |
| `#a#b` | label `a#b` |
| `#` или без `#` | label `""` |
| `#tab\there` | label `tabhere` (WHATWG выкидывает TAB/LF/CR из входа) |
| `uuid:pass@host` | ок |
| `/some/path?...` | ок (path игнорируется) |
| `@[::1]:443` | ок |
| `@exa mple.com` | skip (пробел в opaque host) |
| `@ex%41mple.com` | ок (vless — non-special scheme, opaque host) |
| UUID в верхнем регистре | ок; UUID версии 6 → skip |

Go: `net/url.Parse` отличается от WHATWG (non-special схемы, opaque host, удаление TAB/LF, нормализация порта, IPv6). Рекомендуется общий пакет `vlessuri` с WHATWG-совместимым разбором ровно тех частей, что используются (scheme, userinfo, host, port, query keys, fragment), плюс `decodeURIComponent`-совместимый декодер, **возвращающий ошибку** (чтобы сохранить «падение всей подписки» либо осознанно заменить на skip — решить явно).

### 4.5 Фильтрация (`runtime/refresh.ts:145-169`) и regex
- Для каждого **enabled** output enabled subscription: `regex = parseRegexLiteral(labelIncludeRegex)`; `matched = entries.filter(e => {regex.lastIndex = 0; return regex.test(e.label)})` — сброс `lastIndex` делает флаги `g`/`y` безопасными.
- `parseRegexLiteral` (`config/load-config.ts:22-35`): строка должна начинаться с `/`; `lastSlash = lastIndexOf('/')`; если `<= 0` → `Regex literal must end with "/flags".`; pattern = `[1, lastSlash)`, flags = после. `/a/b/i` → pattern `a/b`, flags `i`. Ошибки `new RegExp` идут как сообщения конфига: `Invalid flags supplied to RegExp constructor 'x'`, `Invalid regular expression: /(ok/i: Unterminated group` **[проверено]**.
- Семантика JS RegExp (без `u` — по UTF-16 code units; `\w`/`\d` ASCII; `i` — Unicode simple case folding, работает для кириллицы). Go `regexp` (RE2) не умеет lookaround/backreference; флаги `i/m/s` → `(?i)(?m)(?s)`, `g/y/d` игнорировать, `u/v` — почти no-op. Варианты: (a) `dlclark/regexp2` с `RegexOptions.ECMAScript` — ближе к JS; (b) RE2 + отклонение неподдерживаемого на этапе конфига (изменение поведения). Надо зафиксировать.
- `matched.length == 0` → output-failure `Filtered output is empty.`
- Успех: `plain = matched.map(e => e.raw).join('\n')` (без завершающего `\n`); `base64 = Buffer.from(plain,'utf8').toString('base64')` — стандартный алфавит с `=`-паддингом (Go `base64.StdEncoding`).

### 4.6 Цикл refresh (`runtime/refresh.ts:171-274`)
```
state.refreshInProgress = true; lastRefreshStartedAt = now
for subscription in subscriptions where enabled (последовательно, без параллелизма):
  try:
    input = loadInputSource(sub.input, {format, fetchTimeoutMs})
    try: writeSourceCache(configPath, sub.id, input.content)  -> log info subscription_source_cache_written
    catch: log warn subscription_source_cache_write_failed (продолжаем)
    entries = parse(input.content.trim())
    reports += buildOutputReports(state, sub, entries)               # usedSourceCache не выставляется
    log info subscription_refreshed
  catch err (ЛЮБАЯ ошибка, включая URIError из парсинга):
    try:
      cached = readSourceCache(configPath, sub.id)
      entries = parse(cached.content.trim())                        # формат НЕ применяется повторно
      reports += buildOutputReports(..., {usedSourceCache: true})
      log warn subscription_source_cache_used
    catch cacheErr:
      log error subscription_source_cache_missed
      log error subscription_refresh_failed
      for output in enabled outputs: reports += updateOutputFailure(..., err.message)   # без usedSourceCache
finally: refreshInProgress = false; lastRefreshFinishedAt = now
return report (sorted by id via localeCompare)
```
Важные следствия:
- **Disk-cache пишется до парсинга** — даже если контент не содержит ни одной валидной строки (например, HTML-страница ошибки с кодом 200), кеш перезаписывается. В памяти lastGood сохраняется (outputs → `Filtered output is empty.`), но после рестарта из кеша уже ничего не соберётся.
- Кеш хранит **декодированный** контент (`input.content`), не сырой ответ.
- При фатальной ошибке парсинга (URIError) кеш уже перезаписан тем же «плохим» контентом, fallback падает так же → `subscription_source_cache_missed` с `cacheError: "URI malformed"` **[проверено]**.
- Успех из кеша → `ok: true`, `usedSourceCache: true`, `lastError` **очищается**; ошибка upstream видна только в warn-логе.
- `updateOutputFailure` (`:73-105`) не трогает `lastGood*`; ставит `lastRefreshAt`, `lastFailureAt` (два отдельных `now()`), `lastError`; `usedCachedValue = Boolean(prev.lastGoodBase64)`.
- `updateOutputSuccess` (`:107-143`): новый объект состояния (immutable replace), `lastError = undefined`.
- Конфиг-поля output (`pathRoute, name, regex, userAgent, profileTitle, profileUpdateInterval`) копируются в state при каждом обновлении.
- `refreshWithConfig` практически не бросает (всё ловится), поэтому `refresh_failed` в cron-обёртке на практике недостижим.

### 4.7 State (`types.ts:73-97`, `refresh.ts:25-60`)
- `createAppState` регистрирует **все** outputs **всех** subscriptions (включая disabled) в `outputs` и `outputsByPath` → запрос к disabled output даёт **502**, а не 404 **[проверено]**.
- Ключ `outputsByPath` = `buildOutputPath(pathRoute, id)` = `` `${pathRoute}/${encodeURIComponent(id)}` `` (`runtime/output-path.ts:1-3`).
- Используемые извне поля: только `lastGoodBase64`, `userAgent`, `profileTitle`, `profileUpdateInterval`. `lastGoodPlain`, `lastGoodLineCount`, `last*At`, `lastError`, `refreshInProgress`, `lastRefresh*At` нигде не экспонируются (нужны только тестам).
- Go: JS однопоточен, обновления атомарны относительно HTTP. В Go — `sync.RWMutex` или `atomic.Pointer[outputState]` на каждый output (запись целиком новой структуры).

### 4.8 Расписание (`runtime/run-server.ts:35-137`)
`runCronLoop(schedule, isStopping, task)`:
```
scheduledAt = now(); startTask()                     # немедленный запуск
while !stopping:
  scheduledAt = nextCron(schedule, scheduledAt)      # от предыдущего слота, не от now → без дрейфа
  sleep(max(0, scheduledAt - now()))
  if stopping: break
  if tickInProgress: onOverrun(scheduledAt); continue   # слот пропускается, без догоняния
  startTask()                                        # не await — задача фоном
await activeTask (если есть)
```
- `runLoggedCronLoop` оборачивает task: исключение → `logger.error({...context, event: failureEvent, error, causes?}, failureMessage)`; overrun → `logger.warn({...context, event: overrunEvent, scheduledAt: ISO}, overrunMessage)`.
- `getErrorLogDetails` (`:19-33`): для `AggregateError` добавляет `causes` = непустые message вложенных ошибок.
- Если процесс «проспал» несколько слотов, `nextCron(scheduledAt)` даёт прошлые даты → delay 0 → слоты отрабатываются подряд (часть как overrun).
- **Баг Node, не переносить**: `setTimeout` с задержкой > 2³¹−1 мс (~24.8 дня, напр. `@yearly`) срабатывает через 1 мс → цикл крутится без ожидания. В Go `time.Timer` такого ограничения нет.

### 4.9 `serve` (`run-server.ts:139-171`)
1. `loadConfig` → `createLogger` → `createAppState`.
2. `process.once('SIGINT'|'SIGTERM', stop)` — только выставляет флаг.
3. log `config_loaded`.
4. **Начальный refresh** (await) — HTTP ещё не слушает (healthz недоступен всё это время, до N × fetchTimeoutMs).
5. `startServer` → log `server_started`.
6. `runLoggedCronLoop` — его первый тик стартует **сразу**, т.е. **при старте refresh выполняется дважды подряд** (второй с логом `refresh_tick`) **[проверено]**.
7. `finally`: `server.close()`.

**Shutdown [проверено]**: SIGTERM лишь ставит флаг; цикл висит в `sleep` до следующего cron-слота (до 10 минут при `*/10`), HTTP продолжает обслуживать, затем ждёт активный refresh, закрывает сервер и выходит с 0. Второй SIGTERM/SIGINT (обработчик `once`) — дефолтное поведение Node → немедленное завершение (exit 143/130). В Docker это значит: `docker stop` → через 10 с SIGKILL. В Go рекомендуется прерывать sleep по `ctx.Done()` (осознанное улучшение; зафиксировать).
Логов о остановке нет.

---

## 5. HTTP

### 5.1 Сервер (`runtime/server.ts:74-123`)
- `listen`: host = до последнего `:`, port = `Number(...)` → `server.listen(port, host)`. Ошибка bind (EADDRINUSE) → reject → `serve` завершается с сообщением в stderr, exit 1. Хост `[::]` с квадратными скобками Node **не** поймёт (getaddrinfo), `:::8080` → host `::` работает. Go: `net.JoinHostPort(host, port)`.
- Путь: `new URL(request.url ?? '/', 'http://localhost').pathname` (`:85`) — WHATWG: query отбрасывается, dot-сегменты нормализуются (`/s/x/../a` → `/s/a`, также `%2e%2e`), `//evil/s/x` → pathname `/s/x` (часть трактуется как authority), `\` → `/`, не-ASCII процент-кодируется, **существующие `%xx` не декодируются** **[проверено]**.
- Метод **не проверяется**: GET/POST/PUT/… отдают одно и то же; HEAD — те же заголовки без тела (Node сам).
- Node defaults: keep-alive (`Connection: keep-alive`, `Keep-Alive: timeout=5`), `headersTimeout` 60 s, `requestTimeout` 300 s, max header 16 KiB.
- Тело пишется `writeHead(status, {'content-type', ...extra})` + `end(body)` → **`Transfer-Encoding: chunked`, без `Content-Length`** **[проверено]**. Go по умолчанию поставит Content-Length — для клиентов эквивалентно.
- Заголовки ответа: имена в нижнем регистре как заданы (`content-type`, `profile-title`, `profile-update-interval`), затем `Date`, `Connection`, `Keep-Alive`. Go каноникализирует имена (`Profile-Title`) — HTTP/1.1 регистронезависим, но для байтовой идентичности можно писать напрямую в `w.Header()["profile-title"]`.
- Никаких cache-заголовков (`Cache-Control`, `ETag`, `Last-Modified`) нет. Access-логов нет.

### 5.2 Маршруты (`runtime/server.ts:34-72`)

| Запрос (pathname) | Статус | Content-Type | Тело | Доп. заголовки |
|---|---|---|---|---|
| `/healthz` | 200 | `text/plain; charset=utf-8` | `ok\n` | — |
| `<pathRoute>/<encodeURIComponent(id)>`, есть `lastGoodBase64` | 200 | `text/plain; charset=utf-8` | base64 (без `\n` в конце) | профильные (см. ниже) |
| то же, `lastGoodBase64` нет (ещё не было успеха, disabled output/subscription) | 502 | `text/plain; charset=utf-8` | `output is not available yet\n` | — |
| всё остальное | 404 | `text/plain; charset=utf-8` | `not found\n` | — |

Профильные заголовки (`:52-71`):
- UA = `request.headers['user-agent']` (Node при дублях берёт первый); если UA пуст/нет, или у output нет `userAgent` → без заголовков.
- Совпадение: `userAgent.startsWith(prefix)` для любого prefix — **регистрозависимо**.
- При совпадении: `profile-title: base64:<std-base64(utf8(profileTitle))>` (если `profileTitle` задан) и `profile-update-interval: <int>` (если задан). Пример: `Тест` → `base64:0KLQtdGB0YI=`; `Available Profile` → `base64:QXZhaWxhYmxlIFByb2ZpbGU=`.
- Несовпадение UA **не** блокирует выдачу тела.

Сопоставление пути — точное строковое равенство с ключом `outputsByPath`: так как ключ строится через `encodeURIComponent(id)`, а pathname — WHATWG-кодированием, совпадают они только для «простых» id. Особенности:
- `encodeURIComponent` оставляет `A-Za-z0-9 - _ . ! ~ * ' ( )`, остальное `%XX` в **верхнем** регистре UTF-8; запрос с `%c3%a4` (нижний регистр) **не** совпадёт с `%C3%A4`.
- `pathRoute` в ключ попадает **без** кодирования: не-ASCII `pathRoute` (`/профили`) никогда не совпадёт с WHATWG-pathname (тот всегда процент-кодирован) → 404.
- Go: использовать «сырой» путь (`r.RequestURI`/`r.URL.EscapedPath()`) и WHATWG-подобную нормализацию (dot-сегменты, `%2e`), **не** `r.URL.Path` (он декодирован). Свою `encodeURIComponent` (не `url.PathEscape`/`QueryEscape`).

---

## 6. Disk source cache (`runtime/source-cache.ts`)
- Каталог: `join(dirname(resolve(configPath)), '.sfc-cache')` (`:4,7-9`).
- Файл: `<dir>/<encodeURIComponent(subscriptionId)>.subscription` (`:5,11-13`). `../main/slash id` → `..%2Fmain%2Fslash%20id.subscription` — не выходит за каталог.
- Запись (`:15-29`): `mkdir -p` (mode 0777 & umask); `tmp = <cachePath>.<pid>.<Date.now()>.tmp`; `writeFile(tmp, content, 'utf8')` (0666 & umask, без fsync); `rename(tmp, cachePath)`; при ошибке — `unlink(tmp)` (ошибку unlink глушить) и пробросить исходную. Возвращает `cachePath`.
- Чтение (`:31-38`): `readFile(cachePath, 'utf8')`; пустой файл допустим (проверки на пустоту нет; дальше просто 0 entries → `Filtered output is empty.`).
- Формат: plain UTF-8 текст декодированной подписки, без заголовков/метаданных.
- Docker: `.sfc-cache` монтируется как volume `./docker/sfc/.sfc-cache:/app/config/.sfc-cache` (корневой `docker-compose.yml`).

---

## 7. Логирование (`logging/create-logger.ts:7-28`)
- pino v10, `base: undefined` → **нет** `pid`/`hostname`. Назначение — **stderr** (fd 2) в обоих форматах.
- JSON: одна строка на событие, порядок ключей: `{"level":30,"time":<epoch ms>,<поля объекта>,"msg":"<message>"}`. Уровни: trace 10, debug 20, info 30, warn 40, error 50, fatal 60; `silent` — ничего.
- pretty: pino-pretty, `colorize: true` (ANSI всегда, даже без TTY), `translateTime: 'SYS:standard'` → `[2026-10-17 08:34:45.334 +0000] INFO: Hello.` + поля строками `    key: <json>` с отступом 4 (массивы многострочно), `ignore: 'pid,hostname'`.
- Идентичная реализация в `scd/src/logging/create-logger.ts` → общий Go-пакет логгера (slog с кастомным handler: `level` числом, `time` в ms, `msg` последним).

### 7.1 События

| event | level | поля (в порядке) | msg | где |
|---|---|---|---|---|
| `config_loaded` | info | `configPath` | `Config loaded.` | `run-server.ts:152` |
| `server_started` | info | `listen` | `Subscription server started.` | `server.ts:102-108` |
| `refresh_tick` | info | — | `Refresh tick started.` | `run-server.ts:159` |
| `refresh_tick_skipped_overrun` | warn | `scheduledAt` (ISO) | `Refresh tick skipped because previous run is still in progress.` | `run-server.ts:125-133,162-163` |
| `refresh_failed` | error | `error`, `causes?` | `Refresh tick failed.` | `run-server.ts:111-121,164-165` |
| `subscription_source_cache_written` | info | `subscriptionId`, `path` | `Subscription source cache written.` | `refresh.ts:189-196` |
| `subscription_source_cache_write_failed` | warn | `subscriptionId`, `error` | `Subscription source cache write failed; continuing with fresh input.` | `refresh.ts:198-205` |
| `subscription_refreshed` | info | `subscriptionId`, `source`, `outputs` (ids enabled outputs), `parsedEntries` | `Subscription refreshed.` | `refresh.ts:209-218` |
| `subscription_source_cache_used` | warn | `subscriptionId`, `path`, `error`, `parsedEntries` | `Subscription refresh used source cache after input load failed.` | `refresh.ts:225-234` |
| `subscription_source_cache_missed` | error | `subscriptionId`, `error`, `cachePath`, `cacheError` | `Subscription source cache is not available after input load failed.` | `refresh.ts:237-246` |
| `subscription_refresh_failed` | error | `subscriptionId`, `error` | `Subscription refresh failed.` | `refresh.ts:247-254` |

Нет логов: на каждый output, на «Filtered output is empty», на HTTP-запросы, на shutdown. `subscription_refreshed` пишется даже если все outputs упали по пустому фильтру. `source` — URL / абсолютный путь / `stdin`. `cacheError` для отсутствующего кеша — текст Node `ENOENT: no such file or directory, open '<path>'` (тест проверяет `/ENOENT/`).

---

## 8. Обработка ошибок — сводка
- Ошибки конфига/CLI → stderr `message`, exit 1.
- Ошибка загрузки одного source не влияет на остальные (per-subscription try/catch).
- Ошибка записи кеша — warn, продолжаем.
- Ошибка фильтра (пусто) — per-output failure, lastGood сохраняется.
- HTTP никогда не возвращает 5xx кроме 502 «not available yet».
- Ошибка bind — фатальна при старте (после начального refresh!).

---

## 9. Docker runtime contract
- `sfc/Dockerfile`: multi-stage `node:24-alpine`; runtime `WORKDIR /app`, пользователь root, без `EXPOSE`, `CMD ["node","dist/cli.js","serve","--config","/app/config/config.yml"]`.
- Нет `.dockerignore` в `sfc/` (у gtl есть).
- Корневой `docker-compose.yml`: сервис `sfc`, `ports: "127.0.0.1:9091:8080"` → конфиг в контейнере должен слушать `0.0.0.0:8080` (README-пример `0.0.0.0:8081` для локального запуска); volumes `./docker/sfc/config.yml:/app/config/config.yml:ro` и `./docker/sfc/.sfc-cache:/app/config/.sfc-cache`; `restart: unless-stopped`. Файл `docker/sfc/config.yml` git-ignored и сейчас отсутствует.
- Образ `ghcr.io/a1ekseev/sfc`, теги релизов `vX.Y.Z` общие для трёх сервисов.
- Для Go: сохранить путь `/app/config/config.yml`, каталог кеша `/app/config/.sfc-cache` (вычисляется от конфига), CMD эквивалент `blanc sfc serve --config /app/config/config.yml`. TZ по умолчанию UTC (влияет на cron).

---

## 10. Дублирование с scd (кандидаты в общие Go-пакеты)

| Модуль sfc | Модуль scd | Различия |
|---|---|---|
| `src/subscription/scan-lines.ts` | `scd/src/subscription/scan-lines.ts` | Идентичны (байт-в-байт). |
| `src/subscription/parse-subscription-line.ts` | `scd/src/subscription/parse-subscription-line.ts` | Одинаковые шаги/порядок проверок, тот же список params и UUID-regex. scd возвращает `ParseResult` c `ok`, `reasonCode` (`empty_line`, `unsupported_scheme`, `invalid_uri`, `missing_required_field`, `unsupported_param`), `reason`, `details`, а при успехе `uri, uuid, address, port, params` (params — `Record`, при дублях последний выигрывает). sfc возвращает только `{line, raw, label}` либо `undefined`. Оба бросают URIError из `decodeURIComponent`. → общий Go-пакет возвращает полную структуру/ошибку с reason code; sfc использует подмножество. |
| `src/input/load-input.ts` | `scd/src/input/load-input.ts` | Логика идентична. sfc: типы `LoadedInput/LoadInputOptions` в `types.ts`, `applyInputFormat` возвращает ещё `source: ''`; sfc экспортирует только `decodeSubscriptionContent`/`loadInputSource` (как и scd). `InputError` в scd наследует `AppError` (с `code`, `options.cause`), в sfc — простой класс с `code`. |
| `src/logging/create-logger.ts` | `scd/src/logging/create-logger.ts` | Идентичны. (gtl — отличается, см. 05-gtl.md.) |
| `runtime/run-server.ts:9-137` (`wait`, `getNextCronDate`, `getErrorLogDetails`, `runCronLoop`, `runLoggedCronLoop`) | `scd/src/runtime/run-daemon.ts:12-140` | Идентичны (копия). → общий пакет `cronloop`. |
| `config/load-config.ts:22-35` `parseRegexLiteral` | `scd/src/config/load-config.ts:30-43`; `scd/src/runtime/filter-manifest.ts:3-8` | Конфиговая версия идентична; в filter-manifest scd — без проверок (предполагает валидный вход). → общий `jsregex`. |
| `config/load-config.ts:37-47` `validateListenAddress` | `scd/src/config/load-config.ts:45-56` | Идентичны (тексты ошибок тоже). |
| `config/load-config.ts:185-205` env-интерполяция | `scd/src/config/load-config.ts:441-449,466` | **Разные**: sfc — после YAML-парсинга, по строковым значениям, отсутствующая → `""`; scd — по сырому тексту до YAML, отсутствующая → `ConfigError("Missing environment variable \"X\" required by config.")`. Общий пакет должен поддержать оба режима. |
| `config/load-config.ts:207-219` резолв input | `scd/src/config/load-config.ts:451-484` | Та же семантика (`-`, http(s), absolute, иначе от каталога конфига). |
| Формат ошибки конфига | scd: `Invalid config at <path>: <path.join('.')||'<root>'>: <msg>; ...` | sfc: только сообщения через `\n`, без пути. |
| `errors.ts` | `scd/src/errors.ts` | scd: иерархия `AppError(code)`; sfc: `ConfigError`, `InputError('EMPTY_INPUT')`. |

---

## 11. Инвентарь тестов (behaviour assertions)

`test/run-server.test.ts`
1. (`:19`) `runCronLoop`: при задаче, работающей дольше одного слота, слот 100 мс пропускается (onOverrun с `1970-01-01T00:00:00.100Z`), следующий запуск ровно в слот 200 — без дрейфа; старты `[0, 200]`.
2. (`:61`) `getErrorLogDetails(AggregateError([e1,e2],'top-level'))` → `message: 'top-level'`, `causes: ['first cause','second cause']`.
3. (`:70`) `runLoggedCronLoop` при overrun пишет warn с `event: 'refresh_tick_skipped_overrun'` и сообщением `Refresh tick skipped because previous run is still in progress.`; error-логов нет.
4. (`:126`) `runLoggedCronLoop` при исключении-AggregateError пишет ровно один error с `event: 'refresh_failed'`, `causes: ['fetch failed','decode failed']`, msg `Refresh tick failed.`; warn нет.

`test/server.test.ts`
5. (`:51`) `/healthz` → 200, тело `ok\n`.
6. (`:57`) неизвестный id под существующим route → 404.
7. (`:62`) output без `lastGoodBase64` → 502.
8. (`:67`) output с payload → 200, тело = `lastGoodBase64`, без доп. заголовков (без UA).
9. (`:74`) UA `ClashMeta/1.0` совпал с префиксом `Clash` → `profile-title: base64:QXZhaWxhYmxlIFByb2ZpbGU=`, `profile-update-interval: 6`.
10. (`:83`) UA `Shadowrocket/1.0` не совпал → 200 без заголовков.
11. (`:91`) у output нет `userAgent` → заголовков нет даже при наличии profileTitle/interval.
12. (`:99`) id существует, но под другим route (`/s/available` при `/profiles`) → 404.

`test/service.test.ts`
13. (`:66`) Конфиг с regex `/broken` и дублирующимся output id отклоняется (сообщение содержит `Duplicate output id` или `Regex literal`).
14. (`:89`) `pathRoute: /profiles` парсится; отсутствие `pathRoute` → ошибка `expected string, received undefined`; `pathRoute: profiles/` → ошибка, содержащая `pathRoute`.
15. (`:148`) `userAgent: [""]` и `profileUpdateInterval: 25` → ошибка `Too small|Too big`.
16. (`:174`) Валидные `userAgent: [Clash, Stash]`, `profileTitle: Main Profile`, `profileUpdateInterval: 24` сохраняются как есть.
17. (`:205`) Plain remote source: один fetch; `trojan://` пропускается; 2 успешных output; state содержит pathRoute/UA/profile-поля; `out-germany.lastGoodPlain` = одна немецкая строка; `out-europe.lastGoodPlain` = две строки в исходном порядке через `\n`.
18. (`:245`) Base64 remote source в `auto` декодируется; выход перекодирован в base64 и декодируется обратно в исходную строку.
19. (`:270`) Успешный refresh пишет `<dir>/.sfc-cache/main.subscription` с декодированным контентом; при последующей ошибке fetch outputs собираются из кеша: `successful 2, failed 0`, у всех `usedSourceCache: true`, lastGood тот же, `outputsByPath` указывает на тот же объект, что `outputs`, `lastError` пуст.
20. (`:309`) Ошибка fetch и нет кеша: `failed 2`, `usedSourceCache` не `true`, `lastError` содержит `network down`; error-лог `subscription_source_cache_missed` c `cachePath` = путь кеша, `cacheError` ~ `ENOENT`, `error` ~ `network down`.
21. (`:341`) Если фильтр стал пустым: report `ok:false`, `usedCachedValue:true`, lastGood не изменился, `lastError` ~ `empty`; кеш при этом перезаписан новым source.
22. (`:369`) Путь кеша для id `../main/slash id` остаётся внутри `.sfc-cache` и оканчивается на `..%2Fmain%2Fslash%20id.subscription`; запись/чтение работают.
23. (`:389`) Если `rename` не удаётся (на месте cachePath — каталог), `writeSourceCache` бросает, а temp-файл `<cachePath>.<pid>.<Date.now()>.tmp` удалён.
24. (`:407`) Повторный успешный refresh заменяет объект состояния (новая ссылка), `outputsByPath` синхронизирован, новый label (`Франкфурт`) в plain, профильные поля и pathRoute сохранены.

---

## 12. Тонкости, которые легко потерять
1. Двойной refresh при старте `serve` (начальный + немедленный первый тик cron) и HTTP недоступен до окончания первого.
2. SIGTERM не прерывает ожидание cron-слота; процесс живёт до следующего слота; второй сигнал убивает сразу.
3. Disabled outputs/subscriptions отдают **502**, не 404.
4. `content.trim()` перед split: внутренние строки сохраняют ведущие/хвостовые пробелы в выходе, крайние — нет; `\r` срезается только перед `\n`.
5. JS `trim`/`\s` ≠ Go `TrimSpace` (BOM U+FEFF, NEL U+0085).
6. Лояльный base64-декодер Node: пропуск мусора, остановка на первом `=`, оба алфавита, без паддинга.
7. `auto`: если в тексте где-либо есть `://` — base64 не пробуется вовсе; декод принимается, только если результат содержит `://`.
8. `decodeURIComponent` бросает → вся подписка (и fallback из кеша) падает с `URI malformed`.
9. Кеш перезаписывается до парсинга, даже «пустым» по смыслу контентом; хранится декодированный текст; при чтении формат не применяется.
10. Успех из кеша = `ok:true`, `lastError` сброшен; `usedSourceCache` в JSON только когда `true`.
11. Сортировка отчёта — ICU `localeCompare`, не байтовая.
12. `User-Agent: node` и прочие undici-заголовки при fetch; таймаут покрывает чтение тела; `response.text()` срезает BOM.
13. Ошибка сети в отчёте — просто `fetch failed`.
14. Env-подстановка только в строковых значениях после YAML, отсутствующая → `""`; ключ `${...}` с флагом `i`.
15. Неизвестные ключи конфига игнорируются; все строки trim-аются; пустые `name`/`profileTitle` исчезают.
16. Пустой `refreshSchedule` = каждую минуту; поддержка `L`, `#`, `@minutely`, 6-полевых выражений; локальный TZ.
17. Порт в `listen` парсится JS `Number()` (hex/экспонента допустимы).
18. Путь запроса: WHATWG-нормализация, без декодирования `%xx`; ключ — `pathRoute + '/' + encodeURIComponent(id)`; не-ASCII pathRoute недостижим.
19. Метод HTTP не проверяется; ответы chunked без Content-Length; имена заголовков в нижнем регистре; никакого кеширования.
20. Профильные заголовки только при `startsWith` по UA (регистрозависимо) и только для заданных полей; `profile-title` = `base64:` + std base64 UTF-8.
21. Флаги regex `g`/`y` безопасны из-за `lastIndex = 0`; JS-семантика RegExp ≠ RE2.
22. Логи в stderr без pid/hostname; `msg` — человекочитаемая фраза, `event` — машинное имя.
23. `refresh` CLI: exit 1 при любом failed output (включая «пустой фильтр»), и тоже пишет кеш.
24. Отсутствует проверка метода и лимитов размера upstream-ответа (весь ответ читается в память).
