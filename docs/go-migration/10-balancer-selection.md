# 10. Выбранный outbound балансера Xray на дашборде scd

Новое требование владельца: дашборд должен показывать, какой outbound **сейчас выбран балансером Xray**. В TS-scd этого нет совсем: `balancerMonitor` только гоняет HTTP-запрос через внешний SOCKS5 (`03-scd-runtime.md` §4.5) и не знает, через какой outbound он прошёл. `GetBalancerInfo` и `OverrideBalancerTarget` scd не вызывает (`02-scd-xray-api.md` §1.2).

Исследование ведётся по Xray-core **v26.10.10** (`701af60772cd`, решение D4). Ссылки `xray:<path>:<line>` относятся к клону `scratchpad/xray-core-26.10.10`. Живая проверка описана в §5, артефакты перечислены в приложении.

> **TL;DR**
> * Источник данных: `RoutingService.GetBalancerInfo(tag)` → `{override.target, principle_target.tag[]}`. Это **не** «последний выбор для реального соединения», а результат `strategy.GetPrincipleTarget(candidates)`, который вычисляется заново при каждом вызове.
> * **leastPing**: `principle_target` всегда содержит ровно один тег, и это тот самый outbound, который выберет `PickOutbound`. Результат детерминирован при неизменном состоянии observatory. Если живых кандидатов нет, приходит `[""]` (один пустой тег, не пустой список).
> * **leastLoad**: приходит отобранное множество. При `expected ≤ 1` и без baselines это один тег, совпадающий с реальным выбором. При `expected > 1` или baselines реальный выбор делается случайно из множества (`dice.Roll`) для каждого соединения. Если кандидатов нет, приходит `[]`.
> * **random / roundRobin**: `principle_target` = **все** кандидаты селектора, включая мёртвые. «Текущего выбора» у этих стратегий нет: он свой у каждого соединения.
> * **Override** (`OverrideBalancerTarget`) виден только в `override.target`. На `principle_target` он не влияет, но в `PickOutbound` побеждает всегда, даже если указан несуществующий тег (трафик при этом рвётся). Эффективный выбор: `override.target != "" ? override : principle`.
> * `ObservatoryService.GetOutboundStatus` даёт `alive/delay/last_error_reason/outbound_tag/last_seen_time/last_try_time/health_ping` для всех наблюдаемых outbound’ов. Порядок списка недетерминирован. Записи удалённых outbound’ов держатся до следующего цикла observer’а.
> * **Изменение конфига Xray:** в `docker/xray/config/01_api.json` нужно добавить `"ObservatoryService"`. `RoutingService` там уже есть. ⚠️ `ObservatoryService` без секции `observatory`/`burstObservatory` **роняет старт Xray** (`core: not all dependencies are resolved.`). В нашем конфиге секция `05_observatory.json` есть.
> * Отображение тега на туннель scd: сравнение `tag == tunnel.outboundTagCurrent`, в запасе — снятие `observatorySubjectSelectorPrefix`. Отремонтированный (repaired) туннель (unprefixed) балансер выбрать не может, и observatory его не наблюдает.
> * Предложение: новый блок `target.balancerSelection {enabled, balancerTag, schedule, observatory}`, отдельный cron-цикл только на чтение (без target-mutex), аддитивные поля в `/api/instance-status` и `/api/status` (`version` остаётся `1`), новая карточка «Xray selection» в target-блоке и метрика `Observatory` в карточках туннелей. Когда фича выключена, дашборд визуально не меняется (D3).

---

## 1. Исходное состояние

| Что | Где | Значение для задачи |
|---|---|---|
| API services Xray | `docker/xray/config/01_api.json` | `HandlerService, LoggerService, StatsService, RoutingService`. **`ObservatoryService` нет** |
| Балансер | `docker/xray/config/04_routing.json` | `tag: x-balancer`, `selector: ["x-observe-"]`, `strategy.type: leastPing`, без `fallbackTag`. Правило `ruleTag: x-balancer` (`ip: geoip:!ru`) → `balancerTag: x-balancer` |
| Observatory | `docker/xray/config/05_observatory.json` | `subjectSelector: ["x-observe-"]`, `probeURL: https://www.google.com/generate_204`, `probeInterval: 15s`, `enableConcurrency: true` |
| Outbounds | `docker/xray/config/06_outbound.json` | `direct` (первый → **default outbound**), `blocked` |
| scd prefix | `target.observatorySubjectSelectorPrefix` (`scd/src/config/load-config.ts:170`; пример `docker/scd/config.example.yml`: `x-observe-`) | Добавляется только к начальным (initial) outbound-тегам (`build-tunnel-topology.ts:57`). После repair outbound пересоздаётся **без** префикса (`03` §4.2), после rejoin — снова с префиксом (`03` §4.3) |
| Тег балансера в конфиге scd | — | **scd его не знает.** `ListRule` возвращает для balancer-правила `tag=""` (`02` §1.5), а `balancerTag` в `ListRuleItem` нет (`xray:app/router/command/command.proto:109-112`). Совпадение `ruleTag == balancerTag` — только соглашение нашего конфига. Поэтому тег балансера нужно задать в конфиге scd явно (§6.5) |

---

## 2. RoutingService: балансерные RPC (v26.10.10)

### 2.1. Сообщения (`xray:app/router/command/command.proto`)

| Сообщение | Поля (номер) | Строки |
|---|---|---|
| `PrincipleTargetInfo` | `repeated string tag = 1` | `:67-69` |
| `OverrideInfo` | `string target = 2` (поля 1 нет) | `:71-73` |
| `BalancerMsg` | `OverrideInfo override = 5`, `PrincipleTargetInfo principle_target = 6` (полей 1–4 нет) | `:75-78` |
| `GetBalancerInfoRequest` | `string tag = 1` | `:80-82` |
| `GetBalancerInfoResponse` | `BalancerMsg balancer = 1` | `:84-86` |
| `OverrideBalancerTargetRequest` | `string balancerTag = 1`, `string target = 2` | `:88-91` |
| `OverrideBalancerTargetResponse` | пусто | `:93` |
| `service RoutingService` | `SubscribeRoutingStats` (stream), `TestRoute`, **`GetBalancerInfo`**, **`OverrideBalancerTarget`**, `AddRule`, `RemoveRule`, `ListRule` | `:118-130` |

Full method paths: `/xray.app.router.command.RoutingService/GetBalancerInfo` и `/xray.app.router.command.RoutingService/OverrideBalancerTarget`. Сервис дополнительно зарегистрирован под старым именем `v2ray.core.app.router.command.RoutingService` (`xray:app/router/command/command.go:148-151`).

Между v26.3.27 и v26.10.10 `command.proto` не менялся (`git diff --stat` пуст; в `command.go` изменилась одна строка). **Новых балансерных RPC нет.** По истории коммитов: `ee2b2c5a` (leastLoad `tolerance`), `6c4008ed` (observatory чистит удалённые outbound’ы), `f124daf5` (observatory без 100% CPU), `598bde74` (балансеры за `atomic.Pointer`), `c4dfcd4c`/`ca4b156b` (исправления burst observatory).

### 2.2. Реализация `GetBalancerInfo` (`xray:app/router/command/command.go:21-47`)

```
ret.Balancer = &BalancerMsg{}
if router implements BalancerOverrider:                    // *router.Router реализует всегда
    res, err = GetOverrideTarget(tag)                      // balancing.go:162-167
    if err → return nil, err                               // "app/router: cannot find tag"
    ret.Balancer.Override = &OverrideInfo{Target: res}     // всегда не-nil, "" если override нет
if router implements BalancerPrincipleTarget:
    res, err = GetPrincipleTarget(tag)                     // balancing.go:138-150
    if err → только LogInfo, principle_target остаётся nil // ошибка RPC НЕ возвращается
    else  ret.Balancer.PrincipleTarget = {Tag: res}
```

`Router.GetPrincipleTarget` (`xray:app/router/balancing.go:138-150`) берёт балансер из `*r.balancers.Load()`, вызывает `b.SelectOutbounds()` (кандидаты по префиксу selector’а), затем `strategy.GetPrincipleTarget(candidates)`. Все четыре стратегии реализуют `BalancingPrincipleTarget`, поэтому ветку `unsupported GetPrincipleTarget` на практике не увидеть.

Кандидаты (`xray:app/proxyman/outbound/outbound.go:164-188`, `Manager.Select`) — **все** зарегистрированные outbound’ы, чей тег начинается с одного из selector’ов, `sort.Strings`. Результат кэшируется по ключу selector’ов, кэш сбрасывается на каждом `AddOutbound`/`RemoveOutbound` (`:107`, `:138`). Добавленные или удалённые через API outbound’ы сразу видны в кандидатах (подтверждено в §5).

**Ошибки** (проверено вживую):
* неизвестный tag балансера → `codes.Unknown`, `app/router: cannot find tag`;
* `RoutingService` не включён в `api.services` → `codes.Unimplemented`, `unknown service xray.app.router.command.RoutingService`.

### 2.3. Как реально выбирается outbound (`Balancer.PickOutbound`, `xray:app/router/balancing.go:95-119`)

```
candidates = SelectOutbounds()                       // ошибка → fallbackTag или error
tag = override.Get() != "" ? override : strategy.PickOutbound(candidates)
if tag == "":
    fallbackTag != "" → fallbackTag
    иначе → error "balancing strategy returns empty tag"
```

Цепочка: `Rule.GetTag()` (`xray:app/router/config.go:21-26`) → `Router.PickRoute` (`xray:app/router/router.go:57-75`). Ошибка `PickRoute` в dispatcher’е ведёт к `default route` (`xray:app/dispatcher/default.go:472-481`): трафик уходит в **default outbound**, то есть в первый outbound конфига (у нас это `direct`). Если же PickRoute вернул несуществующий тег, соединение **закрывается** (`default.go:466-470`, «DO NOT CHANGE…»).

Override:
* `override` хранится в объекте `Balancer` (`xray:app/router/balancing_override.go`) и **не валидируется**: годится любая строка, в том числе тег вне selector’а или несуществующий.
* `OverrideBalancerTarget(tag, "")` — сброс. Для неизвестного балансера ответ `app/router: cannot find tag`.
* `AddRule(shouldAppend=true)` копирует старые `*Balancer` по указателю (`router.go:101`, `maps.Copy`), поэтому override **переживает** добавление и удаление правил scd. Сбрасывается он только рестартом Xray или `ReloadRules(shouldAppend=false)` с новым набором балансеров.
* `GetPrincipleTarget` override **не учитывает**.

### 2.4. Что возвращает `principle_target` для каждой стратегии

`BalancingRule.Build` (`xray:app/router/config.go:125-169`): `leastping` → `LeastPingStrategy`, `roundrobin` → `RoundRobinStrategy`, `leastload` → `LeastLoadStrategy`, `random`/`""` → `RandomStrategy`. Регистр значения не важен.

| Стратегия | `GetPrincipleTarget` | `PickOutbound` (реальный выбор) | Детерминированность | Нет живых кандидатов |
|---|---|---|---|---|
| **leastPing** (`strategy_leastping.go`) | `[]string{l.PickOutbound(candidates)}` (`:18-20`) — **ровно тот же** вызов | Минимальный `Delay` среди статусов observatory с `Alive && tag ∈ candidates`. Строгое `<`, при равенстве побеждает первый в порядке `status` (`:30-56`) | Да, при неизменном observatory. Меняется не чаще одного раза за цикл probe | `[""]` |
| **leastLoad** (`strategy_leastload.go`) | теги всего отобранного множества `pickOutbounds` (`:28-35`) | `selects[dice.Roll(len)]` (`:70-78`) | `expected ≤ 1` и без baselines → 1 тег, совпадает с реальным выбором. Иначе это множество, реальный выбор случаен для каждого соединения | `[]` |
| **random** (`strategy_random.go`) | `candidates` целиком (`:31-33`) | `candidates[dice.Roll(n)]`; при заданном `fallbackTag` сначала отсев мёртвых по observatory (`:35-67`) | Выбора «сейчас» нет | все кандидаты (мёртвые тоже) |
| **roundRobin** (`balancing.go:42-83`) | `candidates` целиком (`:42-44`) | `tags[index % n]`, `index++` под mutex. С `fallbackTag` — только живые | Выбора «сейчас» нет; индекс общий для всех соединений | все кандидаты |

Детали leastLoad (`strategy_leastload.go`):
* `getNodes` (`:142-182`) берёт статусы observatory, где `Alive`, `Delay < maxRTT` (если задан), тег входит в кандидаты, а доля fail не больше `tolerance` (`:184-199`). Сортировка (`:201-219`): `RTTDeviationCost` → `RTTAverage` → `CountFail` → `CountAll desc` → `Tag`.
* С обычным `observatory` (без `health_ping`) `RTTDeviation = RTTAverage = Delay`. Тогда leastLoad(expected=0) ведёт себя как leastPing, только с детерминированным tie-break по тегу.
* С `burstObservatory` сортировка идёт по **стандартному отклонению** RTT (`HealthPing.Deviation`), а не по среднему. Живой прогон (§5.3): стабильно «медленный» outbound (300 ms, dev 0.1 ms) обгоняет быстрый (21 ms, dev 6 ms), и выбор переключается между циклами.
* `selectLeastLoad` (`:102-140`): `expected > available` → все; `expected ≤ 0` → 1; с baselines — пока `RTTDeviationCost < baseline`, затем добивка до `expected`.

Вывод для дашборда:
* «Выбранный outbound» существует только в двух случаях: **leastPing**, а также **leastLoad с одним выбранным узлом**, и ещё при активном override.
* Для random/roundRobin и для leastLoad с множеством можно показать только **набор кандидатов** (pool).
* Какая стратегия настроена, API не сообщает. scd определяет режим по форме ответа (§6.2) либо берёт подсказку из конфига.

### 2.5. Почему не `TestRoute` и не `SubscribeRoutingStats`

* `TestRoute` (`command.go:92-105`) вызывает настоящий `PickRoute`, поэтому даёт реальный выбор с учётом override и fallbackTag. Минусы:
  * **побочные эффекты**: сдвигается индекс roundRobin (вживую: `rr.test` → `dead, fast, mid, slow` за 4 вызова), срабатывают `webhook`-правила, расходуется `dice`;
  * нужен `RoutingContext`, подходящий под правило (`geoip:!ru` → нужен нероссийский `TargetIPs`), а под Lua `routing.script` поведение другое (`08` §3);
  * при отсутствии кандидатов возвращает ошибку `app/router: balancing strategy returns empty tag`.

  Годится только как опциональный отладочный инструмент, для периодического опроса — нет.
* `SubscribeRoutingStats` всегда отвечает `Routing statistics not enabled.`: сервис создаётся с `routingStats = nil` (`command.go:144-145`, `:107-110`). Реальный поток выборов через API получить нельзя.

---

## 3. ObservatoryService

### 3.1. Сообщения

`xray:app/observatory/command/command.proto:11-21`: `GetOutboundStatusRequest {}` → `GetOutboundStatusResponse { xray.core.app.observatory.ObservationResult status = 1 }`. Full method path: `/xray.core.app.observatory.command.ObservatoryService/GetOutboundStatus`. Go-пакет: `github.com/xtls/xray-core/app/observatory/command`, типы — `github.com/xtls/xray-core/app/observatory`.

`xray:app/observatory/config.proto:9-50`:

| `OutboundStatus` | № | Тип | Обычный `observatory` | `burstObservatory` |
|---|---|---|---|---|
| `alive` | 1 | bool | результат последнего probe | `All != Fail` (при `All == 0` → false) |
| `delay` | 2 | int64, **ms** | время GET; мёртвый → **99999999** | `Average` в ms; мёртвый → 0 |
| `last_error_reason` | 3 | string | `the outbound <tag> is dead: GET request failed:…` (текст человекочитаемый, `NotMachineReadable`) | всегда `""` |
| `outbound_tag` | 4 | string | тег | тег |
| `last_seen_time` | 5 | int64, **Unix seconds** | время последнего успешного probe, 0 если ни одного | всегда 0 |
| `last_try_time` | 6 | int64, Unix seconds | время последнего probe | всегда 0 |
| `health_ping` | 7 | `HealthPingMeasurementResult{all=1, fail=2, deviation=3, average=4, max=5, min=6}` | nil | заполнено; `deviation/average/max/min` — **наносекунды** (`time.Duration`) |

Реализации:
* обычный observer: `xray:app/observatory/observer.go`. `GetObservation` (`:41-43`) возвращает внутренний срез без копирования и без lock’а (гонка на стороне Xray, нас не касается). Записи создаются в порядке завершения первого probe (`:203-225`), поэтому при `enableConcurrency` **порядок недетерминирован**. `clearRemovedOutbounds` (`:121-134`) вызывается в **начале** каждого цикла (`:64-119`), так что удалённый outbound остаётся в ответе до `probeInterval + время probe` (вживую ~5 s). Новый outbound не появится, пока по нему не завершится первый probe. Таймаут probe — 5 s (`:166-173`).
* burst: `xray:app/observatory/burst/burstobserver.go:30-62`, итерация по Go-map, порядок случайный. Интервал < 10 s принудительно поднимается до 10 s (`healthping.go:66-69`).

Ошибки:
* `ObservatoryService` не включён → `codes.Unimplemented`, `unknown service xray.core.app.observatory.command.ObservatoryService`.

### 3.2. Включение сервисов

* Имена в `api.services` сравниваются без учёта регистра (`xray:infra/conf/api.go:28-43`): `"ObservatoryService"` → `observatoryservice.Config`, `"RoutingService"` → `routerservice.Config`. **Неизвестные имена молча игнорируются**, так что опечатка проявится только как `Unimplemented`.
* `ObservatoryService` при создании требует feature `extension.Observatory` (`xray:app/observatory/command/command.go:35-47`, `RequireFeatures(…, false)`). Без `observatory`/`burstObservatory` **Xray не стартует**: `Failed to start: main: failed to create server > core: not all dependencies are resolved.` (`xray:core/xray.go:243`). Проверено на `run -test`. То же происходит с балансером `leastPing`/`leastLoad` без observatory (`strategy_leastping.go:22-28`, `common.Must(RequireFeatures)`).
* Для `GetBalancerInfo` нужен `RoutingService`, в нашем конфиге он уже включён. Для сопоставления живости нужен `ObservatoryService`.

**Требуемое изменение конфига** (`docker/xray/config/01_api.json`):

```json
{
  "api": {
    "tag": "api",
    "listen": "0.0.0.0:8080",
    "services": ["HandlerService", "LoggerService", "StatsService", "RoutingService", "ObservatoryService"]
  }
}
```

Это изменение безопасно только при наличии `05_observatory.json`. Документировать в README: «ObservatoryService требует observatory или burstObservatory, иначе Xray не запустится». Значение `probeInterval: 15s` задаёт, как часто может меняться выбор leastPing. Опрос scd чаще этого интервала бесполезен.

---

## 4. Префикс, selector и туннели scd

1. **Селекторы.** Балансер (`selector: ["x-observe-"]`) и observatory (`subjectSelector: ["x-observe-"]`) работают через тот же `HandlerSelector.Select` по префиксу. У scd outbound’ы initial (`x-observe-<tag>`) попадают в оба. Repaired outbound (`<tag>`, без префикса) **не попадает ни в один**: балансер его не выберет, а observatory не будет наблюдать (в живом прогоне `tunnel-repaired`/`tunnel-fast` отсутствуют и в `principle_target`, и в `GetOutboundStatus`).
2. **Сопоставление «тег → туннель»** (в порядке приоритета):
   1. точное совпадение `tag == tunnel.outboundTagCurrent` в текущей topology target’а. Это надёжно и работает даже без префикса, если оператор сам настроил selector;
   2. `tag == tunnel.outboundTagInitial`, если туннель сейчас repaired. Такое возможно только при гонке, потому что балансер не видит unprefixed-тег;
   3. если `prefix != ""` и `strings.HasPrefix(tag, prefix)` → `baseTunnelId = strings.TrimPrefix(tag, prefix)` → поиск в `tunnels`. Это запас на случай гонки с заменой topology;
   4. иначе тег **неуправляемый** (unmanaged): статический outbound из конфига Xray с тем же префиксом или чужой. Показывать сырой тег.
3. **Selector общий для всего Xray.** Если на один Xray смотрят несколько target’ов или в статическом конфиге есть свои `x-observe-*`, балансер выбирает и среди них. scd показывает такой выбор как unmanaged. Уникальность `target.address` по подпискам уже проверяется в конфиге (`load-config.ts`, superRefine по `addresses`).
4. **Rejoin.** Новый prefixed outbound не наблюдается до завершения первого probe (до `probeInterval` + 5 s). Всё это время leastPing/leastLoad его **не выбирают** (вживую: после повторного `AddOutbound(x-observe-fast)` выбор оставался `x-observe-mid` до следующего цикла). На дашборде: «в балансере, ждёт наблюдения» (`awaiting observation`).
5. **Устаревшие записи observatory.** После `RemoveOutbound` статус удалённого тега ещё до ~1 цикла отдаётся `GetOutboundStatus` (вживую `x-observe-fast` висел с `alive:true` после удаления), хотя `principle_target` уже его исключил. Поэтому observatory-данные нужно **фильтровать по текущим тегам topology**, а не показывать все подряд.
6. **Нет живых кандидатов.** leastPing → `[""]`, leastLoad → `[]`. Реальный трафик при этом уходит в `fallbackTag` (если задан) или в **default outbound** (у нас `direct`, то есть трафик, предназначенный для балансера, идёт напрямую). Пометка на дашборде: `no alive candidate → fallback/default outbound`.

---

## 5. Живая проверка (v26.10.10)

### 5.1. Методика

* Бинарь `scratchpad/bin-xray-26.10.10` (`Xray 26.10.10 … 701af60 (go1.27.0)`), собран ранее из клона.
* Конфиг `scratchpad/balcfg/main/` — копия `docker/xray/config` со следующими изменениями:
  * API на `127.0.0.1:18180`, в services добавлен `ObservatoryService`;
  * observatory: `probeURL: http://probe.test/generate_204`, `probeInterval: 2s`;
  * outbound’ы `x-observe-fast|mid|slow` — `freedom` с `redirect` на локальные HTTP-серверы с задержкой 20/120/300 ms (`x2610/balprobe`), `x-observe-dead` — redirect на закрытый порт, `tunnel-repaired` — без префикса;
  * пять балансеров на одном selector’е `x-observe-`: `x-balancer` (leastPing), `bal-load` (leastLoad), `bal-random`, `bal-rr` (roundRobin), `bal-ping-fb` (leastPing + `fallbackTag: direct`).
* Вариант `balcfg/burst/`: `burstObservatory` + leastPing, leastLoad, leastLoad `{expected:2, baselines:[1s], maxRTT:1s, tolerance:0.5}`.
* Вариант `balcfg/noobs-svc/`: без `ObservatoryService`. `balcfg/norouting/`: без `RoutingService`. `balcfg/noobs*/`: без observatory.
* Клиент: `scratchpad/x2610/balclient/main.go` на xray-core `v1.260327.1-0.20261010092107-701af60772cd`, использует `rc.NewRoutingServiceClient` и `oc.NewObservatoryServiceClient`, вывод через protojson. Лог: `scratchpad/bal-live-main.txt`.

### 5.2. Результаты (обычный observatory)

```
GetBalancerInfo(x-balancer):  {"override":{"target":""}, "principleTarget":{"tag":["x-observe-fast"]}}
GetBalancerInfo(bal-load):    {"override":{"target":""}, "principleTarget":{"tag":["x-observe-fast"]}}
GetBalancerInfo(bal-random):  {"override":{"target":""}, "principleTarget":{"tag":["x-observe-dead","x-observe-fast","x-observe-mid","x-observe-slow"]}}
GetBalancerInfo(bal-rr):      то же, что random
GetBalancerInfo(bal-ping-fb): {"override":{"target":""}, "principleTarget":{"tag":["x-observe-fast"]}}
GetOutboundStatus order=[x-observe-fast x-observe-mid x-observe-slow x-observe-dead]
  {"alive":true,  "delay":"21",       "lastErrorReason":"", "outboundTag":"x-observe-fast", "lastSeenTime":"1792229310", "lastTryTime":"1792229310", "healthPing":null}
  {"alive":false, "delay":"99999999", "lastErrorReason":"the outbound x-observe-dead is dead: GET request failed:app/observatory: outbound failed t…", "outboundTag":"x-observe-dead", "lastSeenTime":"0", "lastTryTime":"1792229311", "healthPing":null}
```

| Сценарий | Результат |
|---|---|
| 3 повторных `GetBalancerInfo` подряд | идентичны для всех стратегий (у random/rr это просто список кандидатов) |
| `TestRoute` ×4: leastPing / leastLoad / fb | `fast ×4` — совпадает с `principle_target` |
| `TestRoute` ×4: random | `fast, slow, dead, dead` — мёртвый выбирается (без `fallbackTag` observatory не учитывается) |
| `TestRoute` ×4: roundRobin | `dead, fast, mid, slow` — индекс сдвигается при каждом вызове TestRoute |
| `GetBalancerInfo("no-such-balancer")` | `code=Unknown msg="app/router: cannot find tag"` |
| `OverrideBalancerTarget(x-balancer, x-observe-slow)` | `override.target="x-observe-slow"`, **`principle_target` остаётся `["x-observe-fast"]`**, `TestRoute` → `x-observe-slow` |
| override на `nonexistent-tag` / `tunnel-repaired` | принимается без ошибки; `TestRoute` → `nonexistent-tag` (в реальном трафике соединение закрылось бы) |
| override `""` | сброс, `override.target=""` |
| `OverrideBalancerTarget(no-such-balancer, …)` | `app/router: cannot find tag` |
| repair-подобно: `RemoveOutbound(x-observe-fast)` + `AddOutbound(tunnel-fast)` | сразу: leastPing/leastLoad → `x-observe-mid`, random/rr → без fast; `GetOutboundStatus` **ещё показывает `x-observe-fast`** (устаревшая запись); через 5 s она исчезает; `tunnel-fast` в observatory не появляется |
| rejoin-подобно: `AddOutbound(x-observe-fast)` | сразу: random/rr уже включают его, leastPing остаётся на `x-observe-mid` (наблюдения нет); через 5 s → `x-observe-fast` |
| удалены все живые | leastPing → `"tag":[""]`, leastLoad → `"tag":[]`, random/rr → `["x-observe-dead"]`; `TestRoute`: leastPing/leastLoad → `app/router: balancing strategy returns empty tag` (= default outbound), `bal-ping-fb` → `direct` |
| без `ObservatoryService` | `GetOutboundStatus`: `code=Unimplemented msg="unknown service xray.core.app.observatory.command.ObservatoryService"`, `GetBalancerInfo` работает |
| без `RoutingService` | `GetBalancerInfo`: `code=Unimplemented msg="unknown service xray.app.router.command.RoutingService"`, observatory работает |
| `ObservatoryService` или leastPing без observatory | Xray не стартует: `core: not all dependencies are resolved.` |

### 5.3. Burst observatory

```
t+1s:  bal-load  → ["x-observe-slow"]   (у slow 2 замера, deviation 0.1 ms; у fast 1 замер, deviation = average/2)
t+25s: bal-load  → ["x-observe-fast"]
t+45s: bal-load  → ["x-observe-slow"]   (fast: dev 6.1 ms из-за одного выброса 35 ms; slow: dev 0.1 ms)
       x-balancer (leastPing) → всегда ["x-observe-fast"] (сравнивает delay = Average ms)
       bal-load2 (expected 2 + baseline 1s) → все 3 живых, порядок по deviation
x-observe-dead: {"alive":false, "delay":"0", "healthPing":{"all":"3","fail":"3",...}}, lastSeenTime/lastTryTime = 0
healthPing.average = "21748562" (ns) ↔ delay = "21" (ms)
```

---

## 6. Проектирование для scd (Go)

### 6.1. `xrayapi.Client`: новые методы

В пакете `internal/scd/xrayapi`, рядом с `OutboundAPI/InboundAPI/RoutingAPI` (`02` §5):

```go
// BalancerAPI — только чтение; OverrideBalancerTarget scd не вызывает (см. 6.8).
type BalancerAPI interface {
    GetBalancerInfo(ctx context.Context, balancerTag string) (BalancerInfo, error)
}

type ObservatoryAPI interface {
    GetOutboundStatus(ctx context.Context) ([]OutboundObservation, error)
}

type BalancerInfo struct {
    OverrideTarget   string   // "" = override нет
    PrincipleTargets []string // нормализовано: пустые строки выброшены ([""] → [])
    PrincipleKnown   bool     // false, если principle_target == nil (Xray залогировал ошибку)
}

type OutboundObservation struct {
    Tag         string
    Alive       bool
    DelayMs     *int64     // nil, если !Alive (99999999 / 0 → nil)
    LastError   string     // last_error_reason как есть
    LastSeenAt  *time.Time // nil, если 0; Unix seconds → time
    LastTryAt   *time.Time
    HealthPing  *HealthPing // только burstObservatory
}

type HealthPing struct {
    All, Fail                       int64
    Average, Deviation, Min, Max    time.Duration // ns на проводе
}
```

* Реализация: `rc.NewRoutingServiceClient(conn).GetBalancerInfo` и `oc.NewObservatoryServiceClient(conn).GetOutboundStatus` на том же shared `*grpc.ClientConn` и с тем же `context.WithTimeout(target.timeoutMs)` (`02` §2.7). Новых зависимостей нет: `app/router/command` уже импортируется, `app/observatory/command` и `app/observatory` — пакеты того же модуля (лёгкие, только proto и grpc).
* Маппинг ошибок — общий (`02` §2.7), плюс типизированные sentinel’ы:
  * `codes.Unimplemented` + `unknown service …RoutingService` → `ErrServiceDisabled{Service: "RoutingService"}`, текст `Xray API service RoutingService is not enabled (add it to api.services).`;
  * то же для `ObservatoryService`;
  * `codes.Unknown` + `cannot find tag` → `ErrBalancerNotFound{Tag}`, текст `Xray balancer "<tag>" not found.`.
* Порядок `GetOutboundStatus` не фиксировать: возвращать срез, отсортированный по `Tag`.

### 6.2. Вычисление «выбранного» (чистая функция, легко тестируется)

```go
type SelectionMode string // "override" | "single" | "pool" | "none" | "unknown"

func ResolveSelection(info BalancerInfo) (mode SelectionMode, selected string, pool []string)
    if info.OverrideTarget != ""          → ("override", info.OverrideTarget, info.PrincipleTargets)
    if !info.PrincipleKnown               → ("unknown", "", nil)
    switch len(info.PrincipleTargets):
      0 → ("none", "", nil)          // leastPing [""] / leastLoad []: fallbackTag или default outbound
      1 → ("single", targets[0], targets) // leastPing; leastLoad с 1 узлом
      _ → ("pool", "", targets)      // random / roundRobin / leastLoad expected>1
```

Тонкость: random/roundRobin с **одним** кандидатом тоже дают `single`. Это корректно, ведь выбирать больше не из чего.

Опциональная подсказка `balancerSelection.strategy` в конфиге (`leastPing|leastLoad|random|roundRobin`) нужна только для текста на дашборде («pool: random»). На логику она не влияет.

### 6.3. Каденс опроса

* **Собственный cron-цикл на target** (`internal/common/cronloop`, та же семантика `runCronLoop`: первый тик сразу, пропуск при overrun). Расписание `balancerSelection.schedule`. Поддерживается 6-польный cron с секундами (`06` §3), например `*/15 * * * * *` — по `probeInterval` observatory. Чаще опрашивать бессмысленно: выбор leastPing меняется только после цикла probe.
* Почему не на cron `balancerMonitor`: тот требует `socks5`+`request`, обычно настроен поминутно и семантически это внешний end-to-end-чек. Выбор балансера — дешёвый локальный read (2 unary RPC, сотни байт) и нужен даже без внешнего SOCKS.
* Почему не на каждый HTTP-запрос к status server: `/api/instance-status` часто опрашивают удалённые агрегаторы, а status server сейчас не ходит в Xray вообще (кроме `/api/runtime-state`). Это свойство нужно сохранить.
* Дополнительно (не обязательно): **внеочередной** неблокирующий тик после успешного apply в sync, после repair и после rejoin. Через ~1 цикл observatory выбор всё равно обновится плановым тиком.
* Связь с `balancerMonitor`: в его тике можно сохранить `selectedOutboundTagAtCheck` — последний известный selection на момент внешней проверки. Тогда на дашборде видно, через какой туннель прошла проверка. Это необязательно: значение берётся из state без нового RPC.
* В тике: `GetBalancerInfo` и `GetOutboundStatus` **параллельно** (`errgroup`), ошибки независимы. Target-mutex **не** берётся (только чтение, как в balancerMonitor; `03` §1.1). Обновление state — под state-lock (`03` §4.7). До первого успешного sync (нет `mem.targets[key]`) тик всё равно выполняется: выбор балансера от topology не зависит, отображение на туннели появится после sync. Это отличие от monitor-тиков, его нужно зафиксировать.

### 6.4. Runtime state

`TargetApplyState` получает новое поле (рядом с `balancerMonitor`; при замене topology **не** сбрасывается):

```go
type TargetBalancerSelectionState struct {
    State              string     // "idle" | "ok" | "error"
    BalancerTag        string
    Mode               string     // override|single|pool|none|unknown
    SelectedTag        string     // эффективный (override или single)
    OverrideTarget     string
    PrincipleTargets   []string
    SelectedSince      *time.Time // когда SelectedTag последний раз изменился
    PreviousSelectedTag string
    LastCheckedAt, LastSuccessAt, LastFailureAt *time.Time
    LastError          string     // ошибка GetBalancerInfo
    ConsecutiveFailures int
    Observatory        map[string]OutboundObservation // ключ — tag; nil, если сервис недоступен
    ObservatoryError   string     // ошибка GetOutboundStatus (например, сервис не включён)
    ObservatoryCheckedAt *time.Time
}
```

* Ошибка `GetBalancerInfo` → `State=error`, а `SelectedTag/PrincipleTargets` **сохраняются** от прошлого успеха, как `lastSuccess*` в мониторинге. Дашборд показывает их как устаревшие (`stale`) с текстом ошибки.
* Ошибка только observatory → `State` остаётся `ok`, заполняется `ObservatoryError`, `Observatory` очищается: по устаревшей живости не принимаем решений.
* Логи (`log/slog`, имена событий — как в каталоге `03` §8.1):
  * `info balancer_selection_changed {subscriptionId, targetAddress, balancerTag, from, to, mode}` — при смене `SelectedTag`/`Mode`;
  * `warn balancer_selection_failed {…, error}` — **только при переходе** `ok→error` или смене текста ошибки, чтобы не спамить каждые 15 s;
  * `info balancer_selection_recovered` — при переходе `error→ok`;
  * `warn balancer_observatory_unavailable` — один раз на текст ошибки.

### 6.5. Конфиг

```yaml
target:
  observatorySubjectSelectorPrefix: x-observe-
  balancerSelection:
    enabled: true
    balancerTag: x-balancer        # = routing.balancers[].tag в Xray (не ruleTag!)
    schedule: "*/15 * * * * *"     # 6 полей = с секундами
    observatory: true              # вызывать ObservatoryService (default true)
    # strategy: leastPing          # опциональная подсказка для UI
```

Валидация (в стиле существующих правил `load-config.ts:345-392`):
* `balancerSelection.balancerTag is required when balancerSelection.enabled is true.`;
* `balancerSelection.schedule is required when balancerSelection.enabled is true.` + разбор cron;
* `strict` (неизвестные ключи → ошибка), default `{enabled:false, observatory:true}`;
* без `observatorySubjectSelectorPrefix` — не ошибка: точное совпадение тегов работает, но в `validate-config` выводится предупреждение (warning), что без префикса туннели scd вряд ли попадут в selector.

`/api/runtime-state` → `config.target.balancerSelection` выводится как есть (секретов нет).

### 6.6. JSON API (аддитивно, обратная совместимость)

**`version` остаётся `1`.** `parseRemoteSnapshot` (TS и Go-порт, `03` §7) требует `version === 1`. Повышение версии сломает смешанный парк (новый инстанс → старый агрегатор: `unsupported schema`). Все новые поля опциональны и опускаются (`omitempty`), если фича выключена. Неизвестные поля старые агрегаторы пропускают насквозь (`03` §6.4: remote snapshot хранится как сырой JSON). Отдельный маркер возможностей не нужен: наличие ключа `balancerSelection` у target уже означает поддержку и включённость.

`GroupedStatusTarget` (в `/api/instance-status` → `subscriptions[].targets[]`) получает новый ключ после `balancerMonitor`:

```json
"balancerSelection": {
  "balancerTag": "x-balancer",
  "state": "ok",
  "mode": "single",
  "selectedOutboundTag": "x-observe-de-1",
  "selectedTunnelId": "de-1",
  "selectedDisplayName": "🇩🇪 Germany 1",
  "selectedCountryIso2": "DE",
  "selectedManaged": true,
  "selectedDelayMs": 42,
  "overrideTarget": "",
  "principleTargets": ["x-observe-de-1"],
  "selectedSince": "2026-10-17T09:00:00.000Z",
  "lastCheckedAt": "…", "lastSuccessAt": "…", "lastFailureAt": "…",
  "lastError": "…",
  "observatoryError": "Xray API service ObservatoryService is not enabled (add it to api.services)."
}
```

`StatusSnapshotTunnel` (в `/api/status`, `tunnels[]` и вложенных) получает поля после `balancerMonitor*`, в том же плоском стиле:

```json
"outboundTag": "x-observe-de-1",
"balancerSelected": true,
"balancerCandidate": true,
"observatoryAlive": true,
"observatoryDelayMs": 42,
"observatoryLastError": "…",
"observatoryLastSeenAt": "2026-10-17T09:00:00.000Z",
"observatoryLastTryAt": "2026-10-17T09:00:15.000Z",
"observatoryHealthPingAll": 3, "observatoryHealthPingFail": 0,
"observatoryHealthPingAverageMs": 21.7, "observatoryHealthPingDeviationMs": 0.06
```

* `balancerSelected`: `selectedOutboundTag == tunnel.outboundTagCurrent`. `balancerCandidate`: тег входит в `principleTargets`. Для repaired-туннелей оба `false`, observatory-полей нет. Причина видна по существующему `balanced:false`.
* Временные метки — формат `03` §1 (`…000Z`), из Unix seconds.
* `/api/runtime-state` получает `serviceState.balancerSelection` (полный state, включая `consecutiveFailures`) и опционально `runtime.balancer` — **живой** вызов `GetBalancerInfo`+`GetOutboundStatus` параллельно с `List*`. Ошибки live-вызова **не** должны переводить ответ в 502: этот эндпоинт про ресурсы, поэтому ошибку класть в `runtime.balancer.error`.
* `/api/instances`: без изменений, поля проходят насквозь.

### 6.7. Дашборд

Внутри `target-card`, сразу **после** существующей balancer-card «External SOCKS check» и перед `nodes-grid` — вторая карточка того же класса (`balancer-card state-{…}`):

| Элемент | Значение |
|---|---|
| kicker | `Balancer` |
| title | `Xray selection · {balancerTag}` |
| `Selected` | `{countryIso2} · {displayName}` для управляемого туннеля; сырой тег для unmanaged; `override → {tag}` при override; `pool ({n})` для random/rr; `none (fallback/default outbound)` при отсутствии живых |
| `Latency` | `observatoryDelayMs` выбранного → `"<n> ms"`, иначе `-` |
| `Since` | `selectedSince` |
| `Observatory` | `{alive}/{total} alive` или `unavailable` (tooltip/`title` = `observatoryError`) |
| state-класс | `healthy` — `ok` + `single/override` + выбранный alive; `degraded` — `error`, `none`, override на тег вне topology/selector, выбранный dead; `idle` — ещё не опрашивали |

Карточки туннелей (`node-card`):
* к выбранному туннелю добавляется класс `is-selected` (рамка или бейдж `Selected`); кандидатам в режиме `pool` — бейдж `Pool`;
* новая метрика `Observatory`: `alive · 42 ms` / `dead` (полный `last_error_reason` — в `title`) / `n/a` (repaired, вне selector’а или сервис недоступен) / `awaiting` (prefixed outbound ещё без наблюдения);
* сортировка карточек **не меняется** (`compareLatencyThenName` по latency монитора scd, `03` §6.4). Латентность observatory — отдельная величина: probe идёт изнутри Xray к `probeURL`.

Требование D3 (визуально как сейчас): если `balancerSelection` отсутствует (фича выключена или старый remote-инстанс), разметка и CSS для блока не выводятся, и существующие эталоны (baseline) visual-regression остаются валидными. Для включённой фичи — новые эталоны на фикстурах `single`, `override`, `pool`, `none`, `error`, `observatory unavailable`, `repaired tunnel`.

### 6.8. Что сознательно не делаем

* **`OverrideBalancerTarget` из scd/UI не вызываем.** Он не валидирует тег: опечатка рвёт весь трафик балансера (§2.3). Override переживает правки правил и сбрасывается только рестартом Xray. Status server сейчас только читает, мутации — лишь в sync/repair/rejoin под target-mutex. Если когда-нибудь понадобится, это отдельный POST-эндпоинт с проверкой `target ∈ principle-кандидаты ∪ topology` и под target-mutex.
* `TestRoute` для периодического опроса не используем (побочные эффекты, §2.5).

### 6.9. Тесты

**Fake на bufconn** (расширение fake из `02` §6):

* `fakeRouting` встраивает `rc.UnimplementedRoutingServiceServer` (`command_grpc.pb.go:150`). Хранит `balancers map[string]*fakeBalancer{selectors, strategy, override}` поверх уже существующего map outbound’ов fake HandlerService. Семантика v26.10.10:
  * неизвестный tag → `status.Error(codes.Unknown, "app/router: cannot find tag")`; `Override` всегда не-nil;
  * кандидаты = outbound’ы с префиксом, `sort.Strings`;
  * leastPing: min delay среди alive из fake-observatory, нет таких → `[""]`; leastLoad (expected 0) — то же с tie-break по тегу, нет → `[]`; random/rr → все кандидаты;
  * `OverrideBalancerTarget` хранит строку без валидации (для тестов парсинга override);
  * per-method инъекция ошибок и задержек.
* `fakeObservatory` встраивает `oc.UnimplementedObservatoryServiceServer` (`command_grpc.pb.go:63`). Срез статусов в **перемешанном** порядке (тест не должен зависеть от порядка); мёртвые с `delay=99999999`; режим burst с `health_ping` в ns и `last_*_time=0`; устаревшая запись для удалённого тега.
* «Сервис не включён»: просто **не регистрировать** сервис на `grpc.Server` — grpc-go сам вернёт `Unimplemented` с `unknown service …`, как Xray.

Тест-кейсы:
1. `xrayapi`: декодирование `[""]`→`[]`, `nil principle`→`PrincipleKnown=false`, `delay 99999999`→`nil`, Unix seconds→time, ns→Duration; маппинг `Unimplemented`/`cannot find tag`/`DeadlineExceeded`/`connection refused` в тексты.
2. `ResolveSelection`: таблица `override × len(principle) × PrincipleKnown`.
3. Сопоставление тегов: prefixed → туннель; repaired (unprefixed `outboundTagCurrent`) → не выбран, observatory `n/a`; rejoin-окно (тег в кандидатах, нет наблюдения) → `awaiting`; unmanaged `x-observe-static`; устаревшая запись observatory удалённого тега отфильтрована; пустой prefix + точное совпадение.
4. Тик (`testing/synctest`): `idle→ok`, `ok→error` (selected сохраняется, `lastFailureAt`, 1 лог), повторная та же ошибка (без лога), `error→ok` (`recovered`), смена выбора (`changed`, `selectedSince` обновлён), observatory недоступен при рабочем routing; тик идёт параллельно с sync apply и не блокируется target-mutex’ом; overrun → пропуск.
5. JSON: фича выключена → байты `/api/status` и `/api/instance-status` **идентичны** текущим эталонам; включена → эталоны с новыми ключами в фиксированном порядке; `parseRemoteSnapshot` принимает снимок с новыми полями (version 1) и без них.
6. HTML: unit-тесты рендера блока (тексты `Selected/Latency/Since/Observatory`, классы `is-selected`, `state-*`) + visual regression (§6.7).
7. Конфиг: обязательность `balancerTag`/`schedule`, strict-ключи, 6-польный cron, warning без prefix.
8. Опционально, build tag `xray_live`: интеграционный прогон против реального `xray` 26.10.10 со стендом из §5.1 (`balprobe` + `balcfg/main`). В CI — через образ `ghcr.io/xtls/xray-core:26.10.10` и docker compose smoke.

### 6.10. Изменения вне кода scd

* `docker/xray/config/01_api.json`: + `"ObservatoryService"` (§3.2).
* `docker/scd/config.example.yml`: блок `balancerSelection` с `balancerTag: x-balancer`, `schedule: "*/15 * * * * *"`.
* README scd: раздел «Balancer selection» — семантика по стратегиям (§2.4), требование `ObservatoryService`+observatory, ограничение «repaired-туннели вне балансера», предупреждение про override.
* `02-scd-xray-api.md` §1.2: список используемых RPC расширяется до 11 (`GetBalancerInfo`, `GetOutboundStatus`). D2 «тот же набор RPC» относится к мутациям, новые RPC только читают. **Требует подтверждения владельца** как дополнение к D2.

---

## 7. Открытые вопросы владельцу

1. Подтвердить расширение набора RPC (D2) двумя read-only вызовами и правку `01_api.json`.
2. Каденс по умолчанию: `*/15 * * * * *` (= `probeInterval`) или поминутно, как остальные циклы?
3. Нужна ли в UI подсказка стратегии (`strategy`) для режима `pool`, или достаточно «pool (n)»?
4. Нужен ли когда-нибудь override из UI (§6.8)? По умолчанию — нет.
5. Подсвечивать ли на дашборде ситуацию `none` (весь «зарубежный» трафик уходит в `direct`) как `degraded`? Предложено — да.

---

## Приложение: артефакты

* Клон Xray: `scratchpad/xray-core-26.10.10` (тег `v26.10.10`, `701af607`). Бинарь: `scratchpad/bin-xray-26.10.10`.
* Конфиги стенда: `scratchpad/balcfg/{main,burst,noobs-svc,norouting,noobs,noobs2}/`.
* Probe-сервер: `scratchpad/x2610/balprobe/main.go` (`bin_balprobe 18201:20 18202:300 18203:120`).
* Клиент: `scratchpad/x2610/balclient/main.go` (`bin_balclient 127.0.0.1:18180 {t0|main|kill|plain} <tags,…>`).
* Лог прогона: `scratchpad/bal-live-main.txt`; логи Xray: `scratchpad/bal-*.xlog`.
