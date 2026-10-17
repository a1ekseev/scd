# Этап 0. Эталоны с TypeScript-версии (до удаления TS)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (или executing-plans). Шаги — чекбоксы. Перед задачей прочитать «Глобальные ограничения» в `README.md` этого каталога.

**Goal:** снять с работающей TS-версии воспроизводимые эталоны (выходы CLI, разбор корпуса ссылок, cron-векторы, ответы sfc/gtl, HTML/JSON status-сервера и скриншоты дашборда) и сохранить их как Go-testdata в `testdata/reference/`, чтобы этапы 1–6 сверялись с ними после удаления TS (D3, D20).

**Architecture:** временный каталог `tools/golden/` с Node-скриптами (ESM `.mjs`), которые импортируют собранные `scd/dist`, `sfc/dist`, `gtl/dist` и пишут JSON/HTML/PNG в `testdata/reference/`. Скрипты коммитятся, чтобы эталоны можно было переснять до этапа 7; в задаче 7.3 `tools/golden/` удаляется, `testdata/reference/` остаётся.

**Tech Stack:** Node 24 в контейнере `node:24-alpine` (сейчас 24.22.0; удовлетворяет `engines >=24.14.0` и совпадает с CI `node-version: 24.14.0`) — **основной вариант**, Docker в песочнице есть; запасной — локальный Node 22.22.0 (`npm ci` даёт только warning `EBADENGINE`). Chromium headless shell из `/opt/pw-browsers/chromium_headless_shell-1194/chrome-linux/headless_shell` (Playwright-бандл) — запускается на хосте (в alpine-контейнере браузера нет). Проверено ревью: в `node:24-alpine` `npm ci && npm run build && npm test` зелёные — scd 115/115, sfc 24/24, gtl 20/20; ICU-имена стран (`Intl.DisplayNames` en/ru для 45 кодов) у Node 22.22 и 24.22 совпадают.

**Spec:** `07-decisions.md` D3, D9, D20, D23, D24, D25, D26; `11-xray-vless-matrix.md` (задача 0.5); `00-overview.md` «Этап 0»; `01` §2.6, §14, §17; `03` §6, §10.4; `04` §2.1, §5; `05` §4.5; `06` §3.

## Контекст этапа

- Эталоны, зависящие от ICU (`nameEn` стран, `localeCompare`), снимаются как есть; Go их **не** обязан повторять (D9) — в `testdata/reference/README.md` пометить эти поля как «информационные» и записать версию Node, на которой сняты (`node --version`).
- Шаблон запуска Node-скриптов в контейнере (из корня репозитория; `--network host` — чтобы `npm ci` ходил в registry напрямую, он в `NO_PROXY`):
  ```bash
  docker run --rm --network host -e TZ=UTC -e HTTPS_PROXY -e NO_PROXY \
    -v /root/.ccr/ca-bundle.crt:/ca.crt:ro -e NODE_EXTRA_CA_CERTS=/ca.crt \
    -v "$PWD":/repo -w /repo node:24-alpine sh -c '<команда>'
  ```
- Время замораживать: все скрипты импортируют `tools/golden/lib/freeze-time.mjs` **до** импорта dist-модулей. Он подменяет `globalThis.Date` подклассом, у которого `new Date()`/`Date.now()` возвращают значение из env `GOLDEN_NOW` (default `2026-08-15T10:00:00.000Z`).
- `os.hostname()` не должен попадать в эталоны: во всех фикстурах задавать `statusServer.instanceId`.
- Выходные JSON писать `JSON.stringify(v, null, 2) + "\n"`; Go-тесты сравнивают их семантически.
- Каждая задача дописывает свой раздел в `testdata/reference/README.md` (что за файл, чем снят, какие поля информационные).

---

### Task 0.1: Инструменты эталонов и базовая проверка TS

**Read:** `07` D20; `00` «Этап 0»; `AGENTS.md` (корень) «Common Rules».

**Files:**
- Create: `tools/golden/package.json` (`{"private":true,"type":"module","scripts":{"all":"node run-all.mjs"}}`, без зависимостей)
- Create: `tools/golden/lib/freeze-time.mjs`, `tools/golden/lib/io.mjs` (`writeJSON(path, value)`, `writeText(path, s)`, `repoRoot`)
- Create: `tools/golden/run-all.mjs` (последовательно запускает скрипты 0.2–0.4, которые появятся позже; отсутствующие пропускает с сообщением)
- Create: `tools/golden/README.md` (как пересоздать эталоны; предупреждение «удаляется в задаче 7.3»)
- Create: `testdata/reference/README.md` (индекс), `testdata/reference/scd/fixtures/vpn-sample.txt` (копия `scd/test/fixtures/vpn-sample.txt`)
- Create: `testdata/reference/ts-baseline.txt` (вывод прогонов TS-тестов: число tests/pass/fail по сервису)

**Interfaces:**
- Produces: `freeze-time.mjs` экспортирует `FROZEN_NOW: string`; `io.mjs` — `writeJSON(relPath, value)`, `writeText(relPath, text)`, `repoRoot: string` (абсолютный корень репозитория). Все последующие скрипты этапа 0 пользуются ими.

- [ ] **Step 1: Собрать TS и прогнать TS-тесты.**
  Run (шаблон контейнера из «Контекста этапа»): `for s in scd sfc gtl; do (cd $s && npm ci --no-audit --no-fund && npm run build && npm test 2>&1 | tail -9); done`
  Expected: `ℹ fail 0` во всех трёх; `ℹ pass` = 115 (scd), 24 (sfc), 20 (gtl). Записать сводку и `node --version` в `testdata/reference/ts-baseline.txt`. Если тесты красные — зафиксировать в ts-baseline, какие и почему, и продолжить (эталоны снимаются с собранного кода).
- [ ] **Step 2: Написать `freeze-time.mjs` и самопроверку** `node -e "import('./tools/golden/lib/freeze-time.mjs').then(()=>console.log(new Date().toISOString()))"` → `2026-08-15T10:00:00.000Z`.
- [ ] **Step 3: `io.mjs`, `run-all.mjs`, README-файлы, копия фикстуры.**
- [ ] **Step 4: Проверка:** `node tools/golden/run-all.mjs` завершается с кодом 0 (пока скриптов нет — сообщает «skipped»).
- [ ] **Step 5: Commit** `chore(refs): add golden reference tooling and TS baseline` (трейлер — см. README плана).

---

### Task 0.2: Эталоны домена scd (разбор, generate, print-config, cron)

**Read:** `01` §2.6, §4–§8, §14, §17.4; `09` §6–§7; `06` §3; `07` D9, D23, D24, D26.

**Files:**
- Create: `tools/golden/scd-domain.mjs`
- Create: `testdata/reference/scd/corpus/vless-corpus.txt` — корпус строк (одна ссылка на строку; комментариев нет): все строки `docker/fixtures/subscription.txt`, `scd/test/fixtures/vpn-sample.txt`, все случаи из `01` §17.4 и §5.2/§5.3 (Example.COM/0443/Type=TCP, `:65536`, `@:443`, `ho st`, `+443`, nil UUID, uppercase UUID, `&1=2&zz=3&&=x`, `sni=a&sni=b`, `path=/a+b%2Fc%zz;x`, `[2001:0DB8:0:0::1]`, `encryption=None`, `headerType=`, `allowInsecure=`, `type=tcp` без security, reality без sid, reality без flow, tcp+tls+vision, `alpn=h2,%20http/1.1,,`), примеры `09` §6.1–§6.3 (с `?ed=2048`), ссылки 3x-ui/v2rayN с `spx`, `pqv`, `pcs`, `vcn`, `allowInsecure=1`, пустым `sid`, `fp=Chrome`, битым `pbk`, `type=xhttp` (`path`,`host`,`mode=auto`,`extra=%7B%7D`), `encryption=…` — **две валидные** клиентские строки из `xray vlessenc` бинаря 26.10.10 (вариант «X25519» — ключ 32 байта, и «ML-KEM-768» — ключ 1184 байта; брать строки `"encryption": "mlkem768x25519plus.native.0rtt.…"`; бинарь — см. «Локальный образ Xray» в README плана) плюс **одна негативная**: `mlkem768x25519plus.native.0rtt.<base64url(1184 случайных байт)>` (такой ключ проходит `conf.Build()`, но живой Xray отвергает его на `AddOutbound`: `mlkem: invalid polynomial encoding` — проверено ревью; случайные байты **не** годятся как позитивный пример), ссылки с `ech=`, `fm=`, `type=grpc`.
- Create (выход скрипта): `testdata/reference/scd/corpus/ts-parse.json` — массив `{line, raw, ts: {ok, reasonCode?, reason?, details?, label?, profile?, tag?, normalized?}}` (результат `parseSubscriptionLine` → `normalizeVless`; исключение `URIError` → `{error: "URI malformed"}`)
- Create: `testdata/reference/scd/generate/vpn-sample.{manifest,log,summary}.json`, `testdata/reference/scd/generate/docker-fixture.{manifest,log,summary}.json` (через `generateCommand` в tmp-каталог; `sourceFile` заменить на `<input>`)
- Create: `testdata/reference/scd/print-config/docker-example.json` (`loadConfig('docker/scd/config.example.yml')`, `input` заменить на путь относительно репозитория), `testdata/reference/scd/print-config/minimal.json` (конфиг из `01` §2.6; положить его в `testdata/reference/scd/print-config/minimal.yml`)
- Create: `testdata/reference/scd/config-cases.json` — массив `{name, yaml, valid, messages[]}` для кейсов `01` §17.3 S1–S14 и §17.4 «Конфиг» (сообщения информационные — D9)
- Create: `testdata/reference/common/cron-next.json` — `[{expr, from, next: [t1,t2,t3]}]` от `cron-parser` из `scd/node_modules` при `TZ=UTC`, `from = 2026-08-15T10:00:00.000Z`; выражения: `*/5 * * * *`, `*/10 * * * *`, `*/15 * * * * *`, `0 3 * * *`, `0 0 1 1 *`, `0 9 * * MON-FRI`, `30 2 1,15 * *`, `0 */2 * * *`, `5 4 * JAN,JUL SUN`, `0 0 1 * 1`, `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`, `0 0 12 * * *`
- Create: `testdata/reference/common/cron-rejected.json` — выражения, которые Go отвергает (D28: используем `robfig/cron/v3` как есть, без своих синтаксических ограничений): то, что отвергает сама библиотека — `* * * * * * *`, `0 0 L * *`, `0 0 * * 1#2`, `0 0 1W * *`, `H * * * *`, `* * * * 7`, `@minutely`, `@weekdays` (проверено ревью), плюс две защиты scd — `""` и никогда не срабатывающие `0 0 31 2 *`, `0 0 30 2 *` (robfig возвращает нулевое `Next`; `0 0 29 2 *` валиден). `?`, `@every`, `TZ=`/`CRON_TZ=` библиотека принимает — значит валидны. Ревью сверило `robfig/cron/v3` v3.0.1 с `cron-parser` 5.4.0 на всех 16 выражениях `cron-next` — `Next` совпадает 1:1. Расхождения с TS (dow `7`, `L`, `#`, `H`, `@minutely|@secondly|@weekdays|@weekends`) — в README (7.3).

- [ ] **Step 1: Корпус.** Собрать `vless-corpus.txt` (≥60 строк). Каждую строку сопроводить записью в `testdata/reference/scd/corpus/README.md`: «что проверяет» одной фразой.
- [ ] **Step 2: Скрипт `scd-domain.mjs`** (TZ=UTC внутри: `process.env.TZ='UTC'` до импорта cron-parser) пишет все файлы выше. Для `ts-parse.json` строки подавать через `scanLines(text)` целого корпуса. У `tools/golden/` нет своих `node_modules`, поэтому пакеты импортировать по пути: `import { CronExpressionParser } from '../../scd/node_modules/cron-parser/dist/index.js'` (проверено: работает из ESM), dist-модули — `../../scd/dist/...js`. `cron-next`: `CronExpressionParser.parse(expr, { currentDate: new Date(FROM) })`, три вызова `.next().toISOString()`.
- [ ] **Step 3: Run:** `TZ=UTC node tools/golden/scd-domain.mjs` → без ошибок; `git status` показывает новые файлы; `jq length testdata/reference/scd/corpus/ts-parse.json` = числу строк корпуса.
- [ ] **Step 4: Описать в `testdata/reference/README.md`:** какие поля намеренно разойдутся в Go (теги новых профилей; `jsonOutbound` целиком — новая форма v26 по D11/09 §5; тексты ошибок конфига — D9; `nameEn` — ICU).
- [ ] **Step 5: Commit** `chore(refs): capture scd domain references from TS`.

---

### Task 0.3: Эталоны sfc и gtl

**Read:** `04` §2.1, §4, §5, §11; `05` §4, §10; `docker/sfc/config.example.yml`, `docker/gtl/config.example.yml`.

**Files:**
- Create: `tools/golden/sfc.mjs`, `tools/golden/gtl.mjs`
- Create: `testdata/reference/sfc/refresh-docker-example.json` — отчёт `refreshWithConfig` по копии `docker/sfc/config.example.yml` во временном каталоге (input → абсолютный путь к `docker/fixtures/subscription.b64`); плюс `testdata/reference/sfc/outputs/<output-id>.b64` — `lastGoodBase64` каждого output и `.../<output-id>.plain.txt`
- Create: `testdata/reference/sfc/server-cases.json` — `[{name, path, userAgent?, status, contentType, body, headers}]` из `handleServerRequest(new URL(path, 'http://localhost').pathname, state, {'user-agent': ua})` (именно так его вызывает `startServer`, `sfc/src/runtime/server.ts:84-86`; без `new URL` кейс `/s/../s/test-all` даст 404 вместо 200; `state` — после `refreshWithConfig` по docker-примеру) для: `/healthz`, `/s/test-de` с UA `ClashMeta/1.0`, `/s/test-de` с UA `Shadowrocket/1.0`, `/s/test-nl`, `/s/unknown`, `/s/../s/test-all`, `/other/test-all`
- Create: `testdata/reference/sfc/print-config-docker-example.json`
- Create: `testdata/reference/gtl/payload-vectors.json` — `[{sizeBytes, sha256, first16hex}]` для 0, 1, 16, 1024, 65536, 65537, 98311, 10485760 (через `writeRepeatedPayload` в буфер) + `fill8` для offset 0, 65536, 2^32, 2^32+65536
- Create: `testdata/reference/gtl/routes.json` — `[{path, method, status, headers, body?}]` из `buildResponse(new URL(path, 'http://127.0.0.1').pathname, config)` (как в `handleRequest`, `gtl/src/runtime/server.ts:112-119`) для: `/healthz`, `/load/32`, `/load/0`, `/load/007`, `/load/1?x=y` (pathname-нормализация по `new URL`), `/load/65` (maxSizeKb 64), `/load/`, `/load/1/`, `/load`, `/load/200_32`, `/load/%33`, `/unknown`, `/load/99999999999999999999`; плюс метод HEAD → 405 — проверка метода есть только в неэкспортируемом `handleRequest`, поэтому этот кейс снимать через настоящий сервер: `startServer({...config, server:{listen:'127.0.0.1:<свободный порт>'}})` + `fetch(url, {method:'HEAD'})`, затем `server.close()`
- Create: `testdata/reference/gtl/print-config-docker-example.json`

- [ ] **Step 1:** Написать `sfc.mjs` (с freeze-time; `logger` — pino с `level: 'silent'`).
- [ ] **Step 2:** Написать `gtl.mjs`.
- [ ] **Step 3: Run:** `node tools/golden/sfc.mjs && node tools/golden/gtl.mjs`. Expected: файлы созданы; `jq -r '.[] | select(.sizeBytes==1024) | .sha256' testdata/reference/gtl/payload-vectors.json` = `8ab783947bae918d01b74448625dfd0fef119e16e97d6fc9fdf15e310c87cb01` (05 §4.5).
- [ ] **Step 4:** Раздел в `testdata/reference/README.md`.
- [ ] **Step 5: Commit** `chore(refs): capture sfc and gtl references from TS`.

---

### Task 0.4: Эталоны status-сервера: фикстуры состояния, JSON, HTML, скриншоты

**Read:** `03` §1, §4.4, §6 (полностью), §7, §10.4, §10.5; `07` D3, D22, D23; `scd/src/runtime/status-server.ts` (`handleStatusServerRequest`), `scd/src/runtime/run-state.ts`, `scd/src/runtime/status-aggregation.ts`, `scd/src/runtime/current-runtime-state.ts`, `scd/test/status-server.test.ts`, `scd/test/runtime-state.test.ts`.

**Files:**
- Create: `testdata/reference/scd/status/fixtures/<name>.json` — **общий контракт фикстуры** (его же читает Go в задаче 6.4):
  ```json
  {
    "now": "2026-08-15T10:00:00.000Z",
    "config": {
      "instanceId": "local-main",
      "subscriptions": [ {"id": "sub-1", "enabled": true, "address": "127.0.0.1:8080", "observatorySubjectSelectorPrefix": "x-observe-"} ],
      "remoteInstances": [ {"id": "edge-eu", "apiUrl": "https://edge.example.test/api/instance-status", "schedule": "* * * * *"} ],
      "runtimeState": {"enabled": true, "includeRaw": false, "includeSecrets": false}
    },
    "targets": [ {
      "subscriptionId": "sub-1", "targetAddress": "127.0.0.1:8080",
      "balancerMonitor": { "state": "healthy", "remotePingState": "ok", "consecutiveFailures": 0 },
      "tunnels": [ {
        "baseTunnelId": "vless-at-x-tcp-tls-1", "displayName": "🇦🇹 Вена", "countryIso2": "AT",
        "listen": "127.0.0.1", "port": 20000,
        "outboundTagInitial": "x-observe-vless-at-x-tcp-tls-1", "outboundTagCurrent": "x-observe-vless-at-x-tcp-tls-1",
        "outboundWithoutPrefixTag": "vless-at-x-tcp-tls-1",
        "monitor": { "state": "healthy", "lastLatencyMs": 42, "lastStatusCode": 204, "consecutiveFailures": 0 }
      } ]
    } ],
    "remotes": [ { "id": "edge-eu", "apiUrl": "https://edge.example.test/api/instance-status",
                   "lastAttemptAt": "…", "lastSuccessAt": "…", "lastError": "Remote status API returned HTTP 503.", "snapshot": { } } ]
  }
  ```
  Набор фикстур (минимум): `empty` (одна enabled подписка, нет target-state), `healthy-single`, `degraded-repaired` (03 §10.4 п.2: current=`out-a`, initial=`x-observe-out-a`), `balancer-only` (03 §10.4 п.3), `disabled-subscription` (п.4), `latency-sort` (03 §10.5 п.8: Fast/Slow/Idle/Unknown — имена ASCII, чтобы порядок ICU и `strings.Compare` совпадал, D23), `multi-sub-multi-target` (03 §10.5 п.7), `remote-instances` (03 §10.4 п.8: local + `edge-eu` с ошибкой и snapshot + `edge-us` без данных), `remote-ping-states` (pending/failed/delivered up/down), `hostile-strings` (displayName `<script>alert("x")</script> & 'q'`, remote snapshot с тем же). Все `displayName` в сортируемых группах выбирать так, чтобы ICU-порядок совпал с байтовым.
- Create: `tools/golden/status.mjs` — для каждой фикстуры: строит `LoadedConfig` (пишет временный YAML и вызывает `loadConfig`), `SyncMemoryState` (через `createSyncMemoryState`, `getOrCreateTargetState`, `replaceTargetTopology` + прямую установку monitor-state; `outboundInitial.tag`/`outboundWithoutPrefix.tag` из фикстуры), aggregation state (`createStatusAggregationState` + ручная запись remotes), затем вызывает `handleStatusServerRequest` для `/`, `/api/status`, `/api/instance-status`, `/api/instances`.
- Create (выход): `testdata/reference/scd/status/<name>/{index.html, api-status.json, api-instance-status.json, api-instances.json}`
- Create: `testdata/reference/scd/status/runtime-state/<case>.json` — `buildCurrentRuntimeStateSnapshot` с фейковым клиентом (как в `scd/test/runtime-state.test.ts`) для кейсов 03 §10.5 п.1–6 (`redacted-default`, `vision-udp443`, `repaired-fallback`, `remote-input`, `redact-monitor-urls`, `raw-and-secrets`); это эталон **формы** (ключи, классификация, diff), значения `parsed.*` для ws/fingerprint информационные (TS-декодер читал неверные поля, D2/D6)
- Create: `tools/golden/screenshots.mjs` (запускается **на хосте**, не в alpine-контейнере) — запускает `headless_shell` (путь из env `GOLDEN_CHROME`, default `/opt/pw-browsers/chromium_headless_shell-1194/chrome-linux/headless_shell`) с `--headless --no-sandbox --disable-gpu --hide-scrollbars --force-device-scale-factor=1 --window-size=<w>,2400 --screenshot=<out.png> file://<index.html>` для каждой фикстуры и ширины 1280 и 390 (`--no-sandbox` обязателен: под root без него Chromium завершается с `Running as root without --no-sandbox is not supported`, PNG не создаётся — проверено; оба размера дают PNG ровно `<w>×2400`)
- Create (выход): `testdata/reference/scd/status/<name>/screenshot-1280.png`, `screenshot-390.png`

- [ ] **Step 1:** Написать 10 фикстур JSON (контракт выше; поля monitor/balancerMonitor — имена из `03` §1).
- [ ] **Step 2:** `status.mjs`: HTML/JSON по фикстурам; `runtime-state/*` по 6 кейсам.
- [ ] **Step 3: Run:** `node tools/golden/status.mjs`. Проверка: `grep -c 'node-card' testdata/reference/scd/status/healthy-single/index.html` ≥ 1; `jq '.tunnels|length' testdata/reference/scd/status/healthy-single/api-status.json` = 1; в `hostile-strings/index.html` нет подстроки `<script>alert`.
- [ ] **Step 4:** `screenshots.mjs`; Run: `node tools/golden/screenshots.mjs`; Expected: PNG ненулевого размера на каждую фикстуру × 2 ширины. Если headless_shell недоступен — fallback: полный Chromium `/opt/pw-browsers/chromium-1194/chrome-linux/chrome` с теми же флагами; записать использованный способ в README.
- [ ] **Step 5:** Раздел в `testdata/reference/README.md`: контракт фикстуры (копия JSON-схемы выше с описанием каждого поля), что PNG — справочные (сравнение в Go идёт рендером TS-HTML и Go-HTML в одном браузере, задача 6.5).
- [ ] **Step 6: Commit** `chore(refs): capture status server HTML/JSON references and screenshots`.

---

### Task 0.5: Корпус ссылок матрицы D26 и ожидания

**Read:** `11-xray-vless-matrix.md` (весь: §3 — строки матрицы, §4 — параметры и тексты ошибок Xray, §4.1 — нестандартные параметры генераторов); `07` D15–D18, D25, D26; `09` §3.2–§3.3 (как 3x-ui/v2rayN кодируют параметры). Не зависит от TS (Node не нужен); зависит только от 0.1 (индекс `testdata/reference/README.md`).

**Files:**
- Create: `testdata/reference/scd/corpus/d26-links.txt` — по одной ссылке на строку, без комментариев
- Create: `testdata/reference/scd/corpus/d26-expect.json` — массив по строкам файла выше (контракт — ниже)
- Create: `testdata/reference/scd/corpus/d26-keys.md` — как получены ключи (команды и версия бинаря), какие строки их используют
- Modify: `testdata/reference/README.md` (раздел «Корпус D26»)

**Interfaces:**
- Produces (контракт `d26-expect.json`, его читают тесты 4.3–4.7 и 5.6):
  ```json
  [{"line": 1, "matrixRow": "3#4", "expect": "entry", "profile": "tcp-reality-vision",
    "warnings": [], "params": ["sni","fp","pbk","sid","spx","pqv"], "note": "3x-ui REALITY + vision"},
   {"line": 2, "matrixRow": "4:sid", "expect": "skip", "reasonCode": "unsupported_value",
    "stage": "xray", "expectContains": "invalid \"shortId\"", "note": "odd sid"}]
  ```
  `expect` ∈ `entry|skip`; для `skip`: `reasonCode`, `stage` ∈ `parse|scd|xray` (`parse` — `vless.Parse`, `scd` — `normalize`, `xray` — отказ `conf.Build`, ловится в манифесте), `expectContains` — подстрока `reason` (для `parse`/`scd`) или `details[0]` (для `xray`); `warnings` — коды; `params` — параметры ссылки, которые **обязаны дойти до Xray** (для `paramcheck`, 4.6/5.6): без сброшенного `flow` (`flow_cleared`), без игнорируемых (README плана O13, O16) и без `allowInsecure`; ключи — lowercase, как в `vless.Link`; `matrixRow` — ссылка на строку 11 §3 (`3#<n>`) или §4 (`4:<param>`).

- Правила корпуса:
  - Хосты только `*.example.test`, адреса TEST-NET (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), один фиксированный UUID (плюс `my-user` для D18). Порт 443/8443.
  - Ключи **настоящие**, сняты релизным бинарём Xray 26.10.10 (`SCD_XRAY_BIN`, см. README плана «Локальный образ Xray», или `scratchpad/bin-xray-26.10.10`): `xray vlessenc` → обе клиентские строки `encryption` (X25519 — 32 байта, ML-KEM-768 — 1184 байта); `xray x25519` → `Password (PublicKey)` для `pbk`; `xray mldsa65` → `Verify` для `pqv`; `xray tls ech --serverName ech.example.test` → `ECH config list` для `ech`. Случайные байты **не** годятся как позитивный пример (ревью B2): 1184 случайных байта — только негативная строка (`stage: scd`, «Invalid VLESS encryption key.»).
  - Значения кодируются так, как их кодируют генераторы (`url.QueryEscape` для `path`, `extra`, `fm`, `ech`, `spx`, `vcn`; `pcs` — в формате OpenSSL с `:` и без).
- Обязательные строки (каждая — одна ссылка; минимум 60):
  - **entry** для каждой ✓-строки 11 §3: #1, #2 (vision и vision-udp443), #3, #4, #5, #8, #9, #11 (tcp/ws/xhttp, X25519- и ML-KEM-ключ), #12 (tcp и ws с vision), #13 (tcp+tls+ML-KEM+vision, tcp+reality+ML-KEM+vision → `tcp-reality-vision`, xhttp+reality+ML-KEM+vision → `xhttp-reality-vision`), #14; `type=raw`; ML-KEM `xorpub.1rtt`, `random.0rtt`, с padding-сегментами, с двумя ключами.
  - **entry с warning** `flow_cleared`: #6 (ws+tls+vision), #10 (xhttp+reality+vision без ML-KEM); `allow_insecure_ignored` — не в этом корпусе (зависит от Policy; покрыт vpn-sample в 4.7).
  - **entry по параметрам** (11 §4): `sni`, `fp` (`Chrome` → lowercase, `randomized`, TLS `unsafe`), `alpn` (`h2,http/1.1`; `h3` на xhttp), `pcs` (два значения, с `:` и без), `vcn` (два имени), `ech` (base64 и DNS-форма), `pbk` (std-base64 с `=` → нормализуется), `sid` (пустой, `ab`), `spx` (`/abc?p=10-20&c=2`), `pqv`, `headerType=none`, `headerType=http` с `host`/`path` (tcp+tls, tcp+reality, tcp+ML-KEM), ws `path` с `?ed=2048` и `host`, ws без `path`, xhttp `mode` (все четыре), xhttp `extra` (`xPaddingBytes`, `xmux`, `downloadSettings`), `fm` (tcp `fragment` на tcp+tls и на reality+vision, `header-custom` на ws, udp `noise` + `quicParams` на xhttp `alpn=h3`), REALITY без `sni`/`fp`, свежая REALITY-ссылка 3x-ui (`spx`, `pqv`, `support-x25519mlkem768=true`, `flow=xtls-rprx-vision`), xhttp-ссылка 3x-ui (`x_padding_bytes` + `extra`), id `my-user`.
  - **entry с warning `param_ignored`** (D28): неизвестный параметр `foo=1` — ссылка принимается, параметр игнорируется.
  - **skip `stage: scd`**: `type=grpc`, `type=httpupgrade`, `type=kcp` (`unsupported_combo`), `ws+reality`, `security=none` без ML-KEM (адрес `*.example.test` — conf такой пропустил бы), `security=xtls`, `encryption=None`, ML-KEM со случайным ключом, `flow=xtls-rprx-bogus`, REALITY без `pbk`, `extra=%5B1%5D`, `fm=%7Bx`, `allowInsecure=yes`.
  - **skip `stage: xray`** (тексты — 11 §4 «Ошибка Xray»): `sid=abc`, `sid` из 18 символов, `pcs=zz`, `pcs=abcd`, `fp=bogus`, REALITY `fp=unsafe`, `spx=abc`, `pqv=AAAA`, xhttp `mode=bogus`, `extra` с `xPaddingPlacement:"nowhere"`, `extra` с `headers.Host`, `fm` с неизвестным типом маски, `headerType=bogus`, `pbk` длиной ≠32 байта, ML-KEM `foo`-режим и `2rtt`.

- [ ] **Step 1:** Снять ключи (команды выше), записать `d26-keys.md` (вывод `xray version`, команды, какие строки используют какой ключ; приватные части не нужны и не записываются).
- [ ] **Step 2:** Написать `d26-links.txt` и `d26-expect.json` по правилам выше.
- [ ] **Step 3: Verify:** `jq length testdata/reference/scd/corpus/d26-expect.json` = `grep -c . testdata/reference/scd/corpus/d26-links.txt`; `jq -e 'all(.[]; (.expect=="entry" and has("profile")) or (.expect=="skip" and has("reasonCode") and has("stage")))' …/d26-expect.json` → `true`; `jq -r '[.[]|select(.expect=="entry")|.params[]]|unique|join(",")' …/d26-expect.json` содержит все 20 параметров `paramcheck.Covered` (список — задача 4.6); `grep -vE '@[^?#]*(example\.test|192\.0\.2\.|198\.51\.100\.|203\.0\.113\.)' …/d26-links.txt` → пусто (только фейковые хосты).
- [ ] **Step 4:** Раздел в `testdata/reference/README.md`: контракт `d26-expect.json`, что ожидания взяты из `11-xray-vless-matrix.md`, а не из TS.
- [ ] **Step 5: Commit** `chore(refs): add D26 VLESS matrix corpus with real Xray keys` (body: D25, D26).



### Task 0.6: Корпус меток для фильтров (D29)

**Goal:** зафиксировать решения боевых TS-регулярок, чтобы доказать эквивалентность Go include/exclude-фильтров.

**Files:** Create `testdata/reference/common/label-filter.json`, скрипт `tools/golden/label-filter.mjs`.

- [ ] **Step 1:** Собрать ≥40 меток: варианты с/без `Extra`, `WhiteList`, `Whitelist`, `EXTRA` (регистр), оба слова в разном порядке, эмодзи-флаги, кириллица, пустая строка, а также реальные формы меток из `docker/fixtures/real-shapes/*.txt` (фрагмент после `#`, URL-decoded).
- [ ] **Step 2:** Node 24 (`docker run --rm -v $PWD:/w -w /w node:24-alpine node tools/golden/label-filter.mjs`) вычисляет для каждой метки результат трёх TS-регулярок из D29 (`en`, `extra`, `extraWhitelist`) через тот же `parseRegexLiteral`, что TS (`scd/src/config/load-config.ts`), и пишет `{label, en, extra, extraWhitelist}`.
- [ ] **Step 3:** Проверка: `jq length testdata/reference/common/label-filter.json` ≥ 40; хотя бы по одной метке на каждую комбинацию true/false для трёх фильтров.
- [ ] **Step 4: Commit** `test(reference): capture label filter decisions for D29`.
