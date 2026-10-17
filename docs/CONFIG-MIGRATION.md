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

## scd gtl

Таблица «было → стало»: ключ, причина, значение по умолчанию.

| Ключ | Было (TS) | Стало (Go) | Причина |
|---|---|---|---|
| `server.listen` | `:80` невалиден (host обязателен) | `:8080` допустим — все интерфейсы; IPv6 только в скобках (`[::]:8080`) | `net.SplitHostPort`, идиоматичный Go |

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

Что изменилось относительно cron-parser (D24, D28).

## Подписки

Поведение ссылок: политика allowInsecure (D15), неизвестные параметры (D28), новые профили (D26).

## Запуск

Одна команда `scd …`, один образ, `command:` в docker-compose для sfc/gtl (D10).

## Логи

Формат и поля логов (D27).

## Разобранные примеры

Боевые конфиги `docker/{scd,sfc,gtl}/config.real-shape.yml` → Go-формы `config.real-shape.go.yml` (diff по шагам).

## Чек-лист

- `scd validate-config --config …`
- `scd sfc validate-config --config …`
- `scd gtl validate-config --config …`
