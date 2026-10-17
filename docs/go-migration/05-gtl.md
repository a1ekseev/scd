# 05 — gtl (Gen Test Load): спецификация для переноса на Go

Источник: `gtl/` на ветке `move-to-go` (package.json `version: 1.0.11`). Ссылки `file:line` — относительно `gtl/`.
**[проверено]** — подтверждено прогоном оригинального TS-кода (Node 22, зависимости из `package-lock.json`) и эталонной Go-реализацией генератора в scratch-окружении.

---

## 1. Назначение

HTTP-сервис для детерминированных download/load-тестов. На `GET <load.path>/<sizeKb>` отдаёт ровно `sizeKb * 1024` байт псевдослучайных, но детерминированных данных, стримом по 64 KiB. Для валидных запросов всегда `200`, чтобы клиент скачал всё тело. Не зависит от scd/sfc/Xray (`AGENTS.md:1-7`).

> Документация устарела: `AGENTS.md:19` пишет «deterministic ASCII `a` bytes», но код генерирует xorshift-поток (`src/runtime/server.ts:92-101`); README (`README.md:20`) корректен. Тест `server.test.ts:81` прямо проверяет, что это **не** `aaaaaaaa`.

---

## 2. CLI (`src/cli.ts`)

commander v14, `program.name("gtl").description("Gen Test Load").version("1.0.2")` (`cli.ts:9`). В Go: `blanc gtl <cmd>`.

| Команда | Флаги | Поведение | stdout | Exit |
|---|---|---|---|---|
| `serve` | `--config <path>` (обяз.) | `runServer` (`cli.ts:11-17`) | JSON-логи pino (stdout!) | 0 после штатной остановки по сигналу; 1 при ошибке |
| `validate-config` | `--config <path>` | `loadConfig` → печать `Config is valid\n` (`cli.ts:19-28`) | `Config is valid` | 0/1 |
| `print-config` | `--config <path>` | нормализованный конфиг, `JSON.stringify(config, null, 2) + "\n"` (`cli.ts:30-39`) | JSON | 0/1 |
| `--version` / `-V` | | печатает `1.0.2` (захардкожено, **не** версия пакета 1.0.11) | `1.0.2` | 0 |

- Описания команд: `Start HTTP load generator`, `Validate config file`, `Print normalized config`; описание опции: `Path to config.yml`.
- Ошибки внутри action ловит `runCommand` (`cli.ts:43-50`): stderr ← `formatCliError(error)` + `\n`, `exitCode = 1`. `formatCliError` (`runtime/run-server.ts:31-36`): для `ConfigError` — `message`; для прочих `Error` — **`stack`** (многострочный); иначе `String(error)`.
- Ошибки commander: `error: required option '--config <path>' not specified`, `error: unknown command 'bogus'` (без help, т.к. нет `showHelpAfterError`), exit 1. Без команды — help в stderr, exit 1 **[проверено]**.
- Top-level `await program.parseAsync()` (`cli.ts:41`), ESM.
- Конфиг-путь **не** резолвится в абсолютный (`load-config.ts:68`, `:86`): относительный путь — от CWD, в сообщениях — как передан.

`print-config` порядок ключей — порядок zod-схемы: `server{listen}`, `logging{level,format}`, `load{path,maxSizeKb}` **[проверено]**:
```json
{
  "server": { "listen": "127.0.0.1:18790" },
  "logging": { "level": "info", "format": "json" },
  "load": { "path": "/load", "maxSizeKb": 64 }
}
```
(реально с 2-space многострочным отступом `JSON.stringify(_, null, 2)`).

---

## 3. Конфигурация (`src/config/load-config.ts`)

Загрузка (`:65-89`):
1. `readFile(configPath, 'utf8')`; ошибка → `ConfigError("Failed to read config <configPath>: <err.message>")`, напр. `Failed to read config nope.yml: ENOENT: no such file or directory, open 'nope.yml'`.
2. `yaml.parse` (eemeli/yaml v2, YAML 1.2); ошибка → `ConfigError("Failed to parse config <configPath>: <yaml message>")`.
3. zod `safeParse`; ошибка → `ConfigError(z.prettifyError(error))`.
4. **Env-интерполяции нет** (`/${X}` остаётся буквально **[проверено]**). **Резолва путей нет.**

Все объекты `.strict()` — неизвестные ключи → ошибка `✖ Unrecognized key: "bar"` (в отличие от sfc, где они отбрасываются).

| Путь | Тип | Обяз. | Default | Валидация | Ссылка |
|---|---|---|---|---|---|
| `server` | object (strict) | да | — | | `:39-43` |
| `server.listen` | string | да | — | **без trim**; `sep = lastIndexOf(':')`; `sep <= 0 \|\| sep == len-1` → `listen must be in host:port format`; иначе host = `[0,sep)`, port = JS `Number(rest)`; `host.length == 0 \|\| !isInteger(port) \|\| port<1 \|\| port>65535` → `listen must contain a valid host and TCP port` | `:8-20` |
| `logging` | object (strict) | нет | `{level:"info", format:"json"}` | | `:44-50` |
| `logging.level` | enum `fatal\|error\|warn\|info\|debug\|trace\|silent` | нет | `info` | | `:46` |
| `logging.format` | enum `json\|pretty` | нет | `json` | | `:47` |
| `load` | object (strict) | да | — | | `:51-61` |
| `load.path` | string | да | — | **без trim**; все проверки накапливаются: не начинается с `/` → `load.path must start with /`; `== "/"` → `load.path must not equal /`; `len>1` и оканчивается `/` → `load.path must not end with /`; содержит `?` или `#` → `load.path must not contain query or hash` | `:22-35` |
| `load.maxSizeKb` | number | нет | `10240` | `int`, `>0`, `<= floor(MAX_SAFE_INTEGER/1024) = 8796093022207` | `:54-59` |

Формат `z.prettifyError` (zod v4) **[проверено]**: на каждую issue строка `✖ <message>`, затем (если путь непустой) `  → at <path.dot.joined>`; строки через `\n`; issues **отсортированы по длине пути** (корневые первыми). Примеры:
```
✖ Unrecognized key: "bar"
✖ Unrecognized key: "foo"
  → at server
```
```
✖ Invalid input: expected object, received undefined
  → at load
✖ listen must be in host:port format
  → at server.listen
```
Другие тексты: `✖ Too small: expected number to be >0` (maxSizeKb: 0), `✖ Invalid input: expected int, received number` (1.5), `✖ Too big: expected number to be <=8796093022207`, `✖ Invalid option: expected one of "fatal"|"error"|"warn"|"info"|"debug"|"trace"|"silent"`, пустой файл → `✖ Invalid input: expected object, received null`.

Особенности:
- `listen: " h:80"` проходит валидацию (без trim), упадёт уже при bind.
- Порт — JS `Number()`: `0x50`, `1e3`, `+80` допустимы (как в sfc/scd).
- `load.path` проверяется построчно, может выдать несколько ошибок сразу (`"lo?ad/"` → три).
- `load.path` может быть многосегментным (`/a/b`).
- YAML 1.2: `yes/no` — строки. Целое `64.0` проходит как int.

Пример (`README.md:31-42`):
```yaml
server:
  listen: 0.0.0.0:8080
load:
  path: /load
  maxSizeKb: 10240
logging:
  level: info
  format: json
```

---

## 4. HTTP (`src/runtime/server.ts`)

### 4.1 Сервер
- `startServer` (`:103-110`): `createServer`; `server.listen(port, host)` из `parseListenAddress` (`load-config.ts:91-97`: split по последнему `:`, `Number(port)`). **Не ждёт** события `listening`.
- `runServer` (`runtime/run-server.ts:8-29`): лог `gtl_server_started` пишется **сразу после вызова listen, до фактического bind**. Ошибка bind (EADDRINUSE) приходит событием `error` → `events.once(server,'close')` реджектится → `runCommand` печатает **stack** (`Error: listen EADDRINUSE: address already in use 127.0.0.1:18790\n    at ...`), exit 1 **[проверено]**.
- Node defaults: keep-alive (`Connection: keep-alive`, `Keep-Alive: timeout=5`), `headersTimeout` 60 s, `requestTimeout` 300 s, max header 16 KiB. Go: задать сопоставимые таймауты (кроме WriteTimeout — большие загрузки!).

### 4.2 Обработка запроса (`:112-120`)
1. `request.method !== "GET"` → **405** (в т.ч. HEAD, POST) — до проверки пути, `/healthz` тоже 405 для не-GET. Заголовка `Allow` нет.
2. `pathname = new URL(request.url ?? "/", "http://127.0.0.1").pathname` — WHATWG: query отбрасывается, dot-сегменты нормализуются (`/a/../load/1` → `/load/1`), `//h/load/1` → pathname `/load/1` (часть становится authority) — оба дают 200 **[проверено]**; `%xx` **не** декодируются (`/load/%33%32` → 400).
3. `buildResponse(pathname, config)` (`:16-61`).

### 4.3 Маршрутизация `buildResponse`

| Условие | Статус | Заголовки | Тело |
|---|---|---|---|
| `path === "/healthz"` | 200 | `content-type: text/plain; charset=utf-8`, `content-length: 2` | `ok` (**без** `\n`, в отличие от sfc) |
| `!path.startsWith(load.path + "/")` (в т.ч. `/load` без слэша) | 404 | text/plain + content-length | `not found\n` |
| хвост не `^\d+$` (ASCII-цифры; пусто, `1/`, `abc`, `200_32`, `%33`) | 400 | text/plain + content-length | `invalid load route, expected <load.path>/:sizeKb\n` |
| `sizeKb = Number(хвост)`; не safe-integer, `<0`, `> maxSizeKb` | 400 | text/plain + content-length | `sizeKb must be an integer from 0 to <maxSizeKb>\n` |
| `sizeBytes = sizeKb*1024` не safe-integer (недостижимо из-за лимита maxSizeKb) | 400 | | `requested size is too large\n` |
| иначе | 200 | см. ниже | `sizeBytes` байт payload |

Заголовки 200-ответа load (порядок как в коде, имена в нижнем регистре):
```
content-type: application/octet-stream
content-length: <sizeBytes>
x-gtl-status-code: 200
x-gtl-requested-size-kb: <sizeKb>        # String(Number(...)) → "007" даёт "7"
x-gtl-requested-size-bytes: <sizeBytes>
x-gtl-actual-size-bytes: <sizeBytes>
```
+ автоматические `Date`, `Connection: keep-alive`, `Keep-Alive: timeout=5`. Никаких cache-заголовков.

Текстовые ответы (`textResponse`, `:122-132`): `content-type: text/plain; charset=utf-8`, `content-length: <byteLength(body)>`. 405-тело: `method not allowed\n` (19 байт).

Проверено вживую **[проверено]**: `/load/0` → 200 c `content-length: 0`; `/load/007` → 7168 байт; `/load/1?x=y` → 200; `/load/99999999999999999999` → 400 «sizeKb must be…»; `/load/` и `/load/1/` → 400 «invalid load route»; `/load` → 404.

Go-реализация разбора: `strings.HasPrefix(path, loadPath+"/")`; хвост — только `'0'..'9'`, непустой; числовая проверка: срезать ведущие нули, если длина > 16 → 400 «sizeKb must be…», иначе `strconv.ParseUint` и сравнение с `maxSizeKb` (эквивалентно, т.к. max ≤ 8796093022207, 13 цифр). Путь брать WHATWG-подобно из сырого `RequestURI` (dot-сегменты), **не** из декодированного `r.URL.Path` (иначе `/load/%33%32` станет 200, а не-ASCII `load.path` начнёт совпадать). Ветка `/healthz` проверяется раньше `load.path` (при `load.path: /healthz` запрос `/healthz` — health, `/healthz/5` — load).

### 4.4 Стриминг (`:63-90`)
`writeResponse`: `writeHead(status, headers)`; если есть `errorBody` → `end(errorBody)`; иначе `writeRepeatedPayload(response, bodyBytes)`.

`writeRepeatedPayload(stream, totalBytes)`:
```
chunk = Buffer.alloc(min(65536, max(totalBytes, 1)))   # один переиспользуемый буфер
remaining = totalBytes; offset = 0
while remaining > 0:
    current = remaining >= len(chunk) ? chunk : chunk[0:remaining]
    fillDeterministicPayload(current, offset)           # перезаполняется каждый раз
    if !stream.write(current): await 'drain'            # backpressure
    offset += len(current); remaining -= len(current)
stream.end()
```
- Чанки ровно 64 KiB, последний — остаток. Для `totalBytes == 0` — сразу `end()`.
- Content-Length выставлен заранее → без chunked-кодирования.
- Обрыв клиента: `write()` после destroy возвращает `false`, `'drain'` не приходит, `events.once` реджектится только по `'error'` → промис обработчика зависает навсегда (утечка замыкания). В Go — просто прекращать запись при ошибке `Write` (поведение для клиента идентично).
- Нет логирования запросов и ошибок записи; `void handleRequest(...)` — непойманный reject привёл бы к `unhandledRejection` (в Node 24 — падение процесса); на практике не возникает.

### 4.5 Детерминированный генератор — ДОЛЖЕН быть байт-идентичен (`:92-101`)

```ts
export function fillDeterministicPayload(chunk: Buffer, offset: number): void {
  let state = (0x9e3779b9 ^ offset) >>> 0;
  for (let index = 0; index < chunk.length; index += 1) {
    state ^= state << 13;
    state ^= state >>> 17;
    state ^= state << 5;
    chunk[index] = state & 0xff;
  }
}
```
Семантика:
- Это xorshift32 (13, 17, 5) Марсальи. В JS `^`/`<<` работают над int32, `>>>` — над uint32; на младшие 32 бита результата знак не влияет, поэтому эквивалентно чистой `uint32`-арифметике.
- **Сид = `uint32(0x9E3779B9 XOR uint32(offset))`**, где `offset` — **абсолютное** смещение начала чанка в теле ответа (0, 65536, 131072, …). JS `ToInt32(offset)` = `offset mod 2^32`, т.е. при теле > 4 GiB поток повторяется с периодом 2^32 байт (offset 2^32 даёт тот же чанк, что offset 0) **[проверено]**.
- Генератор **пересевается в начале каждого 64 KiB-чанка**; байт выдаётся **после** шага (первый байт — результат первого шага, не сид).
- Выход — младший байт состояния (`state & 0xff`).
- Значит, байт на позиции `p` тела: `xorshift32^(k+1)(seed(p - p%65536))[low byte]`, где `k = p % 65536`. Значение не зависит от общего размера ответа — префиксы совпадают (`/load/1` — префикс `/load/2`).
- Сид 0 (вырожденный xorshift → нули) возможен только при `offset ≡ 0x9E3779B9 (mod 2^32)`, а offset кратен 65536, младшие 16 бит `0x79B9 ≠ 0` → недостижимо. (Сама функция при offset `0x9E3779B9` выдаёт нули — **[проверено]** `off 2654435769 → 0000000000000000`.)

Эталонная Go-реализация (совпала побайтно, **[проверено]**):
```go
func fill(b []byte, offset uint64) {
    s := uint32(0x9e3779b9) ^ uint32(offset)
    for i := range b {
        s ^= s << 13
        s ^= s >> 17
        s ^= s << 5
        b[i] = byte(s)
    }
}
// stream: for off := uint64(0); off < n; off += 65536 { l := min(65536, n-off); fill(buf[:l], off); w.Write(buf[:l]) }
```

Тест-векторы (Node == Go):

| Вход | Результат |
|---|---|
| `fill(8 байт, offset=0)` | `193e3ab51f37d0bf` |
| `fill(8, 65536)` | `190f1a4032fb31ec` |
| `fill(8, 2^32)` | `193e3ab51f37d0bf` |
| `fill(8, 2^32+65536)` | `190f1a4032fb31ec` |
| первые 16 байт любого ответа ≥16 B | `193e3ab51f37d0bf39b8eeb4d33cb85f` |
| байты 65536..65551 | `190f1a4032fb31ec35b35ff491aa00de` |
| sha256, 0 B | `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855` |
| sha256, 1 B (`0x19`) | `68aa2e2ee5dff96e3355e6c7ee373e3d6a4e17f75f9518d843709c0c9bc3e3d4` |
| sha256, 16 B | `315ce6f66522879ae35976a7e0e053f7389dcfb3a528bfcfce782afe96eb4293` |
| sha256, 1024 B (`/load/1`) | `8ab783947bae918d01b74448625dfd0fef119e16e97d6fc9fdf15e310c87cb01` |
| sha256, 65536 B (`/load/64`) | `653f5cec98c039706a34d4a6d36ac73eedd26cf31dd0daac69656ab776b3ed33` |
| sha256, 65537 B | `9c9581062869709c9760522ed26ef6ffce7ef27b8fb6875c8ab4a0ceb674dc1a` |
| sha256, 98311 B (96 KiB + 7) | `9266733e50d6f87e36ee71ebf99c51392ef72c912d9b3a76e7ebdc13eb96cf5e` |
| sha256, 10485760 B (`/load/10240`) | `e4ed32cd8971b65b26904797c6cf2c5291d5f4e0746dd76970f79717f9aaf00f` |

Рекомендуется перенести эти векторы в Go-тесты как golden.

---

## 5. Жизненный цикл и сигналы (`runtime/run-server.ts:8-29`)
1. `loadConfig` → `createLogger` → `startServer`.
2. log info `gtl_server_started` `{listen}`.
3. `process.once('SIGINT'|'SIGTERM')` → log info `gtl_server_stopping` `{signal}` → `server.close()` (перестаёт принимать; Node ≥19 закрывает idle keep-alive; активные загрузки дописываются до конца).
4. `await once(server, 'close')` → log info `gtl_server_stopped` → процесс завершается сам, exit 0.
5. Второй сигнал — дефолтный обработчик Node → немедленное завершение (130/143).
Go: `http.Server.Shutdown(ctx)` без дедлайна ≈ то же самое (ждёт активные ответы).

---

## 6. Логирование (`logging/create-logger.ts:5-20`)
Отличается от sfc/scd:
- pino с **дефолтным `base`** → в каждой записи `pid` и `hostname`.
- Назначение — **stdout** (fd 1) в обоих режимах.
- JSON: `{"level":30,"time":<epoch ms>,"pid":12999,"hostname":"vm","listen":"127.0.0.1:18790","msg":"gtl_server_started"}`.
- pretty: pino-pretty `colorize: true`, `translateTime: "SYS:standard"`, pid/hostname **не** игнорируются: `[2026-10-17 08:34:45.840 +0000] INFO (16080): gtl_server_started` + `    listen: "0.0.0.0:8080"`.
- Нет поля `event`: имя события — это `msg`.

| msg | level | поля |
|---|---|---|
| `gtl_server_started` | info | `listen` |
| `gtl_server_stopping` | info | `signal` (`SIGINT`/`SIGTERM`) |
| `gtl_server_stopped` | info | — |

Логов на запросы, ошибки и конфиг нет. `logging.level` фактически влияет только на эти три info-строки (`warn`+ их скрывает).

---

## 7. Обработка ошибок — сводка
- Конфиг: `ConfigError` → stderr только message, exit 1.
- Прочие ошибки CLI (bind и т.п.) → stderr **stack**, exit 1.
- HTTP: 404 (не тот префикс), 400 (кривой/слишком большой размер), 405 (не GET), иначе 200. 5xx не бывает.

---

## 8. Docker runtime contract
- `gtl/Dockerfile`: multi-stage `node:24-alpine`; runtime `WORKDIR /app`, root, без `EXPOSE`, `CMD ["node","dist/cli.js","serve","--config","/app/config/config.yml"]`.
- `gtl/.dockerignore`: `node_modules`, `dist`, `coverage`, `*.log`, `config.yml`.
- Корневой `docker-compose.yml`: `ports: "127.0.0.1:9092:8080"` (конфиг должен слушать `0.0.0.0:8080`), volume `./docker/gtl/config.yml:/app/config/config.yml:ro`, `restart: unless-stopped`. Файл `docker/gtl/config.yml` сейчас отсутствует (git-ignored).
- Образ `ghcr.io/a1ekseev/gtl`; release tags общие (`README.md:57-63`).
- Go: CMD `blanc gtl serve --config /app/config/config.yml`; состояние на диске отсутствует.

---

## 9. Общий код с sfc/scd
gtl почти ничего не дублирует буквально, но в Go стоит переиспользовать:
- **listen-парсинг**: `parseListenAddress`/валидация — те же правила, что `validateListenAddress` в sfc/scd (`sfc/src/config/load-config.ts:37-47`), но с другими текстами ошибок (`listen must be in host:port format` / `listen must contain a valid host and TCP port`) и дополнительной (избыточной) проверкой `host.length === 0`. Общий парсер + сервис-специфичные сообщения.
- **Логгер**: `sfc`/`scd` — `base: undefined`, stderr, `ignore: pid,hostname`; `gtl` — с pid/hostname, stdout. Общий пакет с опциями `{dest, includePidHostname}`.
- **Конфиг-лоадер**: gtl — без env, без резолва путей, `.strict()`, `z.prettifyError`; sfc — env после YAML, non-strict, сообщения через `\n`; scd — env по сырому тексту, `Invalid config at ...`. Общий YAML-декодер, но форматирование ошибок — per-service.
- **CLI**: gtl — `formatCliError` (stack для не-ConfigError) vs sfc — всегда `message`.
- **WHATWG-pathname** для маршрутизации — общий helper для sfc и gtl.

---

## 10. Инвентарь тестов (behaviour assertions)

`test/config.test.ts`
1. (`:9`) Валидный конфиг: `listen 127.0.0.1:8080`, `maxSizeKb 64`, `logging debug/pretty` сохраняются.
2. (`:31`) Дефолты: `maxSizeKb = 10240`, `logging.level = info`, `logging.format = json`, `load.path` как задан.
3. (`:49`) `listen: localhost` → ошибка `listen must be in host:port format` (секция `load` отсутствует — ошибки накапливаются).
4. (`:61`) `maxSizeKb: 0` → ошибка `Too small`.
5. (`:76`) Отсутствие `load.path` → ошибка, содержащая `path`.
6–10. (`:90-105`) `load.path` из `"/"`, `"load"`, `"/load/"`, `"/load?x=1"`, `"/load#x"` → ошибка, содержащая `load.path`.

`test/server.test.ts`
11. (`:14`) `/healthz` → 200, тело `ok`, `content-length: 2`.
12. (`:21`) `/load/32` → 200, `bodyBytes 32768`, `content-type: application/octet-stream`, `content-length: 32768`, `x-gtl-status-code: 200`, `x-gtl-requested-size-kb: 32`, `x-gtl-requested-size-bytes: 32768`, `x-gtl-actual-size-bytes: 32768`.
13. (`:33`) При `load.path: /download` запрос `/download/32` → 200, 32768 байт.
14. (`:42`) При `load.path: /download` запрос `/load/32` → 404 (нет алиаса).
15. (`:50`) `/load/0` → 200, `bodyBytes 0`, `content-length: 0`.
16. (`:57`) `/download/200_32` → 400, тело содержит `expected /download/:sizeKb`.
17. (`:66`) `/load/65` при `maxSizeKb 64` → 400, тело содержит `0 to 64`.
18. (`:72`) `/unknown` → 404.
19. (`:77`) `writeRepeatedPayload(96 KiB + 7)` пишет ровно 98311 байт, первые 8 байт ≠ `aaaaaaaa`, среди первых 256 байт > 128 различных значений, поток завершён (`end` вызван).
20. (`:86`) `fillDeterministicPayload` для offset 0 и 64 даёт разные 64-байтные буферы (зависимость от абсолютного offset).

Рекомендуемые дополнительные Go-тесты: golden-векторы из §4.5; 405 для HEAD/POST; `/load/007`; `/load/1?x=y`; `/a/../load/1`; `/load/%33` → 400; `Unrecognized key` в конфиге.

---

## 11. Тонкости, которые легко потерять
1. Генератор пересевается **каждые 64 KiB** от абсолютного offset; offset усекается до uint32 (период 4 GiB); байт берётся после шага; размер чанка — часть контракта.
2. Тело — префикс-стабильно: ответ меньшего размера — префикс большего.
3. `/healthz` → `ok` **без** перевода строки и с `content-length: 2` (у sfc — `ok\n`).
4. Не-GET (включая HEAD) → 405 даже для `/healthz`; без `Allow`.
5. `x-gtl-requested-size-kb` — нормализованное число (`007` → `7`); ведущие нули допустимы.
6. Огромные числа → 400 «sizeKb must be…», а не «invalid route».
7. Маршрут требует `load.path + "/"`; `/load` → 404, `/load/` → 400; хвостовой `/` → 400.
8. WHATWG-нормализация pathname: `//host/load/1` и `/a/../load/1` → 200; `%xx` не декодируется.
9. Конфиг `.strict()` — лишние ключи — ошибка; нет env-подстановки; `listen`/`load.path` без trim.
10. Ошибки конфига — формат `z.prettifyError` с `✖`/`→ at`, сортировка по длине пути; прочие ошибки — со stack.
11. `--version` печатает `1.0.2`, не версию пакета.
12. Логи в **stdout** с `pid`/`hostname`, имя события в `msg`; `gtl_server_started` пишется до фактического bind.
13. Shutdown: ждёт завершения активных скачиваний, затем exit 0; второй сигнал — немедленный выход.
14. Нет backpressure-утечки в Go: при ошибке записи просто выходить (в Node обработчик висит).
15. `AGENTS.md` ошибочно описывает payload как ASCII `a` — при переносе ориентироваться на код/тесты.
