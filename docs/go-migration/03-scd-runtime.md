# 03. scd: runtime-слой (sync, daemon, monitoring, SOCKS/HTTP, status server, logging)

Область: `scd/src/runtime/*` (кроме парсинга VLESS и gRPC-кодирования), `scd/src/logging/*` и соответствующие тесты.
Цель: переписать на Go с **точным** сохранением поведения. Все ссылки `file:line` даны относительно `scd/`.

Сокращения: `SO` = `src/runtime/sync-once.ts`, `RD` = `src/runtime/run-daemon.ts`, `MON` = `src/runtime/monitoring.ts`,
`SH` = `src/runtime/socks-http.ts`, `SS` = `src/runtime/status-server.ts`, `SA` = `src/runtime/status-aggregation.ts`,
`RS` = `src/runtime/run-state.ts`, `CRS` = `src/runtime/current-runtime-state.ts`, `LOG` = `src/logging/create-logger.ts`,
`T` = `src/types.ts`.

---

## 0. TL;DR — что легко потерять

1. **Overlap guard sync** — флаг `syncInProgress` ставится синхронно до первого `await`; второй вызов возвращает *пустой* `SyncReport` (не ошибку) и пишет `sync_skipped_overlap` (`SO:474-477`).
2. **Применяются ВСЕ kinds, если изменился хотя бы один** — `changedPlans` используется только как условие; цикл идёт по `targetPlans` (`SO:217-222`, `SO:253`).
3. **Topology/hashes заменяются только если ВСЕ kinds применились без ошибок** и хоть один plan содержит `topology` (`SO:297-299`). При замене **сбрасывается весь monitor-state туннелей в `idle`** и repaired-состояние (`RS:65-84`).
4. `preparePlanForTarget` (а в нём `buildTargetTopology`, который может бросить "portRange can allocate only N ports") вызывается **вне try** → исключение роняет **весь sync** (`sync_failed`), остальные подписки не обрабатываются (`SO:213-216`, `src/topology/build-tunnel-topology.ts:49-53`). Это не fail-closed per target. Сохранить или осознанно исправить.
5. Порядок `SyncReport.targets`: сначала **все failed-загрузки** (в порядке sort by id), потом loaded (sort by id) (`SO:412-463`, `generate-manifest-from-source.ts:57-65`).
6. `SyncReport.skipped` и `TargetSyncReport.skipped` **дублируются по числу включённых applicators** (каждый plan несёт тот же `manifest.skipped`) (`SO:435`, `SO:243`).
7. `manifest_built.managedCount` для inbound/routing = 0 (берётся из не-target plan, где `managedIds: []`) (`SO:453`, `inbound-applicator.ts:51`).
8. Cron-loop: первый тик **сразу при старте**; следующий слот считается **от предыдущего запланированного слота, а не от now** (нет дрейфа); при overrun слот пропускается с warn (`RD:38-93`).
9. Shutdown по SIGINT/SIGTERM **не прерывает ожидание таймера**: каждый loop выходит только после наступления своего следующего слота (`RD:74-88`). `Promise.race` → status server закрывается, когда закончится *первый* loop.
10. Node `setTimeout` > 2^31-1 мс (~24.8 дня) срабатывает через 1 мс → cron с редким расписанием (`0 0 1 1 *`) фактически запустит тик почти сразу. Баг Node-реализации; в Go воспроизводить не нужно (решение зафиксировать).
11. Monitor: retry **внутри** worker-слота `maxParallel`; repair только после исчерпания попыток; успешный probe → попытка **rejoin** в balancer (`MON:169-228`, `MON:452-523`).
12. Monitor-state обновляется **вне** target-mutex; repair/rejoin — **под** target-mutex, общим с sync apply (`MON:270`, `MON:362`, `SO:247`).
13. После успешного repair `consecutiveFailures` **обнуляется** и state=`degraded` с фиксированным текстом ошибки (`MON:296-307`).
14. Remote Push (remotePing) — **fire-and-forget**, не ожидается тиком; overlap-guard по `remotePingState==='pending'` (`MON:605-767`).
15. `buildRemotePingUrl`: `URLSearchParams.set` сохраняет позицию существующего ключа, `ping` удаляется и добавляется в конец; кодирование form-urlencoded (`пробел→+`, `,→%2C`). **`url.Values.Encode()` в Go сортирует ключи — нельзя использовать** (`MON:89-98`, тесты `monitoring.test.ts:1619-1635`).
16. SOCKS/HTTP latency измеряется **только** от записи HTTP-запроса до получения `\r\n\r\n` (без TCP/SOCKS/TLS handshake) (`SH:357-372`).
17. Таймауты в `socks-http` — это **idle-timeout сокета** (`socket.setTimeout`), а не общий дедлайн; ставится заново на каждой фазе (`SH:148`, `SH:243`).
18. Роутинг status server — **точное сравнение `request.url` (включая query)**; `/api/status?x=1` отдаёт **HTML**. Метод не проверяется. `/api/runtime-state*` — prefix-match (`SS:550-637`).
19. Каждый HTTP-запрос к status server (любой путь) пересобирает локальный snapshot и **сохраняет его** в aggregation state (`SS:535-546`).
20. JSON-ответы: `JSON.stringify(x, null, 2)`, без завершающего `\n`; `undefined`-поля опускаются; порядок ключей = порядок вставки. В Go: `SetIndent("", "  ")`, `SetEscapeHTML(false)`, обрезать `\n` от `Encoder`, `omitempty` + указатели.
21. Сортировки используют `String.prototype.localeCompare` (ICU-коллация, не байтовая) — для Go нужен `golang.org/x/text/collate`.
22. Remote snapshot хранится **как распарсенный JSON целиком** (неизвестные поля проходят насквозь в `/api/instances`) после неглубокой валидации (`SA:41-97`).
23. HTML-экранирование — собственное: `& < > "` (одинарная кавычка НЕ экранируется) (`SS:46-52`). `html.EscapeString` в Go экранирует `'` → нельзя.

---

## 1. Модель состояния (in-memory)

Источник: `RS:11-63`, `T:333-398`.

```
SyncMemoryState
├── syncInProgress: bool                      // overlap guard sync (только этот процесс)
├── targets: map[targetKey]*TargetApplyState  // targetKey = "<subscriptionId>::<targetAddress>" (RS:52-54)
└── targetLocks: map[targetKey]Promise        // цепочка промисов = FIFO-mutex на target (RS:106-128)

TargetApplyState
├── resources: map[ResourceKind]{manifestHash}   // kind ∈ outbound|inbound|routing
├── topologyGeneration: int                      // ++ при каждой замене topology; больше нигде не читается
├── topology?: TargetTopology                    // {tunnels: TunnelMapping[]}
├── tunnels: map[baseTunnelId]TunnelRuntimeState // ПОРЯДОК ВСТАВКИ = порядок topology.tunnels (важно для monitor)
└── balancerMonitor: TargetBalancerMonitorState

TunnelRuntimeState = { tunnel: TunnelMapping, monitor: TunnelMonitorState }
```

`TunnelMapping` (`T:339-352`, строится в `src/topology/build-tunnel-topology.ts:55-75`):
`baseTunnelId` (= entry.tag), `displayName` (= entry.label), `countryIso2?`, `baseOutboundTag`, `outboundTagInitial` (= prefix+tag),
`outboundTagCurrent` (изначально = initial; меняется repair/rejoin), `inboundTag` = `in-<id>`, `routeTag` = `route-<id>`,
`listen` (= `inboundSocks.listen`), `port` (= `portRange.start + index`), `outboundInitial`, `outboundWithoutPrefix` (`{tag, normalized, jsonOutbound}`).

`TunnelMonitorState` (`T:358-371`): `state ∈ healthy|degraded|repairing|idle`, `lastCheckedAt?`, `lastSuccessAt?`, `lastFailureAt?`,
`lastStatusCode?`, `lastLatencyMs?`, `lastSuccessStatusCode?`, `lastSuccessLatencyMs?`, `lastError?`, `consecutiveFailures`.
Начальное: `{state:'idle', consecutiveFailures:0}` (`RS:29-34`).

`TargetBalancerMonitorState` (`T:373-393`): `state ∈ idle|healthy|degraded`, `remotePingState ∈ idle|pending|ok|failed`,
`remotePingLastReportedStatus? ∈ up|down`, `remotePingLastReportedMsg?`, `remotePingLastReportedPingMs?`, `lastCheckedAt?`, `lastSuccessAt?`,
`lastFailureAt?`, `lastStatusCode?`, `lastLatencyMs?`, `lastSuccessStatusCode?`, `lastSuccessLatencyMs?`, `lastError?`, `remotePingLastRunAt?`,
`remotePingLastSuccessAt?`, `remotePingLastFailureAt?`, `remotePingLastStatusCode?`, `remotePingLastError?`, `consecutiveFailures`.
Начальное: `{state:'idle', remotePingState:'idle', consecutiveFailures:0}` (`RS:36-42`).

Все timestamps — `new Date().toISOString()` → формат `2026-01-01T00:00:00.000Z` (UTC, ровно 3 знака миллисекунд). В Go: `t.UTC().Format("2006-01-02T15:04:05.000Z")`.

`getOrCreateTargetState` создаёт состояние лениво (`RS:56-63`). Monitor/balancer тики **не создают** состояние: если `memoryState.targets[key]` нет — тик молча выходит (`MON:440-444`, `MON:537-541`). То есть до первого успешного sync мониторинг — no-op. Status server тоже не создаёт.

Нюанс: `replaceTargetTopology` **мутирует существующий объект** `TargetApplyState` (`RS:65-84`), поэтому ссылка `targetState`, захваченная monitor-тиком до начала sync, остаётся валидной и после замены topology; `setTunnelMonitorState` ищет туннель по id в *текущем* `tunnels` и молча игнорирует отсутствующие (`RS:93-104`). В Go хранить `*TargetApplyState` в map и мутировать на месте.

`balancerMonitor` **не** сбрасывается при замене topology (только `tunnels`, `resources`, `topology`, `topologyGeneration`).

### 1.1 target mutex (`withTargetMutationLock`, `RS:106-128`)
FIFO-цепочка промисов на ключ: каждый вызов ждёт предыдущего (ошибки предыдущего игнорируются), выполняет `fn`, освобождает; ключ удаляется из map, если хвост цепочки — он сам. Семантика = **per-target mutex, FIFO, не реентерабельный**.
Go: `map[string]*sync.Mutex` под глобальным `sync.Mutex` (ключи можно не удалять) — `sync.Mutex` в Go не гарантирует FIFO, но в коде порядок не важен (проверяется только взаимное исключение, тест `monitoring.test.ts:134`). Повторный захват внутри — дедлок (в TS тоже дедлок), не делать.

Используют mutex: apply в sync (`SO:247`), `repairTunnel` (`MON:270`), `rejoinTunnelToBalancer` (`MON:362`). **Не** используют: обновления monitor-state в тиках, balancer monitor, remote ping, status server (чтение).

---

## 2. Sync (`syncWithConfig` / `performSync`)

### 2.1 Поток

```
syncWithConfig(cfg, logger, mem, services)                         SO:468-489
 ├─ if mem.syncInProgress → warn sync_skipped_overlap; return EmptySyncReport()   (не ошибка!)
 ├─ mem.syncInProgress = true
 ├─ try performSync
 │   catch(e) → error sync_failed {error: e.message}; rethrow
 └─ finally syncInProgress = false

performSync                                                         SO:386-466
 1. startedAt = now; info sync_started {sourceCount = len(config.subscriptions)}   // ВКЛЮЧАЯ disabled!
 2. result = loadSubscriptionsFn(config.subscriptions)
      // только enabled, сортировка id.localeCompare || input.localeCompare, последовательно (generate-manifest-from-source.ts:61-93)
      // ошибка загрузки → в failed[] (не исключение)
 3. info subscription_fetched {subscriptions:[{id,source,encoding,targetAddress}]}  // только loaded; source = URL без редактирования!
 4. for failed in result.failed:   → buildFailedSubscriptionTargetReports            SO:347-384
      error subscription_fetch_failed {subscriptionId, source, error, targetAddress}
      для КАЖДОГО enabled applicator: FailedApplyReport(kind, …, message=error); лог apply_failed
      TargetSyncReport{sourceId = subscription.id, unchangedKinds:[], skipped:[]}
 5. for sub in result.loaded:
      try builtPlans = [a.buildPlan(sub) for a in enabled applicators]   // порядок: outbound, inbound, routing (SO:55-58)
      catch → buildManifestFailureTargetReports (error manifest_validation_failed + apply_failed на каждый kind); continue
      skippedEntries += flatten(plan.skipped)                           // дубли по числу applicators
      для каждого plan: info manifest_built {...}
      targetReports += syncSubscriptionTarget(...)
 6. return aggregateReports(targetReports, skippedEntries, startedAt)
```

Fail-closed: пустой ввод / `parsed===0` / все entries отфильтрованы → `generateManifestFromSubscription` бросает `ManifestValidationError` в `buildPlan` → target помечается failed, **никаких** Xray API вызовов (тесты `service.test.ts:1476`, `:1569`). Сообщение: `Subscription source "<id>" produced no valid outbound entries. parsed=… skipped=… filtered=… filteredByCountry=… filteredByLabelRegex=… unsupportedScheme=… unsupportedParam=… unsupportedValue=… unsupportedCombo=… invalidUri=… missingRequiredField=… duplicateTag=…` (`generate-manifest-from-source.ts:31-48`).

### 2.2 `syncSubscriptionTarget` (`SO:204-310`)

```
targetKey = sub.id + "::" + target.address
ts = getOrCreateTargetState(mem, targetKey)
targetPlans = [ {a, a.preparePlanForTarget(plan, target)} ]   // ВНЕ try; может бросить → весь sync падает
changed = targetPlans.filter(p => !ts.resources[p.kind] || ts.resources[p.kind].manifestHash != p.manifestHash)
if len(changed)==0:
    для каждого kind: info skipped_no_changes
    return TargetSyncReport{unchangedKinds = все kinds, resources: [], skipped: flatten(builtPlans.skipped)}
LOCK(targetKey):
    for p in targetPlans (ВСЕ, не только changed), последовательно:
        info resource_apply_started
        try  report = a.applyPlan(p, {subscriptionId, target}); append; logApplyResult
             if report.failed>0 → failed=true
             else hashes[kind]=p.manifestHash; if p.topology → nextTopology = p.topology  // последний выигрывает (routing)
        catch e → FailedApplyReport(kind, …, skipped=p.skipped, message=e.message); logApplyResult; failed=true
        // ошибка одного kind НЕ останавливает применение следующих
    if !failed && nextTopology → replaceTargetTopology(ts, nextTopology, hashes)
    return TargetSyncReport{unchangedKinds: [], resources: reports, skipped: flatten(builtPlans.skipped)}
```

`sourceId` в TargetSyncReport = `builtPlans[0].plan.sourceId ?? subscription.id`.

Следствия:
- При частичной ошибке хэши **не сохраняются** вообще → следующий тик снова применит всё (retry через «изменённость»).
- `topology` с пустым `tunnels` (нет `inboundSocks`) — truthy объект → замена произойдёт, хэши сохранятся.
- Если ни один applicator не включён: `targetPlans=[]` → ветка «unchanged» с `unchangedKinds: []`.
- Monitor-state туннелей и repaired-теги сбрасываются при **любом** успешном полном apply (даже если менялся только outbound-хэш).

### 2.3 `replaceTargetTopology` (`RS:65-84`)
`topologyGeneration++`; `topology = new`; `resources = {kind: {manifestHash}}` (только хэши этого прогона); `tunnels = ordered map baseTunnelId → {tunnel, monitor: idle}`.

### 2.4 Схемы отчётов

`ApplyReport` (`T:151-166`) — порядок ключей при сериализации (`sync` выводит в stdout):
```json
{
  "kind": "outbound",
  "sourceId": "source-1",
  "subscriptionId": "source-1",
  "targetAddress": "127.0.0.1:8080",
  "appliedAt": "2026-01-01T00:00:00.000Z",
  "durationMs": 0,
  "added": 0, "replaced": 0, "removed": 0, "failed": 1,
  "deletedIds": [], "appliedIds": [],
  "items": [ { "id": "outbound", "status": "failed", "message": "Input source timed out" } ],
  "skipped": []
}
```
(выше — `createFailedApplyReport`, `SO:78-108`: `items[0].id = kind`, `durationMs:0`). Порядок ключей отчётов от реальных applicators определяется в `src/apply/apply-*.ts` (вне области этого документа).

`TargetSyncReport` (`SO:110-144`): `{subscriptionId, targetAddress, sourceId, unchangedKinds, failed, resources, skipped}`; `failed = Σ resources[].failed`.

`SyncReport` (`SO:146-159`, `T:178-188`):
```json
{
  "appliedAt": "2026-01-01T00:00:01.234Z",
  "durationMs": 1234,
  "added": 0, "replaced": 0, "removed": 0,
  "unchanged": 1,
  "failed": 0,
  "targets": [ /* TargetSyncReport */ ],
  "skipped": [ /* SkippedEntry, с дублями */ ]
}
```
- `unchanged` = Σ `len(target.unchangedKinds)` (число пропущенных resource-plans, не outbounds).
- `added/replaced/removed/failed` = суммы по всем `ApplyReport`.
- `appliedAt` берётся **в момент агрегации** (конец), `durationMs = now - startedAt`.
- Пустой отчёт overlap (`SO:64-76`): все нули, `targets: []`, `skipped: []`, `appliedAt=now`.

CLI `sync`: `JSON.stringify(report, null, 2) + "\n"` в stdout, `exitCode = failed>0 ? 1 : 0` (`src/cli.ts:26-33`). Ошибка (исключение) → stderr `message\n`, exit 1 (`src/cli.ts:67-70`). `syncOnce` логирует `config_loaded` и использует свежий memory state (`SO:491-496`).

---

## 3. Daemon (`run-daemon.ts`)

### 3.1 Запуск (`RD:142-244`)
```
loadConfig → createLogger → mem = new SyncMemoryState; agg = createStatusAggregationState(remoteInstances)
if runtime.mode != 'daemon' → throw "Config at <configPath> is not in daemon mode."   (до config_loaded)
stopping=false; process.once(SIGINT|SIGTERM, stop)   // once: второй сигнал = дефолтное поведение (немедленный kill)
info config_loaded {configPath}
if statusServer.enabled && statusServer.listen → startStatusServer(...)  (ошибка listen → исключение → выход)
loops = [
  cron(runtime.schedule): info daemon_tick; syncWithConfig(cfg, logger, mem)
      overrun: daemon_tick_skipped_overrun / fail: sync_failed "Daemon sync failed."
  для КАЖДОЙ subscription в config (включая disabled!):
     if monitor.enabled && monitor.schedule: cron → runTargetMonitorTick
         overrun monitor_tick_skipped_overrun / fail monitor_tick_failed, context {subscriptionId, targetAddress}
     if balancerMonitor.enabled && balancerMonitor.schedule: cron → runTargetBalancerMonitorTick
         overrun balancer_monitor_tick_skipped_overrun / fail balancer_monitor_tick_failed, context {subscriptionId, targetAddress}
  для каждого statusServer.remoteInstances: cron(remote.schedule) → collectRemoteStatus
         overrun remote_status_tick_skipped_overrun / fail remote_status_tick_failed, context {remoteInstanceId, apiUrl}
]
await Promise.race(loops)
finally: stopping=true; statusServer.close()
```
Конфиг загружается один раз; hot-reload нет.

Замечание: при ошибке в sync лог `sync_failed` пишется **дважды**: из `syncWithConfig` (`{event, error}`, msg `Sync failed.`) и из `runLoggedCronLoop` (`{event, error, causes?}`, msg `Daemon sync failed.`).

### 3.2 `runCronLoop` (`RD:38-93`) — точный алгоритм

```
tickInProgress=false; activeTask=nil
scheduledAt = now()
startTask()                    // ПЕРВЫЙ ТИК НЕМЕДЛЕННО, не ждёт слота cron
while !stopping:
    scheduledAt = next(schedule, scheduledAt)        // от предыдущего СЛОТА → без дрейфа
    sleep(max(0, scheduledAt - now()))
    if stopping: break
    if tickInProgress: onOverrun(scheduledAt); continue   // слот пропущен, не ставится в очередь
    startTask()
await activeTask (если есть)
```
`startTask` синхронно ставит `tickInProgress=true` и запускает task асинхронно; в `finally` сбрасывает флаг.

Свойства:
- Не более одного выполнения task на loop одновременно; разные loops независимы и параллельны (sync, monitor и balancer одного target могут идти одновременно; координация только через target-mutex).
- Если процесс «проспал» несколько слотов (долгий tick, suspend), цикл быстро прокручивает прошедшие слоты с delay=0: первый свободный — запуск, далее overrun-предупреждения (или повторные запуски, если task очень быстрый).
- `next()` cron-parser возвращает момент **строго после** `currentDate`; первый `scheduledAt` содержит миллисекунды now.
- Тест `run-daemon.test.ts:19` фиксирует: слоты 0(старт),100,200,300; первый тик висит до первого overrun → `startedAt=[0,200]`, `overruns=[100]`.

`runLoggedCronLoop` (`RD:95-140`): оборачивает task в try/catch → `logger.error({...context, event: failureEvent, error, causes?}, failureMessage)`; overrun → `logger.warn({...context, event: overrunEvent, scheduledAt: ISO}, overrunMessage)`, затем вызывает пользовательский `onOverrun`. Ошибка task **не** прерывает loop.

`getErrorLogDetails` (`RD:22-36`): `message`; если `AggregateError` — `causes = errors.map(message).filter(nonEmpty)`, поле `causes` добавляется только если непусто.

### 3.3 Cron-синтаксис (cron-parser `^5.4.0`)
- Вызов: `CronExpressionParser.parse(schedule, { currentDate })` (`RD:18-20`); валидация при загрузке конфига `CronExpressionParser.parse(schedule)` (`src/config/load-config.ts:261,310,354,430`).
- **Timezone не задаётся** → используется локальная TZ процесса (`TZ` env). В Docker-образе `node:24-alpine` TZ не задана → UTC. В Go: `time.Local` (и задокументировать; в контейнере тоже UTC).
- cron-parser v5 поддерживает: 5 полей (`min hour dom mon dow`) **или 6 полей с ведущими секундами**; `*`, списки `,`, диапазоны `-`, шаги `/`, имена месяцев/дней (`JAN`, `MON`), `?`, `L`, `W`, `#`, предопределённые `@yearly/@annually/@monthly/@weekly/@daily/@hourly` (+ ряд других алиасов v5 — сверить с документацией конкретной версии lock-файла). DOM/DOW — семантика OR, если оба ограничены. Фактически в README/примерах/тестах используется только `*/N * * * *` (`README.md:100,109,146,193`).
- Go-рекомендация: `github.com/robfig/cron/v3` с `cron.NewParser(cron.SecondOptional|cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow|cron.Descriptor)` — не поддерживает `L/W/#`; либо `github.com/adhocore/gronx` (поддерживает `L/W/#`). Требование: валидатор конфига и планировщик должны использовать **один** парсер; несовместимые выражения — явная ошибка конфигурации. Написать golden-тесты `next()` для типовых выражений, сверенные с cron-parser.

### 3.4 Таймеры и shutdown
- `wait(ms)` = `setTimeout`; Node ограничивает задержку 2^31-1 мс, больше → 1 мс (см. TL;DR п.10).
- Сигнал только выставляет флаг; спящий loop проснётся в свой слот, увидит `stopping` и выйдет, дождавшись `activeTask`. `Promise.race` завершится, когда выйдет первый loop → `finally` закрывает status server (`server.close` ждёт завершения активных соединений). Остальные loops продолжают спать до своего слота; процесс завершается, когда опустеет event loop (включая fire-and-forget remote ping). Docker `stop` (SIGTERM + 10s → SIGKILL) практически всегда убивает процесс по SIGKILL.
- Если сам `runCronLoop` бросит (невалидный cron — невозможно после валидации) → `runDaemon` reject → CLI печатает ошибку, exit 1.

### 3.5 Маппинг на Go
```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
// каждый loop — goroutine:
func runCronLoop(ctx, sched, task, onOverrun) {
    var running atomic.Bool; var wg sync.WaitGroup
    start := func(){ running.Store(true); wg.Add(1); go func(){ defer wg.Done(); defer running.Store(false); task(ctx) }() }
    at := clock.Now(); start()
    for {
        at = sched.Next(at)
        select { case <-ctx.Done(): goto out; case <-clock.After(max(0, at.Sub(clock.Now()))): }
        if ctx.Err()!=nil { break }
        if running.Load() { onOverrun(at); continue }
        start()
    }
out: wg.Wait()
}
```
- Инжектировать `clock` (now/after) и `nextFn` для тестов (аналог `nowFn/waitFn/getNextDateFn`).
- Решение по совместимости: прерывать ожидание по `ctx.Done()` (быстрый shutdown) — сознательное улучшение; in-flight задачи дожидаться. Task получает ctx, но в TS задачи не отменяются → для точной семантики задачи **не** отменять по сигналу (или отменять только I/O после grace period). Зафиксировать решение.
- `Promise.race` → в Go: дождаться всех loops (`errgroup` без отмены по ошибке) — разница только в моменте закрытия status server.
- Второй сигнал: `signal.NotifyContext` после `stop()` возвращает дефолтное поведение — аналог `process.once`.

---

## 4. Monitoring (`monitoring.ts`)

### 4.1 Per-tunnel monitor tick (`runTargetMonitorTick`, `MON:429-524`)

```
if !(monitor.enabled && monitor.request) → return
ts = mem.targets[key]; if nil → return                       // до первого sync — no-op
snapshot = values(ts.tunnels)  (порядок вставки; снимок на старте тика)
runWithConcurrencyLimit(snapshot, monitor.maxParallel, worker)
worker(rs):
    proxyHost = resolveProxyHost(rs.tunnel.listen, target.address)
    try resp = requestWithRetry(SOCKS rs.tunnel.port, monitor.request, monitor.retry, event monitor_check_retry)
    catch err:
        setTunnelMonitorState(ts, id, m => {...m, state:degraded, lastCheckedAt=lastFailureAt=now,
                                       lastStatusCode=nil, lastLatencyMs=nil, lastError=err.message,
                                       consecutiveFailures=m.consecutiveFailures+1})   // lastSuccess* сохраняются
        await repairTunnel(...)          // исключения отсюда → в AggregateError тика
        return
    setTunnelMonitorState(ts, id, m => {...m, state:healthy, lastCheckedAt=lastSuccessAt=now,
                                   lastStatusCode=lastSuccessStatusCode=resp.statusCode,
                                   lastLatencyMs=lastSuccessLatencyMs=resp.latencyMs,
                                   lastError=nil, consecutiveFailures=0})
    try await rejoinTunnelToBalancer(...)
    catch err → setTunnelMonitorState(lastError = "Tunnel healthy but balancer rejoin failed: <msg>");
                error tunnel_rejoin_failed      // только если rejoinFn бросил (реальный rejoin сам ловит свои ошибки)
```

`resolveProxyHost` (`MON:53-59`): если `listen` = `0.0.0.0` или `::` → `targetAddress.split(':')[0]` (ломается для IPv6-адресов target — сохранить как есть или задокументировать), иначе `listen`.

`runWithConcurrencyLimit` (`MON:230-258`): `concurrency = max(1, min(maxParallel, n))` воркеров; общий индекс; воркер берёт следующий элемент, пока есть. Если `worker` бросил — **этот воркер завершается**, остальные продолжают разбирать очередь. После завершения всех — если были ошибки: `AggregateError(reasons, 'Concurrent task execution failed.')`. Пустой список — сразу выход.
Go: N горутин, общий `atomic.Int64` индекс, сбор ошибок под mutex, `errors.Join`-подобный тип с полем `Causes []string` для лога (сообщение верхнего уровня `Concurrent task execution failed.`).

Probe-retry (`executeRequestViaSocksWithRetry`, `MON:169-228`):
- `attempts = max(1, retry.attempts ?? 1)`, `delayMs = max(0, retry.delayMs ?? 250)` (`MON:148-153`; схема конфига: default `{attempts:1, delayMs:250}`, `load-config.ts:23-26`).
- Попытка неуспешна, если исключение ИЛИ `statusCode != expectedStatus` (ошибка `Expected HTTP <exp>, got <got>.`).
- Между попытками (не после последней): `debug {event: retryEvent, ...logContext, attempt, attempts, delayMs, statusCode? (если ответ был), error}` и `sleep(delayMs)` (0 → без ожидания).
- После исчерпания бросается **последняя** ошибка.
- Retry выполняется внутри слота воркера → maxParallel ограничивает и повторы (тест `monitoring.test.ts:480`).

### 4.2 Repair (`repairTunnel`, `MON:260-339`)
```
LOCK(targetKey):
  cur = ts.tunnels[rs.tunnel.baseTunnelId]; if nil → return       // topology сменилась
  cur.monitor.state = 'repairing'
  client = createMutationClient(target)                             // вне try
  try:
     removeRule(cur.tunnel.routeTag)            (ошибки игнорируются)
     removeOutbound(cur.tunnel.outboundTagCurrent) (ошибки игнорируются)
     repaired = {...cur.tunnel, outboundTagCurrent: outboundWithoutPrefix.tag}
     addOutbound(buildOutboundGrpc(outboundWithoutPrefix.normalized).raw)
     addRule(buildRoutingGrpc(repaired).raw)
     ts.tunnels[id] = {tunnel: repaired, monitor: {...cur.monitor, state:degraded, lastStatusCode:nil,
                       lastLatencyMs:nil, consecutiveFailures:0,
                       lastError:"Tunnel repaired; awaiting next successful monitor check."}}
     warn tunnel_repaired {subscriptionId, targetAddress, tunnelId, outboundTag}
  catch e:
     monitor: state degraded, lastFailureAt=now, lastError=e.message   (tunnel не меняется)
     error tunnel_repair_failed {subscriptionId, targetAddress, tunnelId, error}
```
Repair выполняется и без prefix (тогда «пересоздание» того же outbound). Последовательность вызовов фиксирована тестом `monitoring.test.ts:548`: `removeRule(route) → removeOutbound(initialTag) → addOutbound(unprefixed) → addRule(route->unprefixed)`.

### 4.3 Rejoin (`rejoinTunnelToBalancer`, `MON:341-427`)
`shouldRejoin(rs) = current != initial && initial != withoutPrefix.tag` (т.е. есть prefix и туннель сейчас на unprefixed). Проверка до lock (по снимку) и повторно под lock (по текущему).
```
LOCK:
  cur = ts.tunnels[id]; if nil || !shouldRejoin(cur) → return
  client = createMutationClient(target)
  try:
    rejoined = {...cur.tunnel, outboundTagCurrent: outboundTagInitial}
    addOutbound(build(outboundInitial.normalized))        // ошибка → сразу в outer catch, без отката
    try:
       removeRule(route) (ignore); addRule(build(rejoined)); removeOutbound(cur.outboundTagCurrent) (ignore)
    catch e:   // откат, все шаги best-effort
       removeRule(route) (ignore); addRule(build(cur.tunnel)) (ignore); removeOutbound(rejoined.outboundTagCurrent=initial) (ignore)
       throw e
    ts.tunnels[id] = {tunnel: rejoined, monitor: {...cur.monitor, lastError: nil}}
    info tunnel_rejoined_balancer {subscriptionId, targetAddress, tunnelId, outboundTag}
  catch e:
    monitor.lastError = "Tunnel healthy but balancer rejoin failed: <msg>"   (state остаётся healthy)
    error tunnel_rejoin_failed {..., error}
```
Если туннель уже в balancer — mutation client **даже не создаётся** (тест `:737`).

### 4.4 Состояния туннеля (диаграмма)

```
            replaceTopology (sync)
   ┌──────────────────────────────────────────┐
   ▼                                          │
 [idle] ──probe ok──► [healthy] ◄─────────────┼──probe ok (+rejoin если на fallback)
   │                     │                    │
   │ probe fail          │ probe fail          
   ▼                     ▼                    
 [degraded] (consecutiveFailures+1, lastError=probe error)
   │  repair (под mutex)
   ▼
 [repairing] ──ok──► [degraded] (outboundTagCurrent=unprefixed, cf=0, lastError="Tunnel repaired; …")
       └──fail──► [degraded] (lastFailureAt, lastError=repair error)
```
`balanced` (`RS:130-146`) — вычисляемое поле статуса: `unprefixed = outboundWithoutPrefix.tag || baseOutboundTag`; если пусто или `initial == unprefixed` → `undefined`; если `initial` не оканчивается на `unprefixed` → `undefined`; `prefix = initial[:len-len(unprefixed)]`; пустой → `undefined`; иначе `current.startsWith(prefix)`.

### 4.5 Balancer monitor (`runTargetBalancerMonitorTick`, `MON:526-603`)
Observe-only: без repair/apply, без mutex.
```
if !(enabled && socks5 && request) → return; ts nil → return
try resp = requestWithRetry(socks5.host:port, request, retry, event balancer_monitor_check_retry, ctx {subscriptionId,targetAddress})
catch err:
    bm = {...bm, state:degraded, lastCheckedAt=lastFailureAt=now, lastStatusCode=nil, lastLatencyMs=nil,
          lastError=msg, consecutiveFailures+1}
    scheduleRemotePing({status:'down', msg: msg})
    return
bm = {...bm, state:healthy, lastCheckedAt=lastSuccessAt=now, lastStatusCode=lastSuccessStatusCode=code,
      lastLatencyMs=lastSuccessLatencyMs=latency, lastError=nil, consecutiveFailures=0}
scheduleRemotePing({status:'up', msg:'OK', pingMs: latency})
```
Сам успех/провал balancer-проверки **не логируется** (кроме debug retry и логов remote ping).

### 4.6 Remote Push (`scheduleRemotePing`, `MON:605-767`)
```
if !(remotePing.enabled && remotePing.url && socks5) → return
if bm.remotePingState == 'pending' → warn balancer_monitor_remote_ping_skipped_overrun; return
bm += {remotePingState:'pending', remotePingLastRunAt: now, remotePingLastError: nil,
       remotePingLastReportedStatus, remotePingLastReportedMsg, remotePingLastReportedPingMs (nil для down)}
info balancer_monitor_remote_ping_started
go (не ожидается):
   url = buildRemotePingUrl(remotePing.url, payload)
   resp = viaSocks ? requestViaSocks(socks5, url, GET, remotePing.timeoutMs) : pushRemotePingDirect(url, timeoutMs)
   if code ∉ [200,300): bm += {failed, remotePingLastFailureAt, remotePingLastStatusCode: code,
                              remotePingLastError: "Remote ping returned HTTP <code>."}; warn ..._failed (с statusCode)
   else: bm += {ok, remotePingLastSuccessAt, remotePingLastStatusCode: code, remotePingLastError: nil}; info ..._succeeded
   catch e: bm += {failed, remotePingLastFailureAt, remotePingLastStatusCode: nil, remotePingLastError: e.message}; warn ..._failed (без statusCode)
```
Ошибка push **не меняет** `state` balancer. `remoteHost = new URL(remotePing.url).host` (или `<invalid>`); `proxyHost/proxyPort` в логах только при `viaSocks`.

`buildRemotePingUrl` (`MON:89-98`): WHATWG `URL` + `searchParams.set('status')`, `.set('msg')`, `.delete('ping')`, `.set('ping')` если есть. Точная семантика `URLSearchParams`:
- `set` заменяет первое вхождение **на его месте**, удаляет остальные дубли; если ключа нет — добавляет в конец.
- Сериализация `application/x-www-form-urlencoded`: пробел → `+`, не экранируются только `*-._` и алфанумерика; `,` → `%2C`, `.` остаётся.
- При любом изменении `searchParams` **весь** query пересериализуется (существующие параметры перекодируются в form-encoding).
- Примеры (тесты): `…/token?keep=1&status=down&msg=old&ping=999` + up/OK/42 → `…/token?keep=1&status=up&msg=OK&ping=42`; `…?keep=1&ping=999` + down/failed → `…?keep=1&status=down&msg=failed`; msg `Expected HTTP 204, got 200.` → `msg=Expected+HTTP+204%2C+got+200.`
- Go: свой ordered-query (слайс пар), парсинг `url.ParseQuery`-подобный с сохранением порядка, кодирование `url.QueryEscape` (он даёт `+` для пробела и `%2C`; совпадает с form-encoding для ASCII, проверить `~`: QueryEscape не экранирует `~`, а URLSearchParams экранирует → `%7E`). Fragment сохраняется `URL.toString()`.

`pushRemotePingDirect` (`MON:108-133`): `fetch(url, GET)` с `AbortController` по `timeoutMs`; редиректы следуются (fetch default); `statusCode = response.status`; тело **не читается**, `body.cancel()`. Таймаут → `Remote ping timed out after <ms>ms.`; прочие ошибки — сообщение undici (`fetch failed`). Go: `http.Client{Timeout}` + `resp.Body.Close()` без чтения; текст сетевых ошибок будет отличаться.

### 4.7 Конкурентность monitoring в Go
- Хранить `SyncMemoryState` под одним `sync.RWMutex` (для полей state) + отдельные per-target mutex для Xray-мутаций. Все `set*MonitorState`, чтение status server, `replaceTargetTopology` — под state-lock (короткие критические секции, без I/O). В TS это неявно атомарно благодаря однопоточности; в Go обязательно.
- Snapshot туннелей на старте тика копировать (слайс указателей/ID).
- Remote ping: `go func(){...}()`; переход `pending` выполнять под state-lock (check-and-set атомарно).
- Fire-and-forget горутины учитывать в `WaitGroup` для graceful shutdown (опционально).

---

## 5. SOCKS5 + HTTP клиент (`socks-http.ts`)

API: `requestViaSocks({proxyHost, proxyPort, url, method: GET|HEAD|POST, timeoutMs}) → {statusCode, latencyMs, bodyBytes}` (`SH:314-373`).

### 5.1 Протокол (байт-в-байт)
1. URL парсится WHATWG `new URL(url)`. `port = url.port || (protocol=='https:' ? 443 : 80)`; `path = (pathname || '/') + search` (fragment отбрасывается). `hostname` для IPv6 содержит `[...]`.
2. TCP `net.createConnection({host: proxyHost, port: proxyPort})`; ожидание `connect` с idle-timeout `timeoutMs` → `SOCKS5 proxy connection timed out after <ms>ms.` (сокет destroy) (`SH:198-207`). Ошибка сокета → reject с сообщением Node (`connect ECONNREFUSED 127.0.0.1:1080`).
3. Greeting: write `05 01 00`; read ровно 2 байта; требуется `05 00`, иначе destroy + `SOCKS5 proxy does not accept no-auth authentication.` (`SH:211-216`).
4. CONNECT: `05 01 00 ATYP ADDR PORT_BE16` (`SH:120-140`): если `isIP(hostname)==4` → ATYP `01` + 4 октета; **иначе всегда** ATYP `03` + `len(utf8)` (1 байт, без проверки ≤255) + bytes. IPv6 как ATYP 04 **не** отправляется (уходит как домен `[::1]`).
5. Ответ: read 4 байта; требуется `[0]==05 && [1]==00`, иначе destroy + `SOCKS5 connect failed with code <REP decimal>.`; затем пропуск BND.ADDR+PORT: ATYP 01 → 6 байт, 03 → 1 байт длины + len + 2, 04 → 18; иной ATYP → ничего не читается.
6. Ошибки чтения: таймаут → `SOCKS5 handshake timed out after <ms>ms.` (+destroy); EOF при `readExactly` → `Unexpected EOF while reading <n> bytes from SOCKS proxy.` (если EOF уже был до вызова) или `Unexpected EOF from SOCKS proxy.` (если пришёл во время ожидания) (`SH:66-113`). Лишние байты после handshake, пришедшие в том же chunk, **теряются** (буфер reader'а отбрасывается).
7. HTTPS: `tls.connect({socket, servername: hostname})` — проверка сертификата **включена** (Node CA bundle), SNI = hostname, ALPN не задан. Ожидание `secureConnect` с idle-timeout → `SOCKS HTTPS TLS handshake timed out after <ms>ms.` Go: `tls.Client(conn, &tls.Config{ServerName: host})` + `HandshakeContext`; для IP-hostname Go не шлёт SNI, но проверяет IP SAN — эквивалентно. Корневые CA: Go — системные; Node — встроенный Mozilla bundle (возможное расхождение в контейнере без `ca-certificates`!).
8. HTTP-запрос (`SH:347-355`) — ровно:
   ```
   <METHOD> <path> HTTP/1.1\r\n
   Host: <url.host>\r\n            (host[:port], порт опускается, если дефолтный для схемы)
   Connection: close\r\n
   [Content-Length: 0\r\n]         (только для POST)
   \r\n
   ```
   Без User-Agent/Accept. Тело POST пустое.
9. Ответ (`collectResponseHead`, `SH:240-293`): слушатели вешаются **до** записи запроса; накопление chunks; resolve, как только буфер содержит `\r\n\r\n`, ИЛИ на `end` (с тем, что есть); error → reject; idle-timeout → destroy + `SOCKS HTTP request timed out after <ms>ms.`. Размер не ограничен.
10. Парсинг (`SH:295-312`): нет `\r\n\r\n` → `Invalid HTTP response from monitored endpoint.`; первая строка должна матчить `^HTTP\/1\.[01]\s+(\d{3})` → иначе `Unable to parse HTTP status line: <line>`. `bodyBytes = len(buffer) - sep - 4` (только уже полученное). 1xx не обрабатываются особо.
11. Поток всегда `destroy()` после получения ответа (`SH:360-365`).
12. **Latency** = `round(now() - t0)`, `t0` — непосредственно перед `write(request)`, `now()` — после получения head; `performance.now()` (монотонные мс с дробью) (`SH:357-372`). Handshake не входит (тест `socks-http.test.ts:140`).

### 5.2 Таймауты — тонкость
`socket.setTimeout(ms)` = таймаут **бездействия** (сбрасывается при любой активности). Фазы: connect (set), SOCKS handshake (тот же таймер, слушатель reader'а), TLS (set на TLS-сокете), ответ (set заново). Общего дедлайна нет. В Go: перед каждой фазой `conn.SetDeadline(now+timeout)` и **продлевать после каждого успешного Read** (для ответа) — чтобы эмулировать idle; либо осознанно принять total-per-phase deadline (более строгий). Тексты ошибок таймаутов воспроизвести дословно — они попадают в `lastError`, dashboard и `msg` Remote Push.

Сообщения не-таймаутных сетевых ошибок (ECONNREFUSED, TLS verify) в Go будут отличаться — принять (документировать) или нормализовать.

---

## 6. Status server

### 6.1 Сервер (`startStatusServer`, `SS:640-677`)
- `listen` парсится `lastIndexOf(':')` → `{host, port}` (`SS:34-44`); `server.listen(port, host)`. Ошибка bind → reject (daemon падает).
- Лог `info status_server_started {listen}`.
- Обработчик для **любого** метода/пути: `handleStatusServerRequest(request.url, …)` → `writeHead(code, {'content-type': ...})` + `end(body)`. Node добавляет `Content-Length`, `Date`, `Connection: keep-alive`, `Keep-Alive: timeout=5`. HEAD → без тела. Логов на запросы нет. Необработанное исключение в handler (например, `escapeHtml` на не-строке из удалённого snapshot) → unhandled rejection → **падение процесса** Node 15+. В Go: `recover` → 500 (решение зафиксировать).
- `close()` — `server.close()` (ждёт активные соединения; idle keep-alive закрываются в Node ≥19). Go: `srv.Shutdown(ctx)`.

### 6.2 Подготовка на каждый запрос (`SS:529-548`)
```
snapshot = buildStatusSnapshot(mem)                                  // плоский список туннелей
agg = deps.aggregationState ?? createStatusAggregationState(remoteInstances)
local = {version:1, instanceId: statusServer.instanceId ?? os.hostname(), capturedAt: now,
         subscriptions: buildConfiguredStatusGroups(snapshot, cfg, mem), tunnels: snapshot}
storeLocalStatusSnapshot(agg, local)                                 // побочный эффект на любом запросе
aggregated = buildAggregatedStatusSnapshot(agg)
```

### 6.3 Контракт HTTP API

| Путь (сравнение `request.url`) | Метод | Код | Content-Type | Тело |
|---|---|---|---|---|
| `== "/api/status"` (точно, без query) | любой | 200 | `application/json; charset=utf-8` | `{"tunnels": StatusSnapshotTunnel[]}` |
| `== "/api/instance-status"` | любой | 200 | json | `InstanceStatusSnapshot` (локальный) |
| `== "/api/instances"` | любой | 200 | json | `AggregatedStatusSnapshot` |
| `startsWith("/api/runtime-state")`, `runtimeState.enabled=false` | любой | 404 | json | `{"error":"Runtime state endpoint is disabled."}` |
| то же, нет/пустой (после trim) `subscriptionId` или `targetAddress` | любой | 400 | json | `{"error":"subscriptionId and targetAddress query parameters are required."}` |
| то же, пара не найдена в config | любой | 404 | json | `{"error":"Target not found in loaded config."}` |
| то же, `ApiRequestError` при обращении к Xray | любой | 502 | json | `{"error":"<message>"}` |
| то же, иная ошибка | любой | 500 | json | `{"error":"<message>"}` |
| то же, успех | любой | 200 | json | `CurrentRuntimeStateSnapshot` |
| **всё остальное** (`/`, `/status`, `/api/status?x=1`, `/favicon.ico`, ...) | любой | 200 | `text/html; charset=utf-8` | HTML dashboard |

Все JSON — `JSON.stringify(v, null, 2)` (2 пробела, без финального `\n`). Query для runtime-state: `new URL(url, 'http://status.local').searchParams.get(..)?.trim()` (`+` → пробел, percent-decoding). Порядок проверок: disabled → params → поиск → builder.

Важно: ни один путь не возвращает 404 для неизвестного URL (кроме runtime-state).

### 6.4 JSON-схемы

**StatusSnapshotTunnel** (`RS:148-202`, порядок ключей):
```json
{
  "subscriptionId": "sub-1",
  "targetAddress": "127.0.0.1:8080",
  "displayName": "Alpha",
  "countryIso2": "AT",
  "endpoint": "127.0.0.1:20000",
  "state": "degraded",
  "lastHttpStatus": 204,
  "lastLatencyMs": 42,
  "lastError": "probe failed",
  "lastCheckedAt": "2026-01-01T00:00:00.000Z",
  "lastSuccessAt": "2025-12-31T23:59:00.000Z",
  "lastFailureAt": "2026-01-01T00:00:00.000Z",
  "lastSuccessHttpStatus": 204,
  "lastSuccessLatencyMs": 42,
  "balanced": false,
  "balancerMonitorState": "idle",
  "balancerMonitorLastStatusCode": 204,
  "balancerMonitorLastLatencyMs": 17,
  "balancerMonitorLastError": "…",
  "balancerMonitorLastCheckedAt": "…",
  "balancerMonitorLastSuccessAt": "…",
  "balancerMonitorLastFailureAt": "…",
  "balancerMonitorLastSuccessStatusCode": 204,
  "balancerMonitorLastSuccessLatencyMs": 17,
  "balancerMonitorRemotePingState": "failed",
  "balancerMonitorRemotePingLastRunAt": "…",
  "balancerMonitorRemotePingLastSuccessAt": "…",
  "balancerMonitorRemotePingLastFailureAt": "…",
  "balancerMonitorRemotePingLastStatusCode": 500,
  "balancerMonitorRemotePingLastError": "push failed",
  "balancerMonitorRemotePingLastReportedStatus": "down",
  "balancerMonitorRemotePingLastReportedMsg": "probe failed",
  "balancerMonitorRemotePingLastReportedPingMs": 33
}
```
Все поля после `state` — опциональны (опускаются при undefined). `lastHttpStatus` ← `monitor.lastStatusCode`, `lastSuccessHttpStatus` ← `monitor.lastSuccessStatusCode`. Сортировка: `subscriptionId`, затем `targetAddress`, затем `displayName` (все `localeCompare`) (`RS:195-201`). Туннели перечисляются только из `mem.targets` (то есть после первого успешного sync).

**GroupedStatusTarget.balancerMonitor** (`T:440-459`): `{state, lastStatusCode?, lastLatencyMs?, lastError?, lastCheckedAt?, lastSuccessAt?, lastFailureAt?, lastSuccessStatusCode?, lastSuccessLatencyMs?, remotePingState?, remotePingLastRunAt?, remotePingLastSuccessAt?, remotePingLastFailureAt?, remotePingLastStatusCode?, remotePingLastError?, remotePingLastReportedStatus?, remotePingLastReportedMsg?, remotePingLastReportedPingMs?}` (без `consecutiveFailures`).

**InstanceStatusSnapshot** (`/api/instance-status`):
```json
{
  "version": 1,
  "instanceId": "local-main",
  "capturedAt": "2026-08-15T10:00:00.000Z",
  "subscriptions": [
    {
      "subscriptionId": "sub-1",
      "targets": [
        {
          "subscriptionId": "sub-1",
          "targetAddress": "127.0.0.1:8080",
          "tunnels": [ /* StatusSnapshotTunnel, сортировка по latency */ ],
          "balancerMonitor": { "state": "healthy", "remotePingState": "ok", "remotePingLastStatusCode": 200 }
        }
      ]
    }
  ],
  "tunnels": [ /* StatusSnapshotTunnel, плоский список */ ]
}
```

`groupStatusSnapshot` (`SS:478-523`): группировка subscription→target, обе сортировки `localeCompare`; туннели внутри target сортируются `compareLatencyThenName` (`SS:122-126`): ключ = `+Inf` если `state=='idle'` или нет `lastLatencyMs`, иначе latency; при равенстве (включая Inf vs Inf — в JS `Inf-Inf=NaN`, falsy) → `displayName.localeCompare`. `balancerMonitor` берётся из полей `balancerMonitor*` **первого (неотсортированного) туннеля** группы, `state ?? 'idle'`.

`buildConfiguredStatusGroups` (`SS:148-217`): поверх групп из snapshot для **каждой enabled подписки из config** гарантирует наличие subscription и её target (даже без туннелей и без memory state). `balancerMonitor` = все поля из `mem.targets[key].balancerMonitor`, если target-state есть; иначе — уже имеющийся из группы или `{state:'idle'}`. Disabled подписки не добавляются (но их туннели, если бы были в memory, остались бы из snapshot — на практике memory их не содержит). Финальная сортировка subscription и targets по `localeCompare`; порядок туннелей внутри не меняется.

**AggregatedStatusSnapshot** (`/api/instances`, `SA:146-162`):
```json
{
  "capturedAt": "2026-08-15T10:02:00.000Z",
  "instances": [
    { "id": "local-main", "source": "local", "lastSuccessAt": "<local.capturedAt>", "snapshot": { /* InstanceStatusSnapshot */ } },
    { "id": "edge-eu", "source": "remote", "apiUrl": "https://edge.example.test/api/instance-status",
      "lastAttemptAt": "2026-08-15T10:02:00.000Z",
      "snapshot": { /* как получено */ },
      "lastSuccessAt": "2026-08-15T10:00:00.000Z",
      "lastError": "Remote status API returned HTTP 503." },
    { "id": "edge-us", "source": "remote", "apiUrl": "https://edge-us.example.test/api/instance-status" }
  ]
}
```
Локальный — всегда первым (`id = local.instanceId`, `lastSuccessAt = local.capturedAt`), затем remote, отсортированные по `id.localeCompare`. Порядок ключей у remote — порядок вставки JS: `id, source, apiUrl, lastAttemptAt, snapshot, lastSuccessAt, lastError` (если `lastError` был установлен до первого успеха и затем успех — он удаляется; порядок всегда соответствует этому списку). У local: `id, source, lastSuccessAt, snapshot` — **другой порядок**. `capturedAt` агрегата — новый `now`.

**CurrentRuntimeStateSnapshot** (`/api/runtime-state`, `CRS:420-479`, `T:577-610`):
```json
{
  "subscriptionId": "source-1",
  "targetAddress": "127.0.0.1:8080",
  "capturedAt": "…",
  "config": {
    "subscription": { /* SubscriptionConfig целиком, input редактирован */ },
    "target": { /* SubscriptionTargetConfig, URL мониторинга редактированы */ },
    "resources": { "outbounds": {"enabled": true}, "inbounds": {"enabled": true}, "routing": {"enabled": true} }
  },
  "serviceState": { "balancerMonitor": { /* TargetBalancerMonitorState целиком или ключ отсутствует */ } },
  "expected": {
    "source": "/path/subscription.txt",
    "outbounds":    [ {"baseTunnelId","displayName","countryIso2?","initialTag","fallbackTag","flow?"} ],
    "inbounds":     [ {"baseTunnelId","displayName","countryIso2?","tag","endpoint"} ],
    "routingRules": [ {"baseTunnelId","displayName","countryIso2?","ruleTag","inboundTag","outboundInitialTag","outboundFallbackTag"} ],
    "error": "…опционально"
  },
  "runtime": {
    "outbounds":    [ {"tag","classification":"fixed|managed-initial|managed-fallback|unmanaged","parsed?","rawBase64?"} ],
    "inbounds":     [ {"tag","classification":"fixed|managed|unmanaged","parsed?","rawBase64?"} ],
    "routingRules": [ {"ruleTag","classification":"fixed|managed|unmanaged","parsed":{"ruleTag","outboundTag","inboundTags"},"rawBase64?"} ]
  },
  "diff": {
    "outbounds":    {"matched":[],"matchedFallback":[],"missing":[],"unexpected":[]},
    "inbounds":     {"matched":[],"missing":[],"unexpected":[]},
    "routingRules": {"matched":[],"missing":[],"unexpected":[]}
  }
}
```
Логика (`CRS`):
- Поиск: подписка по `id` (первая, **без** проверки enabled) и `subscription.target.address == targetAddress`; иначе `undefined` → 404 (`CRS:403-418`).
- `expected` (`CRS:213-252`): для `http(s)://` (case-insens.) или `-` — пустые массивы + `error: "Expected topology is unavailable for remote or stdin inputs; runtime-state does not refetch subscription sources."`, `source` = редактированный input. Для файла — **перечитывает файл в момент запроса**, `buildManifest(content.trim(), id||'subscription')` → фильтры → `validateManifestOrThrow` → `buildTargetTopology`; при ошибке — пустые массивы + `error: e.message`, `source` = input. При успехе `source = input.source`. Массивы обнуляются, если соответствующий `resources.*.enabled=false`.
- Xray: `Promise.all([listOutbounds, listInbounds, listRules])` параллельно (`CRS:441-445`); любая ошибка → 502/500.
- Классификация outbound (`CRS:254-307`): `fixedOutbounds` → fixed; `initialTag` → managed-initial (`diff.matched`); `fallbackTag` → managed-fallback (`diff.matchedFallback`); иначе unmanaged (`diff.unexpected`). `missing` = `baseTunnelId` тех expected, у которых нет ни initial, ни fallback тега. Inbound/routing — аналогично по `tag`/`ruleTag`, `missing` содержит теги. Все списки diff и items сортируются `localeCompare`.
- `parsed` — декодирование protobuf (вне области); при `!includeSecrets` из outbound `parsed` удаляются `uuid`, `publicKey`, `shortId` (`CRS:150-160`). `rawBase64` только при `includeRaw` (std base64).
- `serviceState.balancerMonitor` = `mem.targets[key]?.balancerMonitor` (полный объект с `consecutiveFailures`), ключ опускается, если нет.

### 6.5 Редактирование секретов (`CRS:85-148`)
- `subscription.input`: если `includeSecrets` или не `^https?://` (case-insens.) → как есть; иначе `<origin>/<redacted>`, при ошибке парсинга `<redacted>`.
- `target.monitor.request.url`, `target.balancerMonitor.request.url`, `target.balancerMonitor.remotePing.url`: `<origin>/<redacted>` (любая схема; для непарсимых `<redacted>`; для схем без origin WHATWG даёт `"null"` → `null/<redacted>`).
- `origin` = `scheme://host[:port]` (порт опускается, если дефолтный).
- **Не** редактируются: `target.address`, `socks5`, `expected.source` для файлов, `remoteInstances[].apiUrl`, логи (`subscription_fetched.source`, `subscription_fetch_failed.source`, `remote_status_*.apiUrl` пишут URL целиком).
- Флаги берутся из `statusServer.runtimeState.includeRaw/includeSecrets`.

### 6.6 HTML dashboard
Шаблон целиком находится в `SS:128-146` (карточка туннеля), `SS:219-265` (subscription/target/balancer), `SS:267-476` (документ, inline CSS `SS:311-430` и JS табов `SS:438-473`). Внешних ресурсов нет. **Требование: перенести байт-в-байт** (CSS/JS — как Go raw string constant; в них нет обратных кавычек). Рекомендация: до удаления TS сгенерировать golden-файлы (`handleStatusServerRequest` с фиксированными fixtures и замороженным временем/hostname) и сравнивать в Go-тестах побайтно.

Структура:
```
<!doctype html><html lang="en"><head>… <title>Subscription Control Daemon Status</title><style>…</style></head>
<body><main>
  <h1>Subscription Control Daemon Status</h1>
  <nav class="instance-tabs" role="tablist" aria-label="Instances">  (tabs joined "\n")
     <button type="button" class="instance-tab[ is-active]" id="instance-tab-{i}" role="tab"
             aria-selected="{true|false}" aria-controls="instance-panel-{i}" tabindex="{0|-1}">{escape(id)}</button>
  </nav>
  <section class="instance-panel" id="instance-panel-{i}" role="tabpanel" aria-labelledby="instance-tab-{i}" tabindex="0"[ hidden]>
     <div class="instance-head"><div><h2>{id}</h2><p>{source} · {freshness}</p></div></div>
     {subscriptions | '<section class="subscription-card"><p class="empty-state">No snapshot available.</p></section>'}
  </section>
</main><script>…табы: click, ArrowRight/ArrowLeft/Home/End…</script></body></html>
```
- Первый инстанс (локальный) активен; остальные `hidden`. Атрибуты button — каждый на своей строке (многострочный тег), см. `SS:271-279`.
- `freshness`: `lastError` → `Last collection failed: <err>`; иначе `lastSuccessAt` → `Updated <ts>`; иначе `Waiting for first successful collection`. Приоритет у ошибки, даже если есть старый snapshot.
- Subscription: `<section class="subscription-card"><div class="subscription-head"><h2>{id}</h2></div>{targets}</section>`.
- Target: `<section class="target-card">` → `<h3>{targetAddress}</h3><p>{N} tunnel(s)</p>` → balancer-card `state-{stateClass}` с kicker `Balancer`, title `External SOCKS check`, метрики `State`, `Last Check`, `Remote Push` → `<div class="nodes-grid">{cards | '<p class="empty-state">No tunnel data.</p>'}</div>`. Balancer **перед** grid.
- Карточка туннеля: `<article class="node-card state-{stateClass}">`, orb, title `displayName`, meta `{countryIso2 ?? '-'} · {endpoint}` (символ `·` U+00B7), метрики `State` (raw `state`), `Last Check`, `Balancer`.
- Хелперы: `stateClass` (healthy/degraded/repairing, иначе idle) `SS:54-59`; `formatLatency` → `"<n> ms"` / `-`; `formatLastCheck(latency, error)` → latency, иначе `Error: <err>`, иначе `-` (`SS:61-70`); `formatBalancerStatus` → `active`/`removed`/`n/a` (`SS:72-80`); `formatRemotePushStatus(state, reported)` → `delivered|failed|pending|idle` + (` up`/` down` если reported) (`SS:82-92`); `metricClass` (`SS:94-120`): State → ` metric-state-{stateClass}`; Balancer → active/removed/na; Remote Push → `delivered up`→`metric-remote-up`, `delivered down` или содержит `failed` → `metric-remote-down`, `pending` → `metric-remote-pending`, иначе `metric-remote-idle`; прочие → без класса.
- `renderMetric` (`SS:128-130`): `<span class="metric{cls}"><span class="metric-label">{label}</span><span class="metric-value">{value}</span></span>`.
- Экранирование `escapeHtml`: `&`→`&amp;`, `<`→`&lt;`, `>`→`&gt;`, `"`→`&quot;` (в этом порядке). Число туннелей и индексы не экранируются.
- Дашборд **не** содержит ссылок на `/api/runtime-state`, таблиц, «HTTP 204», «Last success», «Kuma» (тесты).

---

## 7. Remote instances (`status-aggregation.ts`)

`createStatusAggregationState(remotes)` — placeholder `{id, source:'remote', apiUrl}` для каждого (`SA:19-32`).

`collectRemoteStatus(remote, state, logger)` (`SA:99-144`):
```
cur = state.remotes[id] ?? placeholder; cur.lastAttemptAt = now(); state.remotes[id]=cur
try:
   resp = fetch(apiUrl, {headers:{accept:'application/json'}, signal: AbortSignal.timeout(timeoutMs)})  // таймаут покрывает и чтение тела
   if !resp.ok (не 2xx) → throw "Remote status API returned HTTP <status>."
   cur.snapshot = parseRemoteSnapshot(await resp.json())
   cur.lastSuccessAt = now(); delete cur.lastError
   info remote_status_collected {remoteInstanceId, apiUrl}
catch e:
   cur.lastError = e.message                      // snapshot и lastSuccessAt СОХРАНЯЮТСЯ
   warn remote_status_collection_failed {remoteInstanceId, apiUrl, error}
```
Функция не бросает → `remote_status_tick_failed` практически недостижим. Редиректы — следуются. Таймаут: сообщение DOMException `The operation was aborted due to timeout` (в Go будет иное).

`parseRemoteSnapshot` (`SA:41-97`): объект; `version === 1`; `instanceId`, `capturedAt` — строки; `subscriptions`, `tunnels` — массивы; каждый subscription — объект со строкой `subscriptionId` и массивом `targets`; каждый target — строка `targetAddress` и массив `tunnels`; каждый туннель (и во вложенных, и в плоском списке) — объект со строками `subscriptionId`, `targetAddress`, `displayName`, `endpoint` и `state ∈ {healthy,degraded,repairing,idle}`. Иначе `Remote status response must be a JSON object.` / `Remote status response has an unsupported schema.`. Остальные поля **не проверяются** и сохраняются как есть. Go: хранить `json.RawMessage` для passthrough в `/api/instances` + типизированную структуру для HTML, валидацию повторить через `map[string]any`.

Интервал: независимый cron-loop на каждый remote (`RD:221-234`), первый опрос сразу при старте daemon. Состояние только в памяти.

---

## 8. Logging (`src/logging/*`)

`createLogger({level, format})` (`LOG:7-27`):
- `json`: `pino({level, base: undefined}, pino.destination(2))` → **stderr**, NDJSON, без `pid/hostname`. Строка: `{"level":30,"time":1760000000000,<поля объекта в порядке вставки>,"msg":"…"}`. Уровни: trace=10, debug=20, info=30, warn=40, error=50, fatal=60; `silent` — ничего. `time` — epoch ms (число).
- `pretty`: transport `pino-pretty` (`colorize: true`, `destination: 2`, `translateTime: 'SYS:standard'` → локальное время `yyyy-mm-dd HH:MM:ss.l o`, `ignore: 'pid,hostname'`). Формат: `[2026-10-17 12:00:00.000 +0000] INFO: Sync started.` + последующие строки `    key: value`. Байт-в-байт не требуется; в Go — `slog.TextHandler`/кастомный handler, близкий по виду.
- Go-рекомендация для `json`: собственный `slog.Handler` (или zerolog) с ключами `level` (int), `time` (int64 ms), поля, `msg` — **именно в этом порядке**, чтобы не сломать парсинг логов. stdout зарезервирован под машинные отчёты (`sync`, `validate-config`, `print-config`, `generate`).
- Значения `error` в логах — всегда строки (сообщения), объектов Error нет → сериализаторы pino не задействованы.
- `src/logging/format.ts` — `buildParseLog(manifest) → {generatedAt, summary, skipped}` (используется `generate`, не логгер).

### 8.1 Каталог событий

| event | level | msg | поля (порядок) | источник |
|---|---|---|---|---|
| `config_loaded` | info | `Config loaded.` | `configPath` | `SO:494`, `RD:160` |
| `sync_skipped_overlap` | warn | `Sync is already running in this process; skipping overlap run.` | — | `SO:475` |
| `sync_failed` | error | `Sync failed.` | `error` | `SO:484` |
| `sync_started` | info | `Sync started.` | `sourceCount` (все подписки, вкл. disabled) | `SO:393` |
| `subscription_fetched` | info | `Subscriptions loaded.` | `subscriptions: [{id, source, encoding, targetAddress}]` | `SO:396-407` |
| `subscription_fetch_failed` | error | `Subscription fetch failed.` | `subscriptionId, source, error, targetAddress` | `SO:356-365` |
| `manifest_validation_failed` | error | `Manifest validation failed.` | `subscriptionId, sourceId, error, targetAddress` | `SO:324-333` |
| `manifest_built` | info | `Manifest built.` | `kind, subscriptionId, sourceId, managedCount, skipped(число), [filtered, filteredByCountry, filteredByLabelRegex], targetAddress` | `SO:447-459` |
| `skipped_no_changes` | info | `Manifest unchanged for resource; skipping Xray API.` | `kind, subscriptionId, targetAddress, observatorySubjectSelectorPrefix?` | `SO:225-234` |
| `resource_apply_started` | info | `Applying resource to target.` | `kind, subscriptionId, targetAddress, observatorySubjectSelectorPrefix?` | `SO:255-264` |
| `apply_finished` | info | `Apply finished.` | `kind, subscriptionId, targetAddress, sourceId, added, replaced, removed, failed, deletedIds, appliedIds, durationMs` | `SO:170-202` |
| `apply_failed` | error | `Apply failed.` | как выше + `errorMessages` (непустые message failed-items), `failedItems` (полные объекты) | `SO:170-202` |
| `daemon_tick` | info | `Daemon tick started.` | — | `RD:171` |
| `daemon_tick_skipped_overrun` | warn | `Daemon tick skipped because previous run is still in progress.` | `scheduledAt` | `RD:174-175` |
| `sync_failed` | error | `Daemon sync failed.` | `error, causes?` | `RD:176-177` |
| `monitor_tick_skipped_overrun` | warn | `Monitor tick skipped because previous run is still in progress.` | `subscriptionId, targetAddress, event, scheduledAt` | `RD:189-196` |
| `monitor_tick_failed` | error | `Monitor tick failed.` | `subscriptionId, targetAddress, event, error, causes?` | `RD:191-196` |
| `balancer_monitor_tick_skipped_overrun` | warn | `Balancer monitor tick skipped because previous run is still in progress.` | `subscriptionId, targetAddress, event, scheduledAt` | `RD:206-213` |
| `balancer_monitor_tick_failed` | error | `Balancer monitor tick failed.` | `subscriptionId, targetAddress, event, error, causes?` | `RD:208-213` |
| `remote_status_tick_skipped_overrun` | warn | `Remote status tick skipped because previous run is still in progress.` | `remoteInstanceId, apiUrl, event, scheduledAt` | `RD:225-232` |
| `remote_status_tick_failed` | error | `Remote status tick failed.` | `remoteInstanceId, apiUrl, event, error, causes?` | `RD:227-232` |
| `status_server_started` | info | `Status server started.` | `listen` | `SS:662` |
| `remote_status_collected` | info | `Remote instance status collected.` | `remoteInstanceId, apiUrl` | `SA:128-131` |
| `remote_status_collection_failed` | warn | `Remote instance status collection failed; keeping the last successful snapshot.` | `remoteInstanceId, apiUrl, error` | `SA:134-142` |
| `monitor_check_retry` | debug | `Monitor check attempt failed; retrying.` | `subscriptionId, targetAddress, tunnelId, attempt, attempts, delayMs, statusCode?, error` | `MON:211-222`, `MON:464-471` |
| `balancer_monitor_check_retry` | debug | `Balancer monitor check attempt failed; retrying.` | `subscriptionId, targetAddress, attempt, attempts, delayMs, statusCode?, error` | `MON:556-562` |
| `tunnel_repaired` | warn | `Tunnel repaired after failed monitor probe.` | `subscriptionId, targetAddress, tunnelId, outboundTag` | `MON:309-318` |
| `tunnel_repair_failed` | error | `Tunnel repair failed.` | `subscriptionId, targetAddress, tunnelId, error` | `MON:327-336` |
| `tunnel_rejoined_balancer` | info | `Tunnel rejoined Xray balancer after successful monitor probe.` | `subscriptionId, targetAddress, tunnelId, outboundTag` | `MON:398-407` |
| `tunnel_rejoin_failed` | error | `Tunnel balancer rejoin failed after successful monitor probe.` | `subscriptionId, targetAddress, tunnelId, error` | `MON:415-424`, `MON:512-521` |
| `balancer_monitor_remote_ping_skipped_overrun` | warn | `Balancer monitor remote ping skipped because previous push is still in progress.` | `subscriptionId, targetAddress` | `MON:619-626` |
| `balancer_monitor_remote_ping_started` | info | `Balancer monitor remote ping started.` | `subscriptionId, targetAddress, status, reportedStatus, viaSocks, remoteHost, [proxyHost, proxyPort]` | `MON:641-658` |
| `balancer_monitor_remote_ping_failed` | warn | `Balancer monitor remote ping failed.` | `subscriptionId, targetAddress, [statusCode], error, reportedStatus, viaSocks, remoteHost, [proxyHost, proxyPort]` | `MON:687-705`, `MON:747-764` |
| `balancer_monitor_remote_ping_succeeded` | info | `Balancer monitor remote ping succeeded.` | `subscriptionId, targetAddress, statusCode, reportedStatus, viaSocks, remoteHost, [proxyHost, proxyPort]` | `MON:718-735` |

Порядок ключей: в большинстве событий `event` первым; в событиях `runLoggedCronLoop` **сначала context**, затем `event` (`RD:117-124`, `RD:129-136`); в retry — `event` затем context. Поля со значением `undefined` pino опускает (например, `observatorySubjectSelectorPrefix`).

---

## 9. Модель конкурентности TS → Go (сводка)

| TS | Go |
|---|---|
| Один event loop; мутации state атомарны между `await` | `sync.RWMutex` на `SyncMemoryState` (state-lock), все чтения/записи полей под ним, без I/O внутри |
| `syncInProgress` флаг | `atomic.Bool.CompareAndSwap(false,true)`; overlap → пустой отчёт + warn |
| `withTargetMutationLock` (FIFO промисы) | per-target `*sync.Mutex` из map под lock; держится во время Xray-вызовов apply/repair/rejoin |
| `runCronLoop` | goroutine на loop; `atomic.Bool tickInProgress`; `time.Timer` + `select ctx.Done()` |
| `Promise.race(loops)` | `WaitGroup` всех loops; закрытие HTTP-сервера после выхода первого/всех (решение) |
| `runWithConcurrencyLimit` + `AggregateError` | пул `min(maxParallel,n)` горутин, общий atomic-индекс; упавший воркер выходит; ошибка с `Causes []string` |
| fire-and-forget remote ping | `go` + check-and-set `pending` под state-lock |
| `fetch` + `AbortSignal.timeout` | `http.Client{Timeout}` / `context.WithTimeout` (включая чтение тела) |
| `socket.setTimeout` idle | `SetDeadline` на фазу, продление после каждого Read |
| `performance.now()` | `time.Since` (монотонные часы), округление `math.Round(ms)` |
| `new Date().toISOString()` | `time.Now().UTC().Format("2006-01-02T15:04:05.000Z")` |
| `localeCompare` | `collate.New(language.Und)` (зафиксировать; кириллица/эмодзи в displayName) |
| `os.hostname()` | `os.Hostname()` |

Все зависимости, которые тесты подменяют (requestViaSocksFn, directRemotePingFn, repairTunnelFn, rejoinTunnelFn, createMutationClient, fetchFn, nowFn, waitFn, getNextDateFn, createConnection, tlsConnect, buildCurrentRuntimeStateSnapshotFn, loadLocalInputFn, createClient, loadSubscriptionsFn, applicators) — сделать полями структур/интерфейсами для Go-тестов.

---

## 10. Инвентарь тестов (для портирования)

### 10.1 `test/run-daemon.test.ts`
1. `runCronLoop skips overrunning ticks without drifting future cron slots` — фейковые часы, слот каждые 100 мс, стоп при ≥300; первый tick блокируется до первого overrun ⇒ запуски в `[0, 200]`, overrun ровно один на слоте `100` (ISO 1970-01-01T00:00:00.100Z).
2. `getErrorLogDetails preserves aggregate causes` — AggregateError('top-level', [first, second]) ⇒ `message='top-level'`, `causes=['first cause','second cause']`.
3. `runLoggedCronLoop logs overrun through logger path` — первый warn: msg `Balancer monitor tick skipped because previous run is still in progress.`, `event=balancer_monitor_tick_skipped_overrun`, context `subscriptionId=sub-1`, `targetAddress=xray:8080`; ошибок нет; пользовательский `onOverrun` тоже вызывается.
4. `runLoggedCronLoop logs aggregate causes through logger path` — task бросает AggregateError ⇒ ровно один error, msg `Monitor tick failed.`, `event=monitor_tick_failed`, `causes=[...]`; warn нет; loop не падает.

### 10.2 `test/socks-http.test.ts`
1. `collectResponseHead resolves when response headers arrive, not when connection closes` — резолв < 250 мс при `close` через 350 мс, буфер начинается с `HTTP/1.1 204`.
2. `collectResponseHead rejects, destroys stream and removes listeners on timeout` — событие timeout ⇒ ошибка `SOCKS HTTP request timed out after 1000ms`, stream destroyed, 0 слушателей data/end/error/timeout. (Go: соединение закрыто, горутин не осталось.)
3. `requestViaSocks attaches response listener before writing request` — фейковый прокси отвечает синхронно на запись ⇒ status 204, latency 23 (ровно один интервал часов между двумя вызовами now), сокет destroyed, третья запись начинается `GET /health HTTP/1.1`, слушатели сняты.
4. `requestViaSocks latency excludes SOCKS handshake setup time` — handshake «стоит» 2×500 мс, ответ 7 мс ⇒ `latencyMs=7`.
5. `requestViaSocks rejects and destroys socket on TCP connect timeout` ⇒ `SOCKS5 proxy connection timed out after 1000ms`, сокет destroyed.
6. `requestViaSocks rejects and destroys socket on SOCKS handshake timeout` ⇒ `SOCKS5 handshake timed out after 1000ms`, destroyed, слушатели сняты.
7. `requestViaSocks rejects and destroys TLS stream on secureConnect timeout` (https) ⇒ `SOCKS HTTPS TLS handshake timed out after 1000ms`, TLS-поток destroyed.
Дополнительно рекомендовано в Go: greeting ≠ `05 00`; REP≠0 (`SOCKS5 connect failed with code 5.`); ATYP 03/04 в ответе; IPv4 vs домен в CONNECT; POST содержит `Content-Length: 0`; `Host` с нестандартным портом; невалидная status line; EOF без `\r\n\r\n`.

### 10.3 `test/status-aggregation.test.ts`
1. `remote collector stores the latest successful status snapshot` — snapshot сохранён целиком; `lastAttemptAt` = первый `now()`, `lastSuccessAt` = второй `now()`; `lastError` отсутствует.
2. `remote collector keeps the last successful snapshot after an error` — HTTP 503 ⇒ snapshot и `lastSuccessAt` сохранены, `lastError` матчит `HTTP 503` (видно через `buildAggregatedStatusSnapshot`, без local — remote первый).
3. `remote collector rejects incompatible API payloads without replacing state` — `{tunnels: []}` ⇒ snapshot не установлен, `lastError` ~ `unsupported schema`.

### 10.4 `test/status-server.test.ts` (через `handleStatusServerRequest`, без сети)
1. `renders HTML dashboard without JSON links and with state cards` — 200, `text/html; charset=utf-8`; title; `balancer-card state-degraded`; `node-card state-healthy`; `Last Check`; `>42 ms<`; `Remote Push`; `>failed down<`; классы `metric-state-healthy`, `metric-state-degraded`, `metric-balancer-active` (prefix `x-observe-` при current=initial); НЕТ `HTTP 204`, `Last success`, `Remote Ping ok`, `Kuma`, `Current runtime state (JSON)`, `/api/runtime-state`, `<table>`; balancer-card раньше `nodes-grid`.
2. `renders degraded current error separately from last success and balancer status` — путь `/status` ⇒ HTML; `node-card state-degraded`, `Error: probe failed`, нет `HTTP 204`, `>removed<`, `metric-balancer-removed` (initial `x-observe-out-a`, current `out-a`).
3. `renders configured target and balancer card even without tunnel rows` — target-state без туннелей ⇒ адрес target, `balancer-card state-healthy`, `No tunnel data.`, `>17 ms<`, `>delivered up<`, `metric-remote-up`, нет `HTTP 204`.
4. `does not render disabled subscriptions from config` — пустая память ⇒ есть `sub-1`, нет `sub-disabled` и `127.0.0.1:9999`.
5. `keeps api status wire format as flat tunnels array` — `/api/status` ⇒ `{tunnels:[1 элемент]}`, ключа `subscriptions` нет.
6. `api status exposes monitor history and balancer participation fields` — degraded-туннель: нет `lastHttpStatus`/`lastLatencyMs`; `lastError='probe failed'`; `lastSuccessHttpStatus=204`; `lastSuccessLatencyMs=42`; `balanced=false`; `balancerMonitorRemotePing*` поля проброшены.
7. `exposes versioned local instance and subscription snapshot` — `/api/instance-status`: `version=1`, непустой `instanceId` (hostname), парсимый `capturedAt`, `subscriptions[0].subscriptionId='sub-1'`, `tunnels=[]`.
8. `exposes and renders the last stored remote instance snapshot` — `/api/instances` содержит `edge-eu` со snapshot и `lastError` ~`HTTP 503`; HTML: `edge-eu`, `Last collection failed`, `role="tablist" aria-label="Instances"`; tab-0 (`local-main`) `aria-selected="true"`, `tabindex="0"`; tab-1 (`edge-eu`) `aria-selected="false"`, `tabindex="-1"`; panel-0 без `hidden`, panel-1 `tabindex="0" hidden`; local раньше edge-eu; `Waiting for first successful collection` (edge-us), `No snapshot available.`; JS содержит `event.key === 'ArrowRight'|'ArrowLeft'|'Home'|'End'`.
9. `returns 400 for runtime-state endpoint without required query params` — error ~ `subscriptionId and targetAddress`.
10. `returns 404 when runtime-state target is missing` — `targetAddress=127.0.0.1%3A9999` ⇒ 404 `Target not found` (реальный builder, Xray не вызывается).
11. `returns 502 when runtime-state builder hits Xray API error` — `ApiRequestError('Xray API unavailable')` ⇒ 502 с этим сообщением.
12. `runtime-state endpoint uses redacted builder output` — builder вызывается с `includeSecrets=false`, `includeRaw=false` из конфига; ответ — сериализация результата builder'а как есть (`input` редактирован, нет `rawBase64`, `serviceState.balancerMonitor.state='healthy'`).

### 10.5 `test/runtime-state.test.ts`
1. `buildCurrentRuntimeStateSnapshot combines local config with runtime API state and redacts by default` — локальный файл; `direct` → fixed (идёт первым по сортировке), prefixed outbound → managed-initial; inbound managed; routing managed; `missing` пусты; `parsed.protocol='vless'`, `parsed.uuid` отсутствует; нет `rawBase64`; inbound `parsed.protocol='socks'`; routing `parsed.outboundTag`; без memoryState — нет `serviceState.balancerMonitor`.
2. `reflects target-specific vision udp443 override in expected and runtime views` — `expected.outbounds[0].flow` и `runtime.outbounds[0].parsed.flow` = `xtls-rprx-vision-udp443`.
3. `treats repaired outbound tag as fallback match` — runtime содержит только unprefixed тег ⇒ `matched=[]`, `matchedFallback=[тег]`, `missing=[]`.
4. `does not refetch remote subscriptions and redacts remote input by default` — https input ⇒ loader не вызывается; `input='https://example.test/<redacted>'`; `expected.error` ~ `does not refetch subscription sources`; `serviceState.balancerMonitor.state='healthy'` из memoryState.
5. `redacts target monitor URLs by default and exposes them with includeSecrets` — три URL → `<origin>/<redacted>`; `remotePing.viaSocks` сохранён; с `includeSecrets` — исходные URL.
6. `exposes raw and secrets only when explicitly enabled` — `includeRaw+includeSecrets` ⇒ есть `rawBase64`, `parsed.uuid` присутствует.
7. `groupStatusSnapshot groups tunnels by subscription and target` — сортировка subscription (`sub-a`, `sub-b`) и targets (`:8080`, `:8082`).
8. `groupStatusSnapshot sorts tunnels by latency and moves idle or missing latency to the end` ⇒ `['Fast','Slow','Idle','Unknown']` (Idle/Unknown — по имени).

### 10.6 `test/monitoring.test.ts`
1. `starts tunnel probes in parallel and serializes repairs via target mutex` — 2 туннеля, оба probe одновременно (max concurrency 2), repair через mutex — max 1 одновременно.
2. `respects monitor.maxParallel and does not let one business failure block healthy probes` — 3 туннеля, `maxParallel=2` ⇒ max concurrency 2; один (port 20000) провален ⇒ 1 repair, первый туннель `degraded`, остальные healthy.
3. `clears current check on failure and preserves last successful check` — после успеха: healthy, 200/25 в current и lastSuccess; после провала: degraded, current-поля очищены, lastSuccess* сохранены, `lastError` ~ `probe failed`.
4. `retries failed tunnel check before marking degraded or repairing` — attempts=2: первая ошибка, вторая успех ⇒ 2 вызова, 0 repair, healthy, cf=0; ровно один debug `monitor_check_retry {attempt:1, attempts:2, error:'temporary probe failure'}`.
5. `retries unexpected status and repairs only after all attempts fail` — expected 204, всегда 200 ⇒ 2 вызова, 1 repair, degraded, `lastError` ~ `Expected HTTP 204, got 200`; debug-лог содержит `statusCode:200`, `error:'Expected HTTP 204, got 200.'`.
6. `keeps retry attempts inside monitor.maxParallel worker slots` — 3 туннеля × 2 попытки, maxParallel 2 ⇒ max concurrency 2, по 2 вызова на порт, все healthy.
7. `real repair path uses injected mutation client and switches to unprefixed outbound` — вызовы `[removeRule:route, removeOutbound:initial, addOutbound:unprefixed, addRule:route->unprefixed]`; degraded; `lastLatencyMs` нет; `outboundTagCurrent=unprefixed`; `balanced=false`.
8. `rejoins repaired tunnel after successful probe` — инжектированный rejoin вызван с этим туннелем; healthy, latency 25.
9. `keeps healthy check local when balancer rejoin fails` — rejoinFn бросает ⇒ healthy, latency 25, тег остаётся unprefixed, `lastError` ~ `rejoin failed: xray rejected rejoin`, error-событие `tunnel_rejoin_failed`.
10. `real rejoin path restores prefixed outbound and routing after successful probe` — вызовы `[addOutbound:initial, removeRule:route, addRule:route->initial, removeOutbound:unprefixed]`; current=initial; `lastError` очищен; `balanced=true`; info `tunnel_rejoined_balancer`.
11. `real rejoin path skips Xray mutation when tunnel is already balanced` — mutation client не создаётся; healthy.
12. `real rejoin path rolls back when prefixed routing add fails` — вызовы `[addOutbound:initial, removeRule, addRule:->initial(fail), removeRule, addRule:->unprefixed, removeOutbound:initial]`; healthy, latency 25; тег unprefixed; `lastError` ~ `balancer rejoin failed: addRule prefixed failed`; `balanced=false`; `tunnel_rejoin_failed`.
13. `real rejoin rollback failure does not fail whole monitor tick` — rollback addRule тоже падает ⇒ тик не бросает; оба порта опрошены; repaired-туннель healthy с ошибкой rejoin; второй healthy без ошибки; `removeOutbound:initial` вызван.
14. `treats unexpected status as failure without stale current success` — 204 при ожидании 200 ⇒ degraded, нет current-полей, нет `lastSuccessStatusCode`, `lastError` ~ `Expected HTTP 200, got 204`.
15. `propagates unexpected exceptions after probe settlement` — repairFn бросает ⇒ тик reject `Concurrent task execution failed`.
16. `runTargetBalancerMonitorTick performs main request via configured external socks` — один вызов `{127.0.0.10, 1080, url, GET}`; healthy, lastStatusCode=200, lastSuccess 200/33.
17. `marks target degraded on failed main check without stale current success` — исходно healthy 204/12; ответ 200 при ожидании 204 ⇒ degraded, current-поля очищены, lastSuccess 204/12 сохранены, `lastError` ~ `Expected HTTP 204, got 200`.
18. `retries primary check and sends one up remote push after final success` — 2 попытки; healthy, latency 33; push ровно один `…/token?status=up&msg=OK&ping=33`; `remotePingState=ok`; один debug `balancer_monitor_check_retry`.
19. `sends one down remote push after all retry attempts fail` — 2 попытки, degraded; push один `…?status=down&msg=Expected+HTTP+204%2C+got+200.`; `remotePingState=ok`; debug-лог с `statusCode:200`.
20. `starts remote ping asynchronously after successful primary check` — тик завершается при висящем push: `remotePingState=pending`, reported up/OK/33, URL `…?keep=1&status=up&msg=OK&ping=33`, socks вызван 1 раз; после освобождения ⇒ `ok`, `remotePingLastStatusCode=200`.
21. `starts remote ping down after primary failure without changing health state on push failure` — push 500 ⇒ state degraded (от primary), `remotePingState=failed`, `remotePingLastStatusCode=500`, `remotePingLastError` ~ `HTTP 500`, reported down + msg.
22. `skips overlapping remote ping pushes` — `remotePingState=pending` заранее ⇒ push не вызывается, warn `balancer_monitor_remote_ping_skipped_overrun`.
23. `sends remote ping through balancer socks when viaSocks is enabled` — второй SOCKS-вызов `{127.0.0.10,1080, …?status=up&msg=OK&ping=33, GET, timeoutMs:3000}`; direct не вызывается; 502 ⇒ `failed`, `remotePingLastStatusCode=502`; healthy; логи started/failed содержат `reportedStatus:'up', viaSocks:true, remoteHost:'push.example.test', proxyHost, proxyPort` (+`statusCode:502` в failed).
24. `pushRemotePingDirect records status and cancels response body without downloading it` — GET на точный URL, status 204, `body.cancel` вызван, тело не читается.
25. `buildRemotePingUrl preserves existing query params and overwrites push params` — см. §4.6.

### 10.7 `test/service.test.ts` — runtime-часть
1. `loadSubscriptions sorts enabled subscriptions by id` — `['source-a','source-b']`, failed пуст.
2. `syncWithConfig skips Xray API when subscription manifest is unchanged in memory` — второй прогон: `unchanged=1`, `targets[0].unchangedKinds=['outbound']`, `listOutbounds` вызван 1 раз, `removeOutbound` никогда.
3. `syncWithConfig applies multiple subscriptions independently` — 2 target-отчёта, клиенты созданы для `:8080`, затем `:8081`, failed=0.
4. `syncWithConfig logs failed apply item messages for diagnostics` — `report.failed=1`; error-лог `apply_failed` с `errorMessages=['xray api rejected dynamic routing rule']` и полным `failedItems`.
5. `syncWithConfig skips overlapping runs inside one process` — второй вызов во время первого: `failed=0`, `targets=[]`, loader второго не вызывается, первый loader вызван 1 раз.
6. `syncWithConfig keeps other subscriptions running when one source load fails` — `failed=1`, 2 targets: `[0]=:8080` (failed=1, ошибка загрузки), `[1]=:8081`; клиент только для `:8081`.
7. `syncWithConfig does not call Xray API when manifest has no valid entries` — `trojan://` ⇒ target `:8080` failed=1, `:8081` failed=0, клиент только `:8081`.
8. `syncWithConfig does not call Xray API when filters remove all entries` — `countryAllowlist:['US']` для AT ⇒ 1 target, failed=1, клиентов нет.
(Тесты `loadConfig*`, `outbound applicator*`, `applyOutbounds*` — область других документов.)

### 10.8 Рекомендуемые дополнительные Go-тесты (поведение не покрыто TS-тестами)
- Частичная ошибка apply ⇒ хэши/topology не меняются, monitor-state не сброшен; повторный sync применяет все kinds.
- Успешный apply ⇒ monitor-state всех туннелей `idle`, `topologyGeneration++`.
- Ошибка `preparePlanForTarget` (нехватка портов) ⇒ `sync_failed`, исключение из `syncWithConfig`.
- Порядок `SyncReport.targets`: failed-загрузки впереди.
- Status server: `/api/status?x=1` ⇒ HTML; `/api/runtime-state` при `enabled=false` ⇒ 404; любой метод ⇒ тот же ответ.
- Golden HTML/JSON, снятые с TS-реализации.
- Cron: `next()` для типовых выражений; первый тик немедленно; быстрый выход по ctx (если принято).

---

## 11. Открытые решения для миграции
1. Сохранять ли падение всего sync при ошибке `preparePlanForTarget` (п. TL;DR 4) — рекомендация: сохранить (поведение) либо явно оформить как fix.
2. Быстрый shutdown по ctx вместо ожидания слота — рекомендация: да (улучшение без изменения контракта).
3. Баг `setTimeout` > 24.8 дня — не воспроизводить.
4. Cron-библиотека и поддержка `L/W/#`/секунд — выбрать и синхронизировать с валидатором конфига.
5. Тексты сетевых/TLS ошибок (видны в dashboard и Remote Push) — принять отличия Go.
6. Idle-timeout vs deadline в SOCKS-клиенте.
7. Паника в HTTP-handler → 500 вместо падения процесса.
8. `localeCompare` → `x/text/collate` (Und) — проверить на реальных displayName (кириллица, эмодзи-флаги).
9. Порядок ключей JSON (local vs remote instance) — воспроизвести кастомным `MarshalJSON` или принять.
