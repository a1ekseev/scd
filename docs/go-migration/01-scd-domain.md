# 01 — scd: доменный слой (спецификация для Go-порта)

Статус: research-only. Источник истины — TypeScript в `scd/src` (ветка `move-to-go`).
Охват: конфиг, загрузка входа, парсинг/нормализация VLESS, flag→country, манифест,
фильтры, генерация JSON outbound, target override, топология туннелей, CLI, ошибки,
лог-события доменного уровня. **Не входит**: Xray gRPC клиент, apply-логика,
runtime-циклы (daemon/monitor/status-server) — только точки стыка.

Все утверждения про поведение проверены запуском собранного `dist/` (Node 22, zod 4.1.5,
yaml 2.8.1, cron-parser 5.4.0, commander 14.0.2) — см. раздел «Проверенные краевые случаи».
Ссылки вида `file:line` даны относительно `scd/`.

---

## 0. Конвейер целиком

```
config.yml ──readFile──► ${ENV} interpolation ──► YAML parse ──► schema (defaults + validation)
                                                          │
                                                          ▼
                              AppConfig (input резолвится относительно каталога конфига)
                                                          │
         loadSubscriptions: enabled-only, sort by (id, input) localeCompare
                                                          │
            loadInputSource(input, {format, fetchTimeoutMs}) → {source, content, encoding}
                                                          │
  generateManifestFromSubscription: buildManifest(content.trim(), id) → applyManifestFilters
                                   → validateManifestOrThrow
                                                          │
       scanLines → parseSubscriptionLine → normalizeVless → dedupe by tag → buildOutboundJson
                                                          │
   buildTargetTopology(manifest, target): sort by tag, prefix, visionUdp443Override, ports
```

`generate` (CLI) — укороченная ветка: `loadInputSource(input)` (auto, 5000ms) →
`buildManifest(content, source)` **без trim и без фильтров** → `validateManifestOrThrow` →
запись двух файлов.

---

## 1. Типы данных (Go-эскизы)

Порядок полей в struct = порядок ключей в JSON, который выдаёт TS (`JSON.stringify`
сохраняет порядок вставки). `omitempty`-семантика TS: поле с `undefined` **отсутствует**,
но пустая строка `""`, `false`, `0`, `[]` — **присутствуют**. В Go для опциональных строк,
которые могут быть законно `""`, использовать `*string` (см. `headerType`).

### 1.1 Конфиг (`src/types.ts:196-331`, схема `src/config/load-config.ts:23-439`)

```go
type AppConfig struct {
    Subscriptions []SubscriptionConfig `json:"subscriptions"`
    Runtime       RuntimeConfig        `json:"runtime"`
    Logging       LoggingConfig        `json:"logging"`
    Resources     ResourceConfig       `json:"resources"`
    StatusServer  StatusServerConfig   `json:"statusServer"`
}

type SubscriptionConfig struct {
    ID             string                     `json:"id"`
    Input          string                     `json:"input"`           // после резолва путей
    Enabled        bool                       `json:"enabled"`         // default true
    Format         string                     `json:"format"`          // auto|plain|base64, default auto
    FetchTimeoutMs int                        `json:"fetchTimeoutMs"`  // default 5000
    Filters        *SubscriptionFiltersConfig `json:"filters,omitempty"`
    Target         SubscriptionTargetConfig   `json:"target"`
}

type SubscriptionFiltersConfig struct {
    CountryAllowlist  []string `json:"countryAllowlist,omitempty"`  // nil если ключа нет; [] сохраняется если задан []
    LabelIncludeRegex *string  `json:"labelIncludeRegex,omitempty"` // JS regex literal "/.../flags"
}

type SubscriptionTargetConfig struct {
    Address              string                      `json:"address"`
    TimeoutMs            int                         `json:"timeoutMs"`            // default 5000
    FixedOutbounds       []string                    `json:"fixedOutbounds"`       // default []
    FixedInbounds        []string                    `json:"fixedInbounds"`        // default []
    FixedRouting         []string                    `json:"fixedRouting"`         // default []
    VisionUdp443Override bool                        `json:"visionUdp443Override"` // default false
    InboundSocks         *InboundSocksConfig         `json:"inboundSocks,omitempty"`
    Monitor              TargetMonitorConfig         `json:"monitor"`
    BalancerMonitor      TargetBalancerMonitorConfig `json:"balancerMonitor"`
    ObservatorySubjectSelectorPrefix *string         `json:"observatorySubjectSelectorPrefix,omitempty"`
}

type InboundSocksConfig struct {
    Listen    string `json:"listen"`
    PortRange struct {
        Start int `json:"start"`
        End   int `json:"end"`
    } `json:"portRange"`
}

type TargetMonitorConfig struct {
    Enabled     bool                        `json:"enabled"`            // default false
    Schedule    *string                     `json:"schedule,omitempty"`
    MaxParallel int                         `json:"maxParallel"`        // default 10
    Retry       RetryConfig                 `json:"retry"`              // default {1,250}
    Request     *TargetMonitorRequestConfig `json:"request,omitempty"`
}

type RetryConfig struct {
    Attempts int `json:"attempts"` // default 1, int >= 1
    DelayMs  int `json:"delayMs"`  // default 250, int >= 0
}

type TargetMonitorRequestConfig struct {
    URL            string `json:"url"`
    Method         string `json:"method"`         // GET|HEAD|POST, default GET
    ExpectedStatus int    `json:"expectedStatus"` // 100..599, обязателен
    TimeoutMs      int    `json:"timeoutMs"`      // default 5000
}

type TargetBalancerMonitorConfig struct {
    Enabled    bool                          `json:"enabled"`              // default false
    Schedule   *string                       `json:"schedule,omitempty"`
    Socks5     *BalancerMonitorSocks5Config  `json:"socks5,omitempty"`
    Request    *TargetMonitorRequestConfig   `json:"request,omitempty"`
    Retry      RetryConfig                   `json:"retry"`                // default {1,250}
    RemotePing TargetBalancerRemotePingConfig `json:"remotePing"`          // default {false,5000,false}
}
// ВНИМАНИЕ: порядок ключей в выводе print-config для balancerMonitor:
// enabled, schedule?, socks5?, request?, retry, remotePing (порядок схемы, load-config.ts:139-169).

type BalancerMonitorSocks5Config struct {
    Host string `json:"host"`
    Port int    `json:"port"` // 1..65535
}

type TargetBalancerRemotePingConfig struct {
    Enabled   bool    `json:"enabled"`        // default false
    URL       *string `json:"url,omitempty"`
    TimeoutMs int     `json:"timeoutMs"`      // default 5000
    ViaSocks  bool    `json:"viaSocks"`       // default false
}

type RuntimeConfig struct {
    Mode     string  `json:"mode"`               // run-once|daemon, default run-once
    Schedule *string `json:"schedule,omitempty"`
}

type LoggingConfig struct {
    Level  string `json:"level"`  // fatal|error|warn|info|debug|trace|silent, default info
    Format string `json:"format"` // json|pretty, default json
}

type ResourceConfig struct {
    Outbounds struct{ Enabled bool `json:"enabled"` } `json:"outbounds"` // default true
    Inbounds  struct{ Enabled bool `json:"enabled"` } `json:"inbounds"`  // default false
    Routing   struct{ Enabled bool `json:"enabled"` } `json:"routing"`   // default false
}

type StatusServerConfig struct {
    Enabled         bool                         `json:"enabled"`              // default true
    Listen          string                       `json:"listen"`               // default "127.0.0.1:9090" (может быть "")
    InstanceID      *string                      `json:"instanceId,omitempty"`
    RemoteInstances []RemoteStatusInstanceConfig `json:"remoteInstances"`      // default []
    RuntimeState    struct {
        Enabled        bool `json:"enabled"`        // default true
        IncludeRaw     bool `json:"includeRaw"`     // default false
        IncludeSecrets bool `json:"includeSecrets"` // default false
    } `json:"runtimeState"`
}

type RemoteStatusInstanceConfig struct {
    ID        string `json:"id"`
    APIURL    string `json:"apiUrl"`
    Schedule  string `json:"schedule"`
    TimeoutMs int    `json:"timeoutMs"` // default 5000
}

type LoadedConfig struct {
    ConfigPath string     // абсолютный путь
    Config     AppConfig
}
```

### 1.2 Парсинг/манифест (`src/types.ts:3-141`)

```go
type SourceLine struct {
    Line    int    // 1-based номер строки в исходном тексте (до фильтрации пустых)
    Raw     string // строка без '\n' и без '\r' перед '\n'; НЕ trimmed
    Trimmed string // JS String.prototype.trim()
}

type CountryInfo struct {
    Emoji  string `json:"emoji"`
    ISO2   string `json:"iso2"`
    NameEn string `json:"nameEn"`
    NameRu string `json:"nameRu"`
}

type SkipReasonCode string // empty_line | unsupported_scheme | invalid_uri | missing_required_field |
                           // unsupported_param | unsupported_value | unsupported_combo | duplicate_tag

type SkippedEntry struct {
    Line       int      `json:"line"`
    Raw        string   `json:"raw"`
    Label      *string  `json:"label,omitempty"`   // отсутствует для ранних отказов парсера; "" возможно
    ReasonCode string   `json:"reasonCode"`
    Reason     string   `json:"reason"`
    Details    []string `json:"details,omitempty"` // отсутствует если не задан
}

type ParseSuccess struct { // внутренний
    Line    int
    Raw     string
    Label   string
    UUID    string
    Address string
    Port    int
    Params  OrderedMap[string]string // ключи lowercased, порядок — как в JS-объекте (см. §5.6)
}

type NormalizedOutbound struct {
    Kind          string       `json:"kind"`     // "outbound"
    Protocol      string       `json:"protocol"` // "vless"
    Profile       string       `json:"profile"`  // tcp-tls | ws-tls | tcp-reality-vision
    Tag           string       `json:"tag"`
    Line          int          `json:"line"`
    Raw           string       `json:"raw"`
    Label         string       `json:"label"`
    Address       string       `json:"address"`
    Port          int          `json:"port"`
    UUID          string       `json:"uuid"`
    Encryption    string       `json:"encryption"` // всегда "none"
    Network       string       `json:"network"`    // tcp | ws
    Security      string       `json:"security"`   // tls | reality
    Flow          *string      `json:"flow,omitempty"`        // "" → отсутствует
    ALPN          []string     `json:"alpn"`                  // всегда есть, может быть []
    SNI           *string      `json:"sni,omitempty"`         // "" → отсутствует
    Host          *string      `json:"host,omitempty"`
    Path          *string      `json:"path,omitempty"`
    Fingerprint   *string      `json:"fingerprint,omitempty"`
    AllowInsecure *bool        `json:"allowInsecure,omitempty"`
    HeaderType    *string      `json:"headerType,omitempty"`  // ВНИМАНИЕ: "" сохраняется как "" (не omit)!
    PublicKey     *string      `json:"publicKey,omitempty"`
    ShortID       *string      `json:"shortId,omitempty"`
    Country       *CountryInfo `json:"country,omitempty"`
    City          *string      `json:"city,omitempty"`
    Query         OrderedMap   `json:"query"` // = Params (lowercased keys, все значения как есть)
}

type JsonOutbound struct {
    Tag      string `json:"tag"`
    Protocol string `json:"protocol"` // "vless"
    Settings struct {
        Vnext []struct {
            Address string `json:"address"`
            Port    int    `json:"port"`
            Users   []struct {
                ID         string  `json:"id"`
                Encryption string  `json:"encryption"` // "none"
                Flow       *string `json:"flow,omitempty"`
            } `json:"users"`
        } `json:"vnext"`
    } `json:"settings"`
    StreamSettings OrderedMap `json:"streamSettings"` // см. §11
}

type ManifestEntry struct {
    Kind         string             `json:"kind"`   // "outbound"
    Tag          string             `json:"tag"`
    Label        string             `json:"label"`
    Profile      string             `json:"profile"`
    Line         int                `json:"line"`
    Country      *CountryInfo       `json:"country,omitempty"`
    City         *string            `json:"city,omitempty"`
    Normalized   NormalizedOutbound `json:"normalized"`
    JsonOutbound JsonOutbound       `json:"jsonOutbound"`
}

type ManifestSummary struct {
    TotalLines           int `json:"totalLines"`
    Parsed               int `json:"parsed"`
    Skipped              int `json:"skipped"`
    Filtered             int `json:"filtered"`
    FilteredByCountry    int `json:"filteredByCountry"`
    FilteredByLabelRegex int `json:"filteredByLabelRegex"`
    UnsupportedScheme    int `json:"unsupportedScheme"`
    UnsupportedParam     int `json:"unsupportedParam"`
    UnsupportedValue     int `json:"unsupportedValue"`
    UnsupportedCombo     int `json:"unsupportedCombo"`
    InvalidURI           int `json:"invalidUri"`
    MissingRequiredField int `json:"missingRequiredField"`
    DuplicateTag         int `json:"duplicateTag"`
}

type OutboundManifest struct {
    Version     int             `json:"version"`     // 1
    GeneratedAt string          `json:"generatedAt"` // new Date().toISOString(): "2026-10-17T08:32:15.445Z" (UTC, ms, 'Z')
    SourceFile  string          `json:"sourceFile"`
    Entries     []ManifestEntry `json:"entries"`
    Skipped     []SkippedEntry  `json:"skipped"`
    Summary     ManifestSummary `json:"summary"`
}

type ParseLog struct { // src/logging/format.ts:3-15
    GeneratedAt string          `json:"generatedAt"` // == manifest.generatedAt
    Summary     ManifestSummary `json:"summary"`
    Skipped     []SkippedEntry  `json:"skipped"`
}
```

### 1.3 Загрузка/топология

```go
type LoadedInput struct { Source, Content string; Encoding string /* plain|base64 */ } // load-input.ts:8-12

type LoadedSubscription struct {   // generate-manifest-from-source.ts:8-16
    ID, Input, Source, Content, Encoding string
    Filters *SubscriptionFiltersConfig
    Target  SubscriptionTargetConfig
}
type FailedSubscriptionLoad struct { // :18-24
    ID, Input, Source, Error string  // Source == Input (конфиговое значение, не резолвнутый путь loader'а)
    Target SubscriptionTargetConfig
}

type PreparedTunnelOutbound struct { Tag string; Normalized NormalizedOutbound; JsonOutbound JsonOutbound }

type TunnelMapping struct {          // types.ts:339-352
    BaseTunnelID          string  // = entry.tag (без префикса)
    DisplayName           string  // = entry.label
    CountryISO2           *string
    BaseOutboundTag       string  // = entry.tag
    OutboundTagInitial    string  // prefix + tag
    OutboundTagCurrent    string  // изначально = OutboundTagInitial (мутируется runtime'ом при repair)
    InboundTag            string  // "in-" + tag
    RouteTag              string  // "route-" + tag
    Listen                string  // target.inboundSocks.listen
    Port                  int     // portRange.start + index
    OutboundInitial       PreparedTunnelOutbound // tag = prefix+tag
    OutboundWithoutPrefix PreparedTunnelOutbound // tag = tag
}
type TargetTopology struct { Tunnels []TunnelMapping }
```

---

## 2. Конфиг (`src/config/load-config.ts`)

### 2.1 Алгоритм `loadConfig(configPath)` (`load-config.ts:462-489`)

1. `resolvedPath = path.resolve(configPath)` — относительно **cwd** процесса. `configDir = dirname(resolvedPath)`.
2. `readFile(resolvedPath, 'utf8')`. Ошибка ФС пробрасывается как есть (НЕ `ConfigError`), напр.
   `ENOENT: no such file or directory, open '/abs/path/config.yml'`.
3. `interpolateEnv(raw)` — над **сырым текстом до YAML** (`:441-449`):
   - regex `/\$\{([A-Z0-9_]+)\}/gi` — имя из `[A-Za-z0-9_]+` (флаг `i` ⇒ строчные тоже), без default-синтаксиса, без экранирования.
   - Переменная не определена (`process.env[name] === undefined`) ⇒ `ConfigError("Missing environment variable \"NAME\" required by config.")`. Пустая строка — допустима.
   - Подстановка происходит **везде, включая YAML-комментарии**: `# ${NOPE}` в комментарии валит загрузку (проверено).
   - Значение вставляется сырым текстом и потом интерпретируется YAML'ом (значение `true` станет bool, `"a: b"` может сломать структуру).
   - Ошибка на первой отсутствующей переменной (в порядке появления).
4. `parseYaml(interpolated) ?? {}` — пакет `yaml@2.8.1`, дефолтные опции: YAML 1.2 core schema, `uniqueKeys: true`
   (дубликат ключа ⇒ ошибка `Map keys must be unique at line N, column M:` + сниппет), merge-ключи `<<` не включены,
   несколько документов ⇒ ошибка. Пустой файл/`null` ⇒ `{}`. Ошибки YAML пробрасываются как есть (не `ConfigError`).
   YAML 1.2: `yes/no/on/off` — **строки** (⇒ ошибка типа для bool-полей), `0x1F` — число.
5. Валидация схемой (zod 4). При ошибке:
   `ConfigError("Invalid config at <resolvedPath>: " + issues.map(i => (i.path.join('.') || '<root>') + ': ' + i.message).join('; '))` (`:470-474`).
6. Пост-обработка (`:476-488`): для каждой подписки `input`:
   - если `isRemoteInput` (`/^https?:\/\//i` или ровно `-`) — без изменений;
   - иначе если абсолютный — как есть, иначе `path.resolve(configDir, input)`.
   - `~` не раскрывается; `ftp://x/y` трактуется как относительный путь ⇒ `<configDir>/ftp:/x/y`.
   - Резолвятся и выключенные (`enabled:false`) подписки.
7. Возврат `{configPath: resolvedPath, config}`; порядок ключей верхнего уровня: `subscriptions, runtime, logging, resources, statusServer`.

### 2.2 Поля, типы, дефолты, правила

Обозначения: **strict** — неизвестные ключи ⇒ ошибка `Unrecognized key: "x"` / `Unrecognized keys: "a", "b"`
(путь = путь объекта). **strip** — неизвестные ключи молча выбрасываются (и не попадают в `print-config`).
`trim` — строка обрезается (JS trim) до проверки длины. Числа — строго JSON-number (строка `"5000"` ⇒ ошибка),
`int` — `Number.isInteger` (1.5 ⇒ `Invalid input: expected int, received number`; `1.0` из YAML = 1 ⇒ ок).

| Путь | Тип / правило | Default | Объект |
|---|---|---|---|
| (root) | object | — | strip |
| `subscriptions` | array, min 1 | **обязателен** | |
| `subscriptions[].id` | string trim min1 | обязателен | strict |
| `subscriptions[].input` | string trim min1 | обязателен | |
| `subscriptions[].enabled` | bool | `true` | |
| `subscriptions[].format` | enum `auto,plain,base64` | `auto` | |
| `subscriptions[].fetchTimeoutMs` | int > 0 | `5000` | |
| `subscriptions[].filters` | object, optional | — | strip |
| `…filters.countryAllowlist` | array of string; preprocess: строки → `trim().toUpperCase()`; затем `trim().length(2)`; optional | — | |
| `…filters.labelIncludeRegex` | string; preprocess trim, пустое ⇒ отсутствует | — | |
| `subscriptions[].target` | object | **обязателен** | strict |
| `target.address` | string trim min1 | обязателен | |
| `target.timeoutMs` | int > 0 | `5000` | |
| `target.fixedOutbounds/fixedInbounds/fixedRouting` | array of string trim min1 | `[]` | |
| `target.visionUdp443Override` | bool | `false` | |
| `target.inboundSocks` | object optional | — | strip |
| `…inboundSocks.listen` | string trim min1 | обязателен | |
| `…inboundSocks.portRange.start/end` | int > 0 (верхняя граница НЕ проверяется!) | обязательны | strip |
| `target.monitor` | object | `{enabled:false,maxParallel:10,retry:{attempts:1,delayMs:250}}` | strict |
| `monitor.enabled` | bool | `false` | |
| `monitor.schedule` | string trim, optional | — | |
| `monitor.maxParallel` | int > 0 | `10` | |
| `monitor.retry` | `{attempts:int>=1 (1), delayMs:int>=0 (250)}` | `{1,250}` | strict |
| `monitor.request` | object optional | — | strip |
| `…request.url` | string trim, WHATWG `new URL()` должен распарситься (любая схема: `mailto:a@b`, `ftp://x` проходят; значение НЕ нормализуется) | обязателен | |
| `…request.method` | enum `GET,HEAD,POST` | `GET` | |
| `…request.expectedStatus` | int 100..599 | обязателен | |
| `…request.timeoutMs` | int > 0 | `5000` | |
| `target.balancerMonitor` | object | `{enabled:false, retry:{1,250}, remotePing:{enabled:false,timeoutMs:5000,viaSocks:false}}` | strict |
| `balancerMonitor.enabled` | bool | `false` | |
| `balancerMonitor.schedule` | string trim optional | — | |
| `balancerMonitor.socks5` | `{host: string trim min1, port: int 1..65535}` optional | — | strip |
| `balancerMonitor.request` | как `monitor.request` | — | strip |
| `balancerMonitor.retry` | как `monitor.retry` | `{1,250}` | strict |
| `balancerMonitor.remotePing` | `{enabled:bool(false), url?: url, timeoutMs:int>0 (5000), viaSocks:bool(false)}` | см. выше | strict |
| `target.observatorySubjectSelectorPrefix` | string; preprocess trim, пустое ⇒ отсутствует | — | |
| `runtime` | object | **обязателен** (`runtime: {}` ок) | strip |
| `runtime.mode` | enum `run-once,daemon` | `run-once` | |
| `runtime.schedule` | string trim optional | — | |
| `logging` | object | **обязателен** | strip |
| `logging.level` | enum `fatal,error,warn,info,debug,trace,silent` | `info` | |
| `logging.format` | enum `json,pretty` | `json` | |
| `resources` | object | **обязателен** (`resources: {}` ок) | strip |
| `resources.outbounds/inbounds/routing` | `{enabled: bool}` | `true/false/false` | strip |
| `statusServer` | object | `{enabled:true, listen:"127.0.0.1:9090", remoteInstances:[], runtimeState:{true,false,false}}` | strip |
| `statusServer.enabled` | bool | `true` | |
| `statusServer.listen` | string trim (может стать `""`) | `127.0.0.1:9090` | |
| `statusServer.instanceId` | string; preprocess trim, пустое ⇒ отсутствует | — | |
| `statusServer.remoteInstances[]` | `{id: trim min1, apiUrl: url, schedule: trim min1, timeoutMs: int>0 (5000)}` | `[]` | strict |
| `statusServer.runtimeState` | `{enabled:true, includeRaw:false, includeSecrets:false}` | см. | strip |

Особенность дефолтов-объектов: если ключ `monitor` отсутствует — подставляется весь default-объект
(уже содержащий `retry`). Если `monitor: {}` задан — дефолты подставляются по полям. Итог одинаков.
Ключи optional-полей, которых не было во входе, в выводе отсутствуют.

### 2.3 Кросс-полевые правила (root superRefine, `load-config.ts:225-439`)

Выполняются в этом порядке; каждое добавляет issue (не прерывает), **кроме** отмеченного `return`:

1. Нет ни одной `enabled` подписки ⇒ path `subscriptions`: `At least one subscription must be enabled.`
2. Дубликаты `target.address` (точное сравнение после trim, **включая выключенные подписки**) ⇒ path `subscriptions`:
   `Duplicate target address "<addr>" is not allowed.` (по одному issue на адрес, в порядке первого появления).
3. Если `runtime.mode == daemon`:
   - нет `schedule` (отсутствует или `""` после trim) ⇒ path `runtime.schedule`: `runtime.schedule is required when runtime.mode is "daemon".` и **`return` — все последующие проверки (4–9) пропускаются**.
   - иначе cron-parse; ошибка ⇒ path `runtime.schedule`: текст ошибки cron-parser (напр. `Constraint error, got value 61 expected range 0-59`).
4. `inbounds.enabled && !outbounds.enabled` ⇒ `resources.inbounds`: `resources.inbounds.enabled requires resources.outbounds.enabled.`
5. `routing.enabled && (!outbounds.enabled || !inbounds.enabled)` ⇒ `resources.routing`: `resources.routing.enabled requires resources.outbounds.enabled and resources.inbounds.enabled.`
6. Для каждой подписки (включая выключенные), path всегда `subscriptions` (кроме указанного):
   1. `(inbounds.enabled || routing.enabled) && !inboundSocks` ⇒ `inboundSocks is required when resources.inbounds.enabled or resources.routing.enabled is true.`
   2. Если `monitor.enabled`:
      - нет schedule ⇒ `monitor.schedule is required when monitor.enabled is true.`; иначе ошибка cron ⇒ текст cron-parser;
      - нет request ⇒ `monitor.request is required when monitor.enabled is true.`
      - нет inboundSocks ⇒ `inboundSocks is required when monitor.enabled is true.`
      - не все три resources включены ⇒ path `resources`: `monitor.enabled requires resources.outbounds.enabled, resources.inbounds.enabled and resources.routing.enabled.`
   3. Если `balancerMonitor.enabled`:
      - нет schedule ⇒ `balancerMonitor.schedule is required when balancerMonitor.enabled is true.`; иначе cron error;
      - нет socks5 ⇒ `balancerMonitor.socks5 is required when balancerMonitor.enabled is true.`
      - нет request ⇒ `balancerMonitor.request is required when balancerMonitor.enabled is true.`
   4. `remotePing.enabled && !remotePing.url` ⇒ `balancerMonitor.remotePing.url is required when balancerMonitor.remotePing.enabled is true.`
   5. `remotePing.enabled && !balancerMonitor.enabled` ⇒ `balancerMonitor.remotePing.enabled requires balancerMonitor.enabled to be true.`
7. Если `statusServer.enabled`:
   - `listen == ""` ⇒ `statusServer.listen`: `statusServer.listen is required when statusServer.enabled is true.`
   - иначе `validateListenAddress` (`:45-55`): `sep = lastIndexOf(':')`; `sep <= 0 || sep == len-1` ⇒ `Listen address must be in "host:port" format.`;
     `port = Number(after)`; не целое или `<=0` или `>65535` ⇒ `Listen address port must be an integer between 1 and 65535.`
     **JS `Number()`**: `"0x50"`→80, `" 80"`→80, `"1e3"`→1000, `"80.0"`→80 — всё это проходит (проверено `localhost:0x50` ⇒ valid). Хост не валидируется.
   - Если `statusServer.enabled == false` — listen не проверяется вовсе (`listen: ''` валидно).
8. `remoteInstances`: дубликат id ⇒ path `statusServer.remoteInstances.<i>.id`: `Duplicate remote instance id "<id>" is not allowed.`
9. `remoteInstances[i].schedule` — cron-parse; ошибка ⇒ path `statusServer.remoteInstances.<i>.schedule`.

`filters` superRefine (`:72-94`), path `subscriptions.<i>.filters.<field>`:
- для каждого кода в `countryAllowlist`, не входящего в 45 поддерживаемых ISO2 (§7) ⇒ `Unsupported ISO2 country code "<CODE>".`
  (выдаётся и для кодов неверной длины — `"USA"`, `""` — дополнительно к length-ошибке, если superRefine вообще выполнился, см. 2.4);
- `labelIncludeRegex`: `parseRegexLiteral` (`:30-43`):
  - не начинается с `/` ⇒ `Regex literal must start with "/".`
  - `lastIndexOf('/') <= 0` (только ведущий слэш) ⇒ `Regex literal must end with "/flags".`
  - `new RegExp(pattern, flags)` бросает ⇒ текст V8: `Invalid flags supplied to RegExp constructor 'x'`, `Invalid regular expression: /(/i: Unterminated group`.
- `countryAllowlist: []` валиден и фильтр не применяется.

`inboundSocks` superRefine (`:112-120`): `end < start` ⇒ path `subscriptions.<i>.target.inboundSocks.portRange.end`: `inboundSocks.portRange.end must be greater than or equal to start.`

### 2.4 Семантика zod 4, важная для точного набора сообщений

- Issues выдаются в порядке: сначала структурные (по ходу обхода схемы, ключи в порядке схемы, элементы массивов по индексу), затем refine'ы (внутренние раньше внешних, т.к. выполняются при выходе из узла).
- **Refine/superRefine узла НЕ выполняется, если в его поддереве есть «прерывающий» issue**: `invalid_type` (неверный тип/отсутствует обязательное), `invalid_value` (enum), `unrecognized_keys`. «Продолжаемые» issue (`too_small`, `too_big`, `invalid_format` — URL) refine не блокируют. Проверено:
  - `format: b64` + дубль адресов ⇒ только enum-ошибка;
  - `id: ''` + дубль адресов ⇒ обе ошибки;
  - `countryAllowlist: ['xk','USA']` ⇒ length-ошибка **и** `Unsupported ... "XK"`, `"USA"`;
  - `countryAllowlist: [..., 5]` ⇒ refine фильтров не выполняется.
- Формат стандартных сообщений zod 4 (воспроизводить дословно, если нужна полная совместимость):
  - `Invalid input: expected string, received number` / `expected object, received undefined` / `expected array, received undefined` / `expected boolean, received string` / `expected int, received number`
  - `Invalid option: expected one of "auto"|"plain"|"base64"`
  - `Too small: expected string to have >=1 characters`, `Too big: expected string to have <=2 characters`, `Too small: expected string to have >=2 characters`
  - `Too small: expected array to have >=1 items`, `Too small: expected number to be >=100`, `Too small: expected number to be >=1`, `Too big: expected number to be <=599` (по аналогии), `Too small: expected number to be >0` (для `positive()`)
  - `Invalid URL`
  - `Unrecognized key: "targets"` / `Unrecognized keys: "foo", "bar"`
- Пустой YAML ⇒ `Invalid config at /abs/c.yml: subscriptions: Invalid input: expected array, received undefined; runtime: Invalid input: expected object, received undefined; logging: Invalid input: expected object, received undefined; resources: Invalid input: expected object, received undefined`.

Рекомендация для Go: декодировать YAML в `any` (yaml.v3 `Node`/`map[string]any`), а затем писать ручной валидатор, имитирующий эти правила (типы JS: number/string/bool/object/array). **Не** декодировать прямо в typed struct: yaml.v3 принимает `yes/on` для typed bool и молча игнорирует неизвестные ключи / приводит типы иначе.

### 2.5 Cron

Валидация — `CronExpressionParser.parse(expr)` из `cron-parser@5.4.0` без опций. Принимается очень широкий синтаксис:
5 или 6 полей (секунды опциональны в начале), алиасы `@daily/@hourly/...`, `L`, `#`, `H` (hash), имена `MON-FRI`;
даже `* * * *` (4 поля) и одиночные токены парсятся. Отказы: `* * * * * * *` ⇒ `Invalid cron expression, too many fields`;
`@every 5m` ⇒ `Validation error, cannot resolve alias "eve"`; `x` ⇒ `Constraint error, got value 0 expected range 1-12`.
Порт должен использовать совместимый парсер (детали семантики расписаний — в документе про runtime).

### 2.6 Пример `print-config` для минимального конфига

Вход:
```yaml
subscriptions:
  - id: s1
    input: ./sub.txt
    target:
      address: 127.0.0.1:8080
runtime: {}
logging: {}
resources: {}
```
Выход (stdout, `JSON.stringify(config, null, 2) + "\n"`):
```json
{
  "subscriptions": [
    {
      "id": "s1",
      "input": "/abs/dir/sub.txt",
      "enabled": true,
      "format": "auto",
      "fetchTimeoutMs": 5000,
      "target": {
        "address": "127.0.0.1:8080",
        "timeoutMs": 5000,
        "fixedOutbounds": [],
        "fixedInbounds": [],
        "fixedRouting": [],
        "visionUdp443Override": false,
        "monitor": { "enabled": false, "maxParallel": 10, "retry": { "attempts": 1, "delayMs": 250 } },
        "balancerMonitor": {
          "enabled": false,
          "retry": { "attempts": 1, "delayMs": 250 },
          "remotePing": { "enabled": false, "timeoutMs": 5000, "viaSocks": false }
        }
      }
    }
  ],
  "runtime": { "mode": "run-once" },
  "logging": { "level": "info", "format": "json" },
  "resources": { "outbounds": { "enabled": true }, "inbounds": { "enabled": false }, "routing": { "enabled": false } },
  "statusServer": {
    "enabled": true,
    "listen": "127.0.0.1:9090",
    "remoteInstances": [],
    "runtimeState": { "enabled": true, "includeRaw": false, "includeSecrets": false }
  }
}
```
(реальный вывод — с отступом 2 пробела на каждом уровне, здесь сжат). Порядок ключей внутри подписки:
`id, input, enabled, format, fetchTimeoutMs, filters?, target`; в target:
`address, timeoutMs, fixedOutbounds, fixedInbounds, fixedRouting, visionUdp443Override, inboundSocks?, monitor, balancerMonitor, observatorySubjectSelectorPrefix?`;
в monitor: `enabled, schedule?, maxParallel, retry, request?`. print-config печатает секреты (подставленные env) как есть.

---

## 3. Загрузка входа (`src/input/load-input.ts`)

### 3.1 Диспетчеризация (`:111-140`)

| input | ветка | `source` в результате |
|---|---|---|
| ровно `-` | stdin | `"stdin"` |
| `/^https?:\/\//i` | HTTP(S) | исходная строка URL (даже после редиректа) |
| иное | файл, `path.resolve(input)` (относительно **cwd**) | абсолютный путь |

Каждая ветка: `raw = ensureNonEmpty(read(), src)` → `applyInputFormat(raw, format)` → `content = ensureNonEmpty(resolved.content, src)`.
`ensureNonEmptyContent` (`:23-28`): если `content.trim() == ""` ⇒ `InputError(code 'EMPTY_INPUT', "Input source <src> returned empty content.")`,
где `<src>` = `stdin` / URL / абсолютный путь. Проверка делается **дважды** (до и после декодирования).

- stdin (`:55-61`): читается целиком до EOF, байты конкатенируются, UTF-8 декод (невалидные → U+FFFD). BOM **не** снимается.
- файл: `readFile(path,'utf8')` — BOM **не** снимается (останется в `raw` первой строки; см. §5.1). Ошибка ФС ⇒ сообщение Node
  `ENOENT: no such file or directory, open '/abs/path'`. `fetchTimeoutMs` на файлы не влияет.
- HTTP (`:30-53`):
  - `fetch(input, {redirect:'follow', signal})`; **никаких явных заголовков** (тест проверяет `init.headers === undefined`).
    Фактически undici шлёт: `accept: */*`, `accept-language: *`, `sec-fetch-mode: cors`, `user-agent: node`,
    `accept-encoding: gzip, deflate` (на Node 24 также `br`), `connection: keep-alive`. Метод GET.
    Рекомендация Go: выставить `User-Agent: node` и `Accept: */*`, т.к. провайдеры подписок часто ветвятся по UA.
  - Таймаут `fetchTimeoutMs` (default 5000) — **на весь запрос, включая чтение тела** (таймер снимается в `finally` после `text()`).
  - `!response.ok` (статус вне 200..299) ⇒ `Error("Unable to download input from <url>: <status> <statusText>")`
    (statusText — reason-phrase сервера, напр. `404 Nope Custom`; для HTTP/2 пустой ⇒ `"...: 404 "`).
  - Если сработал abort ⇒ `Error("Input source <url> timed out after <ms>ms.")` (перекрывает любую другую ошибку при abort).
  - Сетевая ошибка ⇒ `TypeError("fetch failed")` — сообщение дословно `fetch failed`.
  - `response.text()` — всегда UTF-8, **снимает ведущий BOM**, charset из Content-Type игнорируется (проверено с `windows-1251`).

### 3.2 Декодирование (`applyInputFormat`, `:94-109`)

`normalizeBase64(v)` (`:67-69`): удалить все `\s` (JS-класс: включает NBSP, U+FEFF, U+2028 и др.), `trim`, `-`→`+`, `_`→`/`.

- `plain`: `content = raw`, `encoding = "plain"`.
- `base64`: `content = NodeBase64Decode(normalizeBase64(raw)).utf8()`, `encoding = "base64"` — **без** проверки алфавита и без проверки на `://`; мусор декодируется в мусор (не ошибка).
- `auto`: `content = decodeSubscriptionContent(raw)`; `encoding = content === raw ? "plain" : "base64"`.

`decodeSubscriptionContent(input)` (`:71-92`, экспортируется):
1. `t = input.trim()`; если `t == ""` или `t` содержит `://` ⇒ вернуть `input` (исходный, не trimmed).
2. `n = normalizeBase64(t)`; если `n == ""` или содержит символ вне `[A-Za-z0-9+/=]` ⇒ вернуть `input`.
3. `d = NodeBase64Decode(n).utf8()`; если `d` содержит `://` ⇒ вернуть `d`, иначе `input`.

**Node base64 decoder** (нужно эмулировать в Go, `encoding/base64` строже):
- символы вне алфавита (`+/` и URL-safe `-_` тоже принимаются) пропускаются;
- первый `=` **завершает** декодирование (`"dmxlc3M=Ly8"` → `"vless"`);
- padding необязателен; хвост из 1 символа отбрасывается, из 2–3 символов даёт 1–2 байта;
- результат → UTF-8 с заменой невалидных последовательностей на U+FFFD.

Проверенные случаи `auto`: стандартный, URL-safe, без padding, с переносами строк каждые 20 символов, с ведущими/хвостовыми пробелами — декодируются; `=` в середине, `!` в строке, `hello world`, base64 текста без `://` — возвращаются без изменений.

---

## 4. Сканирование строк (`src/subscription/scan-lines.ts:3-11`)

```
lines = text.split(/\r?\n/)                       // одиночный '\r' НЕ разделитель
       .map((raw, i) => {line: i+1, raw, trimmed: JSTrim(raw)})
       .filter(l => l.trimmed != "")
```
- Номер строки — индекс в тексте **до** фильтрации пустых (пустые строки увеличивают счётчик).
- `totalLines` = число непустых строк.
- `JSTrim` = набор ECMAScript WhiteSpace+LineTerminator: `\t \n \v \f \r`, пробел, U+00A0, U+1680, U+2000–U+200A, U+2028, U+2029, U+202F, U+205F, U+3000, **U+FEFF**. **Не** включает U+0085 (NEL). Go `strings.TrimSpace` отличается в обе стороны (режет NEL, не режет BOM) — нужна своя функция.

---

## 5. Парсинг строки (`src/subscription/parse-subscription-line.ts`)

### 5.1 Порядок проверок (`:44-108`)

| # | Условие | reasonCode | reason (дословно) | label | details |
|---|---|---|---|---|---|
| 1 | `trimmed == ""` (недостижимо после scanLines) | `empty_line` | `The line is empty.` | — | — |
| 2 | `trimmed` не содержит `://` | `unsupported_scheme` | `The line does not contain a supported URI scheme.` | — | — |
| 3 | `new URL(trimmed)` бросает | `invalid_uri` | `The line is not a valid URI.` | — | — |
| 4 | `url.protocol !== "vless:"` (protocol уже lowercased парсером ⇒ `VLESS://` принимается) | `unsupported_scheme` | `Unsupported scheme "<protocol>".` напр. `Unsupported scheme "trojan:".` | — | — |
| 5 | label = `decodeURIComponent(fragment)`; uuid = `decodeURIComponent(username)` — **могут бросить URIError** (см. ниже) | — | — | — | — |
| 6 | uuid не матчит `/^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i` | `missing_required_field` | `The VLESS URI does not contain a valid UUID.` | label | — |
| 7 | `hostname == ""` или порт пуст/0/вне 1..65535 | `missing_required_field` | `The VLESS URI does not contain a valid address and port.` | label | — |
| 8 | есть параметры вне списка | `unsupported_param` | `Unsupported VLESS parameter(s): a, b.` | label | `["a","b"]` |

Важно:
- Проверка №2 — по `trimmed` целиком: `foo bar ://` проходит №2 и падает на №3 (`invalid_uri`).
- `vless:uuid@host:443` (без `//`) ⇒ `unsupported_scheme` («does not contain a supported URI scheme»).
- UUID: версия 1–5, вариант 8/9/a/b, регистр любой; nil-UUID `00000000-…` отклоняется; uuid сохраняется **как есть** (регистр не меняется).
- В label/username `decodeURIComponent` бросает `URIError: URI malformed` при `%` без двух hex (`#100%`) или при невалидном UTF-8 (`#a%FFb`). **Исключение не ловится** ⇒ `buildManifest` падает целиком: в `generate` ⇒ stderr `URI malformed`, exit 1; в sync ⇒ ошибка построения плана ⇒ `manifest_validation_failed` с `error: "URI malformed"` и target failed. Label декодируется **до** проверки UUID.
- Пароль в userinfo (`uuid:pass@`) игнорируется. `a@b@h` ⇒ username `a%40b` ⇒ после decode `a@b` ⇒ invalid UUID.
- Path (`@host:443/some/path?…`) игнорируется.

Допустимые параметры (ключи сравниваются после `toLowerCase()`): `type, path, security, alpn, encryption, fp, sni, flow, host, allowinsecure, headertype, pbk, sid` (`:4-18`).

### 5.2 Семантика WHATWG URL (схема `vless` — «non-special»), которую нужно воспроизвести

Проверено на Node:
- Перед парсингом из строки удаляются **все** ASCII tab/LF/CR (в любом месте) — таб внутри label исчезает (`a\tb` → `ab`), но `raw` сохраняет таб.
- Ведущие/хвостовые C0-control и пробел обрезаются (уже trimmed).
- Host — «opaque host»: **регистр сохраняется** (`Example.COM`), percent-escape сохраняется буквально (`ho%41st` → address `ho%41st`), IPv4 не нормализуется (`0x7F.1` остаётся). Запрещённые символы (пробел, `^`, `|`, `<`, `>`, `[`/`]` вне IPv6, `#`, `/`, `?`, `@`, `\`) ⇒ throw ⇒ `invalid_uri`.
- IPv6 в скобках нормализуется и сериализуется сжато с lowercase: `[2001:0DB8:0:0::1]` → `[2001:db8::1]`; `[::ffff:1.2.3.4]` → `[::ffff:102:304]`. `address` содержит **скобки**.
- Порт: только цифры; ведущие нули допустимы (`0443` → 443); `>65535`, `443x`, `+443` ⇒ throw ⇒ `invalid_uri`; пустой порт (`host:`/без `:`) ⇒ `port=""` ⇒ №7; `0` ⇒ №7. Нет default-порта.
- Пустой host с портом (`@:443`) ⇒ throw ⇒ `invalid_uri`.
- Fragment = всё после **первого** `#` (последующие `#` и `?` входят в label: `#🇩🇪 Берлин#2` → label `🇩🇪 Берлин#2`; `#f?x=1` → label `f?x=1`). Fragment percent-encode'ится парсером (пробел, `"`, `<`, `>`, `` ` ``, не-ASCII), затем `decodeURIComponent` возвращает исходные символы. Литеральные `%20` в исходной строке декодируются в пробел. Итого: label ≈ strict percent-decode сырого фрагмента (без удаления TAB/LF — они уже удалены), с ошибкой на кривых `%`/невалидном UTF-8.
- Нет `#` или пустой `#` ⇒ label `""`.
- `?` внутри фрагмента не является query.

### 5.3 Query (`url.searchParams`, `application/x-www-form-urlencoded`)

- Разделитель только `&` (`;` — обычный символ: `path=/a;x` → `/a;x`). Go `url.ParseQuery` отбрасывает пары с `;` — **нельзя использовать**.
- `+` → пробел; `%XX` декодируется; невалидная escape (`%zz`) остаётся буквально (не ошибка). Невалидный UTF-8 после декода → U+FFFD.
- Пустые сегменты (`&&`) пропускаются; `=x` даёт ключ `""`; `k` без `=` даёт значение `""`.
- Ключ lowercased (`toLowerCase`, Unicode), значение — как есть. Дубликаты: **последний выигрывает** (`sni=a&sni=b` → `b`).
- Ключи хранятся в JS-объекте ⇒ **порядок перечисления**: сначала ключи-«array index» (канонические целые 0..2³²−2) по возрастанию, затем остальные в порядке первого появления. Пример: `type=tcp&security=tls&1=2&zz=3&&=x` ⇒ details `["1","zz",""]`, reason `Unsupported VLESS parameter(s): 1, zz, .`. Этот же порядок — порядок ключей `query` в JSON.

---

## 6. Нормализация (`src/normalize/normalize-vless.ts`)

### 6.1 Определение профиля (`:53-68`)

```
network  = lower(params.type ?? "tcp")      // "" (type=) => "" ⇒ combo-ошибка
security = lower(params.security ?? "")
flow     = params.flow ?? ""                // регистр ВАЖЕН
tcp+tls                       → "tcp-tls"   (flow любой, включая xtls-rprx-vision — сохраняется!)
ws+tls                        → "ws-tls"
tcp+reality+flow=="xtls-rprx-vision" → "tcp-reality-vision"
иначе                         → unsupported_combo
```

### 6.2 Порядок отказов (`:82-157`), все SkippedEntry содержат `label` (всегда, возможно `""`)

| # | Условие | reasonCode | reason | details |
|---|---|---|---|---|
| 1 | профиль не определён | `unsupported_combo` | `Unsupported VLESS type/security/flow combination.` | `["type=<raw type ?? 'tcp'>", "security=<raw security ?? ''>", "flow=<raw flow ?? ''>"]` (значения **не** lowercased) |
| 2 | `encryption` задан, не `""` и не `"none"` (регистр важен: `None` ⇒ отказ) | `unsupported_value` | `Unsupported VLESS encryption value.` | `["encryption=<v>"]` |
| 3 | ws-tls и `path` отсутствует или `""` | `missing_required_field` | `WebSocket VLESS requires a path.` | — |
| 4 | reality: первый отсутствующий/пустой из `sni, fp, pbk, sid` (в этом порядке) | `missing_required_field` | `REALITY VLESS requires parameter "<name>".` | — |
| 5 | `allowinsecure` задан, но не `1/0/true/false` (true/false регистронезависимо; `""` тоже отказ) | `unsupported_value` | `allowInsecure must be 0, 1, true, or false.` | `["allowInsecure=<v>"]` |
| 6 | `headertype` задан, не `""`, не `"none"` (регистр важен) | `unsupported_value` | `Only headerType=none is supported in v1.` | `["headerType=<v>"]` |

Параметры, которые принимаются, но не используются профилем (pbk/sid для tls, path/host для tcp), просто лежат в `query`/полях normalized и игнорируются билдером.

### 6.3 Поля результата (`:162-189`)

```
kind="outbound", protocol="vless", profile, tag=buildTag(...), line, raw, label, address, port, uuid,
encryption="none",
network  = profile=="ws-tls" ? "ws" : "tcp",
security = profile=="tcp-reality-vision" ? "reality" : "tls",
flow        = params.flow || undefined      // "" ⇒ отсутствует
alpn        = (params.alpn ?? "").split(",").map(trim).filter(nonEmpty)   // "h2, http/1.1,," ⇒ ["h2","http/1.1"]
sni         = params.sni  || undefined
host        = params.host || undefined
path        = params.path || undefined
fingerprint = params.fp   || undefined
allowInsecure = "1"/"true"(ci) ⇒ true; "0"/"false"(ci) ⇒ false; отсутствует ⇒ undefined
headerType  = params.headertype ?? undefined   // "" СОХРАНЯЕТСЯ как "" (в JSON "headerType": "")
publicKey   = params.pbk || undefined
shortId     = params.sid || undefined
country     = extractCountryInfo(label)
city        = inferCity(label)
query       = params (lowercased keys)
```

### 6.4 Тег (`buildTag`, `:70-80`)

```
tag = "vless-" + country + "-" + city + "-" + profile + "-" + hash
country = lower(extractCountryInfo(label).iso2) ?? "xx"
ic      = inferCity(label)
city    = slugify(ic ?? "") || (ic ? sha8(ic) : "") || slugify(address) || "endpoint"
hash    = sha8(raw)                      // raw = НЕтримленная строка (см. §4)
sha8(s) = hex(sha256(utf8(s)))[0:8]      // lowercase hex
```

`inferCity(label)` (`:43-51`):
1. удалить с начала максимальный префикс из символов `\p{Regional_Indicator}` (U+1F1E6–U+1F1FF) и JS-`\s`;
2. JS-trim; пусто ⇒ undefined;
3. взять часть до первой `,`, trim;
4. если пусто или содержит `extra` (регистронезависимо, где угодно в подстроке) ⇒ undefined, иначе city.

`slugify(v)` (`:29-37`): `NFKD` → удалить U+0300–U+036F → `toLowerCase()` → `[^a-z0-9]+` → `-` → обрезать ведущие/хвостовые `-` → **затем** взять первые 32 символа (хвостовой `-` после обрезки длины **может остаться**).

Примеры (проверены):

| label | address | tag | city |
|---|---|---|---|
| `🇦🇹 Вена, Австрия` | example.com | `vless-at-3ed0e2a1-tcp-tls-<h>` (`3ed0e2a1` = sha8("Вена")) | `Вена` |
| `🇩🇪 Германия, Extra` | | `vless-de-c2574453-…` | `Германия` |
| `🇩🇪` | example.com | `vless-de-example-com-…` | — |
| `🇩🇪 Extra Whitelist` | example.com | `vless-de-example-com-…` | — |
| `Café Zürich` | | `vless-xx-cafe-zurich-…` | `Café Zürich` |
| `  🇩🇪Berlin` (пробелы до флага) | | `vless-xx-berlin-…` (страна не найдена, город найден) | `Berlin` |
| `🇩🇪🇦🇹 Wien` | | `vless-de-wien-…` | `Wien` |
| `🇽🇰 Pristina` (флаг вне карты) | | `vless-xx-pristina-…` | `Pristina` |
| `abcdefghijklmnopqrstuvwxyz0123-56789` | | `vless-xx-abcdefghijklmnopqrstuvwxyz0123-5-…` (32 символа) | |
| `` (пусто) | `[2001:db8::1]` | `vless-xx-2001-db8-1-…` | — |

Один и тот же URI с разным окружающим whitespace даёт **разные теги** (хеш от `raw`).

---

## 7. Карта флагов (`src/flag-country-map/index.ts`)

- 45 регионов (`:3-8`): `AE AR AT AU BE BG BR CA CH CO CZ DE DK ES FI FR GB GR HK HR HU IE IL IN IT JP KR KZ LT MX MY NL NO PE PL PT RO RU SE SG SK TR UA US ZA`.
- emoji = два Regional Indicator: `U+1F1E6 + (c - 'A')` для каждой буквы (`:60-64`).
- `nameEn` = `Intl.DisplayNames(['en'],{type:'region'})` — зависит от ICU Node; **в Go захардкодить** (значения Node 22):
  AE United Arab Emirates, AR Argentina, AT Austria, AU Australia, BE Belgium, BG Bulgaria, BR Brazil, CA Canada,
  CH Switzerland, CO Colombia, CZ Czechia, DE Germany, DK Denmark, ES Spain, FI Finland, FR France, GB United Kingdom,
  GR Greece, HK **Hong Kong SAR China**, HR Croatia, HU Hungary, IE Ireland, IL Israel, IN India, IT Italy, JP Japan,
  KR South Korea, KZ Kazakhstan, LT Lithuania, MX Mexico, MY Malaysia, NL Netherlands, NO Norway, PE Peru, PL Poland,
  PT Portugal, RO Romania, RU Russia, SE Sweden, SG Singapore, SK Slovakia, TR **Türkiye**, UA Ukraine, US United States, ZA South Africa.
  (Перепроверить на Node 24.14 перед фиксацией golden-файлов.)
- `nameRu` — таблица `:10-56` (ОАЭ, Аргентина, Австрия, Австралия, Бельгия, Болгария, Бразилия, Канада, Швейцария, Колумбия, Чехия, Германия, Дания, Испания, Финляндия, Франция, Великобритания, Греция, Гонконг, Хорватия, Венгрия, Ирландия, Израиль, Индия, Италия, Япония, Южная Корея, Казахстан, Литва, Мексика, Малайзия, Нидерланды, Норвегия, Перу, Польша, Португалия, Румыния, Россия, Швеция, Сингапур, Словакия, Турция, Украина, США, ЮАР).
- `extractCountryInfo(label)` (`:81-84`): взять **первые две code point** label (`[...label].slice(0,2)`), конкатенировать, искать в карте. Любой символ перед флагом (пробел, BOM) ⇒ не найдено. Порядок ключей объекта: `emoji, iso2, nameEn, nameRu`.
- Тот же набор ISO2 используется для валидации `countryAllowlist` в конфиге.

---

## 8. Построение манифеста (`src/manifest.ts:50-109`)

```
lines := scanLines(text); summary := zero; summary.totalLines = len(lines)
seen := set()
for line in lines:
    p := parseSubscriptionLine(line)        // может panic/throw URIError (§5.1)
    if fail: skipped.append(p.skipped); increment(summary, p.skipped); continue
    n := normalizeVless(p)
    if skip: skipped.append(n); increment; continue
    if n.tag in seen:
        skipped.append({line, raw, label, reasonCode:"duplicate_tag",
                        reason: `Duplicate generated outbound tag "<tag>".`}); increment; continue
    seen.add(n.tag)
    entries.append({kind, tag, label, profile, line, country, city, normalized: n, jsonOutbound: buildOutboundJson(n)})
    summary.parsed++
return {version:1, generatedAt: nowISO(), sourceFile, entries, skipped, summary}
```

`increment` (`:25-48`): `skipped++` и счётчик по коду: `unsupported_scheme→unsupportedScheme`, `unsupported_param→unsupportedParam`,
`unsupported_value→unsupportedValue`, `unsupported_combo→unsupportedCombo`, `invalid_uri→invalidUri`,
`missing_required_field→missingRequiredField`, `duplicate_tag→duplicateTag`. `empty_line` увеличивает только `skipped`.

Дубликат возможен только при идентичном `raw` с идентичными label/address/profile (хеш от raw) — т.е. фактически одна и та же строка дважды. Первое вхождение побеждает. Порядок entries = порядок строк (сортировки нет).

---

## 9. Фильтры (`src/runtime/filter-manifest.ts`)

`applyManifestFilters(manifest, filters)` (`:54-89`):
1. Если `!(countryAllowlist?.length) && !labelIncludeRegex` ⇒ вернуть **тот же** манифест (summary.filtered* остаются 0).
2. Если `countryAllowlist` непуст: оставить entries, у которых `country?.iso2` ∈ allowlist; без страны ⇒ отфильтровано. `filteredByCountry` = число отброшенных.
3. Если `labelIncludeRegex`: `parseRegexLiteral` (без валидации, `:3-8`: pattern = между первым символом и последним `/`, flags = после) ⇒ для каждого entry: `regex.lastIndex = 0; regex.test(entry.label)`; не совпало ⇒ отброшено. Применяется к **остаткам** после шага 2.
4. Результат: копия манифеста с новыми entries и summary: `parsed = len(entries)`, `filtered = byCountry + byRegex`, `filteredByCountry`, `filteredByLabelRegex`. `skipped`, `totalLines`, причины — без изменений; отфильтрованные **не** попадают в `skipped[]`.

Regex — **JavaScript (ECMAScript)**: README рекомендует lookahead `(?!...)`, `\b`, флаги `i`, `u` (`\p{RI}`), — RE2 (Go `regexp`) это не поддерживает. Использовать `github.com/dlclark/regexp2` с `regexp2.ECMAScript` (+ `IgnoreCase` для `i`, `Multiline` для `m`, `Singleline`/dotall для `s`, Unicode-поведение для `u`) и вручную валидировать допустимые флаги `d g i m s u v y` (без повторов; `u`+`v` вместе — ошибка).
Семантика флагов при `lastIndex=0` перед каждым тестом: `g` — без эффекта; `y` (sticky) — совпадение обязано начинаться с позиции 0 (проверено: `/Extra/y` пропускает только label, начинающийся с `Extra`).

---

## 10. Подписки → манифест (`src/runtime/generate-manifest-from-source.ts`)

`loadSubscriptions(sources)` (`:61-93`):
- брать только `enabled`, сортировать `compare(a,b) = a.id.localeCompare(b.id) || a.input.localeCompare(b.input)`;
- **последовательно** (не параллельно) `loadInputSource(input, {format, fetchTimeoutMs})`;
- успех ⇒ `loaded[]` (`source` = результат loader'а); исключение ⇒ `failed[]` с `error = err.message` (или `String(err)`), `source = input` из конфига.

`localeCompare` = ICU-коллация (локаль по умолчанию `en-US` ≈ root): регистронезависимо на первичном уровне, при равенстве строчная раньше заглавной (`b < B`), `é` после `e`, `_` раньше `-` (`a_b < a- < a-c < a0 < ab < aB`), числа не натурально (`src10 < src9`). Для id подписок использовать `golang.org/x/text/collate` (`language.English`/`Und`), а не байтовое сравнение.

`validateManifestOrThrow(manifest, sourceId)` (`:50-55`): если `summary.parsed == 0 || len(entries) == 0` ⇒
`ManifestValidationError` (code `NO_VALID_ENTRIES`) с сообщением (одна строка, разделитель — пробел, `:31-48`):
```
Subscription source "<sourceId>" produced no valid outbound entries. parsed=0 skipped=1 filtered=0 filteredByCountry=0 filteredByLabelRegex=0 unsupportedScheme=1 unsupportedParam=0 unsupportedValue=0 unsupportedCombo=0 invalidUri=0 missingRequiredField=0 duplicateTag=0
```

`generateManifestFromSubscription(sub)` (`:95-108`): `sourceId = sub.id || "subscription"`;
`manifest = validate(applyManifestFilters(buildManifest(sub.content.trim(), sourceId), sub.filters), sourceId)`;
возвращает `{manifest, parseLog: buildParseLog(manifest), sourceId}`. Т.е. в sync-пути `sourceFile` манифеста = **id подписки**, а не путь/URL.
Отфильтровавшие всё фильтры ⇒ тот же `NO_VALID_ENTRIES` (с `filtered>0`).

Примечание: в sync эта функция вызывается **отдельно для каждого включённого applicator'а** (outbound/inbound/routing — `outbound-applicator.ts:83`, `inbound-applicator.ts:42`, `routing-applicator.ts:42`), т.е. манифест строится до 3 раз (разные `generatedAt`). В Go можно строить один раз — результат детерминирован, кроме `generatedAt`.

---

## 11. JSON outbound (`src/builders/build-outbound-json.ts:3-68`)

```jsonc
{
  "tag": "<tag>",
  "protocol": "vless",
  "settings": { "vnext": [ { "address": "<address>", "port": <port>,
      "users": [ { "id": "<uuid>", "encryption": "none" /*, "flow": "<flow>" если flow непуст */ } ] } ] },
  "streamSettings": {
    "network": "tcp|ws",
    "security": "tls|reality",
    // security==tls:
    "tlsSettings": { /* только присутствующие, в этом порядке: */ "serverName": sni, "alpn": [..] /*если len>0*/,
                     "fingerprint": fp, "allowInsecure": bool /*если определён*/ },   // может быть {} 
    // security==reality:
    "realitySettings": { "serverName": sni, "fingerprint": fp, "publicKey": pbk, "shortId": sid },
    // network==ws:
    "wsSettings": { "path": path ?? "/", "headers": { "Host": host } /*только если host*/ }
  }
}
```
- Для `tcp-tls` с `flow=xtls-rprx-vision` поле `users[0].flow` **выводится** (профиль tcp-tls не чистит flow).
- Для tcp параметры `path`/`host` игнорируются; `alpn` для reality игнорируется.
- `tlsSettings` присутствует всегда при tls (даже пустой `{}`).

Пример ws+tls и reality — см. §14.2.

---

## 12. Target override и топология

### 12.1 `overrideOutboundForTarget(entry, target, tag = entry.tag)` (`src/targets/override-outbound-for-target.ts:23-40`)

```
effectiveFlow = (target.visionUdp443Override && normalized.protocol=="vless" &&
                 normalized.security=="reality" && normalized.flow=="xtls-rprx-vision")
                ? "xtls-rprx-vision-udp443" : normalized.flow
normalized' = {...normalized, tag, flow: effectiveFlow}     // порядок ключей сохраняется
return {...entry, tag, normalized: normalized', jsonOutbound: buildOutboundJson(normalized')}
```
tcp-tls с `flow=xtls-rprx-vision` **не** переписывается (только reality).

### 12.2 `buildTargetTopology(manifest, target)` (`src/topology/build-tunnel-topology.ts:37-78`)

1. Нет `target.inboundSocks` ⇒ `{tunnels: []}`.
2. `entries = sort(manifest.entries, by tag.localeCompare, then line asc, then normalized.raw.localeCompare)`. Теги состоят только из `[a-z0-9-]`, поэтому ICU-порядок совпадает с байтовым (`-` < цифры < буквы) — в Go допустим `strings.Compare`/`sort.SliceStable` по тегу. Теги в манифесте уникальны ⇒ тай-брейки фактически не используются.
3. `portCount = end - start + 1`; если `len(entries) > portCount` ⇒ `Error("Target \"<address>\" inboundSocks.portRange can allocate only <portCount> ports, but <n> tunnels were generated.")`.
4. Для i-го entry:
```
baseTunnelId       = tag
outboundTagInitial = prefix ? prefix + tag : tag          // prefix = observatorySubjectSelectorPrefix
outboundInitial    = override(entry, target, outboundTagInitial)  // {tag, normalized, jsonOutbound}
outboundWithoutPrefix = override(entry, target, tag)
displayName = label; countryIso2 = country?.iso2; baseOutboundTag = tag
outboundTagCurrent = outboundTagInitial
inboundTag = "in-" + tag;  routeTag = "route-" + tag      // БЕЗ префикса
listen = inboundSocks.listen;  port = start + i
```
«Стабильность» = детерминированная сортировка по тегу; добавление/удаление одной строки сдвигает порты всех туннелей с бо́льшим тегом. Порт выше 65535 не проверяется (portRange.end не ограничен сверху).

Связанные факты из apply-слоя (для контекста): outbound applicator сортирует манифест тем же компаратором, хеш плана = `sha256(JSON.stringify(entries.map(e=>e.jsonOutbound)))` (`outbound-applicator.ts:18-33`); при наличии топологии outbound-манифест цели строится из `tunnel.outboundTagCurrent` + `outboundInitial` (`:55-69`); без inboundSocks — из `buildTargetManifest` (prefix + override, `:42-53`).

---

## 13. Ошибки (`src/errors.ts`)

```go
type AppError struct { Code, Message string }  // name = имя класса
// ConfigError            Code "CONFIG_ERROR"      — env missing, schema invalid
// ApiRequestError        Code "API_REQUEST_ERROR" — gRPC (вне охвата)
// InputError             Code "EMPTY_INPUT"       — пустой вход
// ManifestValidationError Code "NO_VALID_ENTRIES" — parsed==0
```
Коды наружу нигде не выводятся (CLI печатает только `message`); важны тексты. Прочие ошибки (ФС, YAML, fetch, URIError, топология) — «голые» `Error` с сообщением как описано.

---

## 14. CLI (`src/cli.ts`, commander 14)

Программа `scd`, description `Generate and apply Xray resources from subscriptions.`, `showHelpAfterError()`. Опции принимают `--opt value` и `--opt=value`; повтор — последний выигрывает. Опции версии нет.

| Команда | Опции (все required) | stdout | exit |
|---|---|---|---|
| `generate` | `--input <input>` `--output <output>` `--log <log>` | `JSON.stringify(summary,null,2)+"\n"` | 0 |
| `sync` | `--config <config>` | `JSON.stringify(SyncReport,null,2)+"\n"` | `report.failed > 0 ? 1 : 0` |
| `daemon` | `--config <config>` | — (логи в stderr) | — |
| `validate-config` | `--config <config>` | `{"configPath": "<abs>", "valid": true}` (pretty 2, `\n`) | 0 |
| `print-config` | `--config <config>` | нормализованный конфиг (§2.6) | 0 |

Ошибки:
- любое исключение из action ⇒ stderr `<error.message>\n`, exit 1 (`cli.ts:67-70`). Без стека, без префикса.
- commander: неизвестная команда ⇒ stderr `error: unknown command 'foo'` + пустая строка + help программы, exit 1;
  отсутствует опция ⇒ `error: required option '--config <config>' not specified` + help подкоманды, exit 1;
  неизвестная опция ⇒ `error: unknown option '--x'` + help подкоманды, exit 1;
  без аргументов ⇒ help в **stderr**, exit 1; `--help`/`-h` ⇒ help в **stdout**, exit 0; `help [command]` — встроенная подкоманда.
- Текст help верхнего уровня:
```
Usage: scd [options] [command]

Generate and apply Xray resources from subscriptions.

Options:
  -h, --help                 display help for command

Commands:
  generate [options]
  sync [options]
  daemon [options]
  validate-config [options]
  print-config [options]
  help [command]             display help for command
```
- `daemon` при `runtime.mode != daemon` ⇒ `Config at <abs> is not in daemon mode.` (exit 1) (`run-daemon.ts:148-150`).
- `sync` игнорирует `runtime.mode` (всегда один прогон).

### 14.1 `generate` (`src/commands/generate.ts:10-20`)

1. `outputPath = resolve(--output)`, `logPath = resolve(--log)` (относительно cwd).
2. `loadInputSource(--input)` — format `auto`, timeout 5000, `-` = stdin.
3. `buildManifest(loaded.content, loaded.source)` — **content НЕ trimmed**, **фильтров нет**, `sourceFile` = абсолютный путь / URL / `"stdin"`.
4. `validateManifestOrThrow(manifest, loaded.source)` — при ошибке файлы **не создаются**.
5. `writeFile(output, JSON.stringify(manifest,null,2))` и `writeFile(log, JSON.stringify(parseLog,null,2))` — **без завершающего `\n`**, режим по умолчанию (0666 & umask), перезапись.
6. stdout — summary.

### 14.2 Пример (реальный вывод)

Вход `gen.txt`:
```
trojan://x
vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@h.example:443?type=ws&security=tls&path=%2Fws&host=cdn.example&alpn=h2&fp=chrome&sni=s.example&allowInsecure=0#🇩🇪 Берлин, Германия
vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@203.0.113.1:443?security=reality&type=tcp&flow=xtls-rprx-vision&sni=io.example&fp=chrome&pbk=PBK&sid=ab&headerType=none&encryption=none#🇳🇱 Амстердам
```
stdout:
```json
{
  "totalLines": 3,
  "parsed": 2,
  "skipped": 1,
  "filtered": 0,
  "filteredByCountry": 0,
  "filteredByLabelRegex": 0,
  "unsupportedScheme": 1,
  "unsupportedParam": 0,
  "unsupportedValue": 0,
  "unsupportedCombo": 0,
  "invalidUri": 0,
  "missingRequiredField": 0,
  "duplicateTag": 0
}
```
`out.json` (сокращено только по отступам; порядок ключей точный):
```json
{
  "version": 1,
  "generatedAt": "2026-10-17T08:32:15.445Z",
  "sourceFile": "/abs/gen.txt",
  "entries": [
    {
      "kind": "outbound",
      "tag": "vless-de-51bd674f-ws-tls-5cc7d5dc",
      "label": "🇩🇪 Берлин, Германия",
      "profile": "ws-tls",
      "line": 2,
      "country": { "emoji": "🇩🇪", "iso2": "DE", "nameEn": "Germany", "nameRu": "Германия" },
      "city": "Берлин",
      "normalized": {
        "kind": "outbound", "protocol": "vless", "profile": "ws-tls",
        "tag": "vless-de-51bd674f-ws-tls-5cc7d5dc", "line": 2,
        "raw": "vless://7d1b…#🇩🇪 Берлин, Германия",
        "label": "🇩🇪 Берлин, Германия", "address": "h.example", "port": 443,
        "uuid": "7d1b6590-1069-4372-92be-8d0a0ae6eaf5", "encryption": "none",
        "network": "ws", "security": "tls", "alpn": ["h2"], "sni": "s.example",
        "host": "cdn.example", "path": "/ws", "fingerprint": "chrome", "allowInsecure": false,
        "country": { "emoji": "🇩🇪", "iso2": "DE", "nameEn": "Germany", "nameRu": "Германия" },
        "city": "Берлин",
        "query": { "type": "ws", "security": "tls", "path": "/ws", "host": "cdn.example",
                   "alpn": "h2", "fp": "chrome", "sni": "s.example", "allowinsecure": "0" }
      },
      "jsonOutbound": {
        "tag": "vless-de-51bd674f-ws-tls-5cc7d5dc", "protocol": "vless",
        "settings": { "vnext": [ { "address": "h.example", "port": 443,
          "users": [ { "id": "7d1b6590-1069-4372-92be-8d0a0ae6eaf5", "encryption": "none" } ] } ] },
        "streamSettings": { "network": "ws", "security": "tls",
          "tlsSettings": { "serverName": "s.example", "alpn": ["h2"], "fingerprint": "chrome", "allowInsecure": false },
          "wsSettings": { "path": "/ws", "headers": { "Host": "cdn.example" } } }
      }
    },
    {
      "kind": "outbound",
      "tag": "vless-nl-3c2095ba-tcp-reality-vision-8878680f",
      "label": "🇳🇱 Амстердам", "profile": "tcp-reality-vision", "line": 3,
      "country": { "emoji": "🇳🇱", "iso2": "NL", "nameEn": "Netherlands", "nameRu": "Нидерланды" },
      "city": "Амстердам",
      "normalized": {
        "kind": "outbound", "protocol": "vless", "profile": "tcp-reality-vision",
        "tag": "vless-nl-3c2095ba-tcp-reality-vision-8878680f", "line": 3, "raw": "vless://…#🇳🇱 Амстердам",
        "label": "🇳🇱 Амстердам", "address": "203.0.113.1", "port": 443,
        "uuid": "7d1b6590-1069-4372-92be-8d0a0ae6eaf5", "encryption": "none",
        "network": "tcp", "security": "reality", "flow": "xtls-rprx-vision", "alpn": [],
        "sni": "io.example", "fingerprint": "chrome", "headerType": "none",
        "publicKey": "PBK", "shortId": "ab",
        "country": { "emoji": "🇳🇱", "iso2": "NL", "nameEn": "Netherlands", "nameRu": "Нидерланды" },
        "city": "Амстердам",
        "query": { "security": "reality", "type": "tcp", "flow": "xtls-rprx-vision", "sni": "io.example",
                   "fp": "chrome", "pbk": "PBK", "sid": "ab", "headertype": "none", "encryption": "none" }
      },
      "jsonOutbound": {
        "tag": "vless-nl-3c2095ba-tcp-reality-vision-8878680f", "protocol": "vless",
        "settings": { "vnext": [ { "address": "203.0.113.1", "port": 443,
          "users": [ { "id": "7d1b6590-1069-4372-92be-8d0a0ae6eaf5", "encryption": "none", "flow": "xtls-rprx-vision" } ] } ] },
        "streamSettings": { "network": "tcp", "security": "reality",
          "realitySettings": { "serverName": "io.example", "fingerprint": "chrome", "publicKey": "PBK", "shortId": "ab" } }
      }
    }
  ],
  "skipped": [
    { "line": 1, "raw": "trojan://x", "reasonCode": "unsupported_scheme", "reason": "Unsupported scheme \"trojan:\"." }
  ],
  "summary": { "totalLines": 3, "parsed": 2, "skipped": 1, "filtered": 0, "filteredByCountry": 0,
               "filteredByLabelRegex": 0, "unsupportedScheme": 1, "unsupportedParam": 0, "unsupportedValue": 0,
               "unsupportedCombo": 0, "invalidUri": 0, "missingRequiredField": 0, "duplicateTag": 0 }
}
```
`log.json`: `{"generatedAt": <тот же>, "summary": {...}, "skipped": [...]}`.

Сериализация JSON в Go должна совпадать с `JSON.stringify(v, null, 2)`: отступ 2 пробела, `": "`, пустые массивы `[]`, пустые объекты `{}`, **без HTML-экранирования** (`<`,`>`,`&` как есть — в Go `SetEscapeHTML(false)`), не-ASCII как есть, U+2028/U+2029 **как есть** (JS их не экранирует; Go `encoding/json` экранирует ⇒ нужен пост-процессинг/свой encoder), числа — целые без `.0`.

### 14.3 `sync` — форма отчёта (для полноты; детали — в документе про sync)

```go
type SyncReport struct {
    AppliedAt string `json:"appliedAt"`; DurationMs int `json:"durationMs"`
    Added, Replaced, Removed, Unchanged, Failed int // json: added, replaced, removed, unchanged, failed
    Targets []TargetSyncReport `json:"targets"`; Skipped []SkippedEntry `json:"skipped"`
}
type TargetSyncReport struct { SubscriptionID, TargetAddress, SourceID string; UnchangedKinds []string; Failed int; Resources []ApplyReport; Skipped []SkippedEntry }
type ApplyReport struct { Kind, SourceID, SubscriptionID, TargetAddress, AppliedAt string; DurationMs, Added, Replaced, Removed, Failed int;
    DeletedIDs, AppliedIDs []string; Items []ApplyReportItem /* {id,status,message?} */; Skipped []SkippedEntry }
```
Реальный пример при неудачной загрузке источника/пустом манифесте (Xray не вызывается): для каждого включённого resource kind
создаётся failed ApplyReport с `items: [{"id": "<kind>", "status": "failed", "message": "<error>"}]`, `failed: 1`, `skipped: []`.
**Порядок `targets`**: сначала все `failed` загрузки (в порядке сортировки id), затем `loaded` — не единый порядок по id.

---

## 15. Лог-события доменного уровня

Логгер pino → **stderr** (`create-logger.ts:7-27`), `base: undefined` (нет pid/hostname).
JSON-строка: `{"level":<num>,"time":<epoch ms>,<поля...>,"msg":"<текст>"}`; уровни: trace 10, debug 20, info 30, warn 40, error 50, fatal 60; `silent` — ничего.
`pretty`: pino-pretty, colorize, `translateTime: 'SYS:standard'`, `ignore: 'pid,hostname'`, в fd 2.

| event | level | поля (в порядке) | msg | где |
|---|---|---|---|---|
| `config_loaded` | info | `configPath` | `Config loaded.` | `sync-once.ts:494`, `run-daemon.ts:160` |
| `sync_started` | info | `sourceCount` (= число **всех** подписок в конфиге, включая выключенные) | `Sync started.` | `sync-once.ts:393` |
| `subscription_fetched` | info | `subscriptions: [{id, source, encoding, targetAddress}]` (только loaded) | `Subscriptions loaded.` | `:396-406` |
| `subscription_fetch_failed` | error | `subscriptionId, source, error, targetAddress` | `Subscription fetch failed.` | `:356-366` |
| `manifest_validation_failed` | error | `subscriptionId, sourceId, error, targetAddress` | `Manifest validation failed.` | `:324-332` |
| `manifest_built` | info | `kind, subscriptionId, sourceId, managedCount, skipped` (+ `filtered, filteredByCountry, filteredByLabelRegex` если у плана есть manifest.summary), `targetAddress` | `Manifest built.` | `:447-460` |
| `apply_failed`/`apply_finished` | error/info | `kind, subscriptionId, targetAddress, sourceId, added, replaced, removed, failed, deletedIds, appliedIds, durationMs, errorMessages[], failedItems[]` | `Apply failed.`/`Apply finished.` | `:170-201` |
| `sync_skipped_overlap` | warn | — | `Sync is already running in this process; skipping overlap run.` | `:475` |
| `sync_failed` | error | `error` | `Sync failed.` | `:484` |

Реальные строки (неудачные источники):
```
{"level":30,"time":1792226039473,"event":"sync_started","sourceCount":2,"msg":"Sync started."}
{"level":50,"time":1792226039480,"event":"subscription_fetch_failed","subscriptionId":"missing","source":"/abs/nope.txt","error":"ENOENT: no such file or directory, open '/abs/nope.txt'","targetAddress":"127.0.0.1:1","msg":"Subscription fetch failed."}
{"level":50,"time":1792226039481,"event":"manifest_validation_failed","subscriptionId":"bad","sourceId":"bad","error":"Subscription source \"bad\" produced no valid outbound entries. parsed=0 ...","targetAddress":"127.0.0.1:2","msg":"Manifest validation failed."}
```
Ошибки до создания логгера (конфиг) логов не дают — только stderr-сообщение CLI.

---

## 16. Тонкости, которые легко потерять при порте

1. **Хеш тега от НЕтримленной `raw`**: ведущие/хвостовые пробелы, BOM, одиночный `\r` в конце меняют тег.
2. **sync тримит весь content, generate — нет** (`generate-manifest-from-source.ts:100` vs `generate.ts:14`): в sync ведущие пустые строки исчезают ⇒ **номера строк сдвигаются**, а у первой/последней строки срезается окружающий whitespace ⇒ **теги могут отличаться** между `generate` и `sync` для одного входа.
3. JS `trim`/`\s` ≠ Go `TrimSpace`/`unicode.IsSpace` (U+FEFF, U+0085). Файл с BOM: в sync BOM снимается trim'ом content, в generate — остаётся в `raw` строки 1 (тег зависит от BOM), но `trimmed` без BOM ⇒ строка парсится. HTTP-ответ: BOM снимает `text()`. Строка с ведущим NEL ⇒ `invalid_uri`.
4. `decodeURIComponent` бросает на кривом `%` в label/username ⇒ **весь источник падает** (не skip строки) с сообщением `URI malformed`.
5. `headerType=` (пусто) сохраняется как `"headerType": ""` в normalized; остальные пустые параметры (`flow=`, `sni=` …) — опускаются. `allowInsecure=` (пусто) — **отказ** строки.
6. `type`/`security` регистронезависимы, `flow`, `encryption`, `headerType` — регистрозависимы; `allowInsecure` true/false — регистронезависимо.
7. Ключи query lowercased в `query`; details/reason ошибок combo используют исходные значения; details `allowInsecure=`/`headerType=` пишутся в camelCase, хотя ключ lowercased.
8. Порядок ключей `query` и списка unsupported params — JS-порядок свойств (числовые ключи первыми).
9. Последний дубликат параметра выигрывает; `;` не разделитель; `+` → пробел; `%zz` остаётся буквально.
10. Host не lowercased; IPv6 нормализуется и хранится со скобками; percent-escape в host остаётся.
11. Табы внутри строки удаляются URL-парсером (влияет на label), но не на raw/хеш.
12. `inferCity` отбрасывает город, если в первой части встречается `extra` где угодно (`Extraordinary` тоже).
13. `extractCountryInfo` — строго первые 2 code point; флаги вне 45 стран ⇒ `xx`, но `inferCity` всё равно срезает их.
14. `slugify` режет до 32 **после** удаления дефисов по краям ⇒ возможен хвостовой `-` и `--` в теге.
15. Город без ASCII-символов ⇒ sha8(город) — UTF-8 байты, без нормализации.
16. tcp-tls сохраняет `flow` (в т.ч. vision) в JSON; `visionUdp443Override` трогает только reality.
17. Prefix (`observatorySubjectSelectorPrefix`) применяется только к outbound-тегу; `in-`/`route-` теги и порты — по базовому тегу.
18. Порты назначаются по ICU/байтовому порядку **тегов**, не по порядку строк.
19. `countryAllowlist` нормализуется (trim+upper) в конфиге; `print-config` покажет `DE` для `de`.
20. Фильтры не меняют `skipped`; меняют `parsed`. Пустой allowlist ⇒ фильтр выключен.
21. Regex — ECMAScript (lookahead!), флаг `y` значим, `g` нет.
22. Валидация конфига: выключенные подписки участвуют в проверке дубликатов адресов и inboundSocks; `return` после отсутствующего daemon-schedule глушит остальные кросс-проверки; refine'ы глушатся «прерывающими» zod-ошибками.
23. `${VAR}` подставляется и в комментариях; подстановка до YAML.
24. `statusServer.listen` порт парсится JS `Number()` (hex, экспонента, пробелы допустимы).
25. URL-валидация в конфиге = «WHATWG URL парсится», любая схема.
26. `input` с `HTTP://` — remote (регистронезависимо); `ftp://…` — файл; относительные пути в конфиге — от каталога конфига, в `generate` — от cwd.
27. HTTP: таймаут на весь запрос с телом; сообщение таймаута имеет приоритет; `User-Agent: node`; редиректы следуют; `source` = исходный URL.
28. Base64: Node-лениентность (стоп на первом `=`, пропуск мусора) — в режиме `base64` мусор не ошибка; в `auto` строгая проверка алфавита.
29. Пустой вход проверяется до и после декодирования; сообщение содержит `stdin` / URL / абсолютный путь.
30. `loadSubscriptions` последовательный; ошибка одной подписки не останавливает остальные; `failed[].source` = конфиговый input.
31. `generate` пишет файлы без завершающего `\n`, stdout — с `\n`; при `NO_VALID_ENTRIES` файлы не создаются.
32. JSON: без экранирования `<>&`, U+2028/2029 не экранировать, порядок ключей как в TS.
33. `nameEn` берётся из ICU — захардкодить (`Türkiye`, `Hong Kong SAR China`, `Czechia`).
34. Сортировка подписок по `id` — ICU-коллация, не байты.
35. `sourceCount` в `sync_started` считает все подписки, включая выключенные.

---

## 17. Инвентарь тестов (→ Go test cases)

### 17.1 `test/parser.test.ts` (фикстура `test/fixtures/vpn-sample.txt`: 12 строк — 8×tcp+tls (`path=%2F`, `alpn=h2`), 2×ws+tls (`flow=` пустой, `allowInsecure=1`, label с `#2(Test)`), 2×reality-vision)

| # | Тест (строка) | Утверждение |
|---|---|---|
| P1 | `scanLines reads current vpn sample` (:25) | фикстура даёт > 10 непустых строк |
| P2 | `buildManifest parses … supported profiles` (:31) | профили включают tcp-tls, ws-tls, tcp-reality-vision; `summary.parsed == len(entries)` |
| P3 | `filters by country allowlist` (:41) | 2 строки AT/DE, allowlist `[DE]` ⇒ 1 entry DE; parsed 1, filtered 1, byCountry 1, byRegex 0 |
| P4 | `filters by label regex` (:74) | regex `/,\s*Extra(?!\s*Whitelist)\s*$/i`; `…Extra` проходит, `…Extra Whitelist#2` нет ⇒ parsed 1, filtered 1, byCountry 0, byRegex 1; label оканчивается на `Extra` |
| P5 | `applies country and label filters sequentially` (:107) | AT Extra, DE Extra, NL Extra Whitelist#2; allowlist [DE,NL] + regex ⇒ 1 entry DE; filtered 2 (1+1) |
| P6 | `country map resolves Austria` (:143) | `extractCountryInfo('🇦🇹 Вена, Австрия, Extra') == {emoji:'🇦🇹', iso2:'AT', nameEn:'Austria', nameRu:'Австрия'}` |
| P7 | `same country with different cities yields different tags` (:152) | одинаковый uuid/host, `Вена` vs `Зальцбург` ⇒ 2 entries с разными тегами |
| P8 | `skips duplicate generated tags` (:162) | одна строка дважды ⇒ 1 entry, parsed 1, skipped 1, duplicateTag 1, `skipped[0].reasonCode == duplicate_tag` |
| P9 | `non-vless scheme is skipped` (:173) | `trojan://example` ⇒ `unsupported_scheme` |
| P10 | `unknown vless parameter is skipped` (:182) | `foo=bar` ⇒ `unsupported_param` |
| P11 | `base64 subscription content is decoded` (:193) | `decodeSubscriptionContent(base64(vless-line))` начинается с `vless://` |
| P12 | `loadInputSource downloads from URL and decodes base64` (:202) | fetch вызван без headers и с abort-signal; source = URL; encoding base64; content начинается с `vless://` |
| P13 | `rejects timed out remote content` (:229) | висящий fetch, `fetchTimeoutMs: 10` ⇒ ошибка `/timed out after 10ms/` |
| P14 | `rejects empty remote content` (:248) | тело `""`, 200 ⇒ `/returned empty content/` |
| P15 | `rejects empty local file content` (:266) | файл `"   \n"` ⇒ `/returned empty content/` |
| P16 | `plain file does not depend on fetch timeout` (:278) | файл + `fetchTimeoutMs: 1` ⇒ ок, source = путь, content начинается с `vless://` |
| P17 | `reads from stdin and decodes base64` (:284) | stdin = base64 ⇒ source `stdin`, encoding base64 |
| P18 | `rejects empty stdin content` (:309) | stdin `"   \n"` ⇒ `/returned empty content/` |
| P19 | `unsupported vless combination is skipped` (:327) | `type=grpc&security=tls` ⇒ парсер ок, normalize ⇒ `unsupported_combo` |
| P20 | `ignores trailing empty lines` (:342) | строка + `\n` ⇒ totalLines 1, parsed 1, skipped 0 |
| P21 | `builders produce JSON and gRPC payload for ws+tls` (:352) | JSON: network ws, security tls; gRPC raw непуст и содержит `websocket` и `xray.transport.internet.tls.Config` (gRPC — вне охвата этого документа) |
| P22 | `grpc payload uses reality type names` (:376) | gRPC содержит `xray.transport.internet.reality.Config`, не содержит `\bws\b` (вне охвата) |
| P23 | `applyOutbounds replaces existing tags` (:396) | apply-слой (вне охвата): items[0] removed, items[1] replaced; порядок remove→add |
| P24 | `applyOutbounds rolls back on add failure` (:423) | apply-слой (вне охвата): последний item failed, финальный вызов — re-add старого raw |
| P25 | `generateCommand rejects manifest with no valid entries` (:455) | файл `trojan://example\n` ⇒ `/produced no valid outbound entries/` |

### 17.2 `test/topology.test.ts`

| # | Тест (строка) | Утверждение |
|---|---|---|
| T1 | `allocates stable ports and target-scoped tags` (:35) | 2 entries (DE строка первой, AT второй), range 20000–20010, prefix `x-observe-` ⇒ 2 туннеля, порты 20000, 20001 (по сортировке тегов: `vless-at…` < `vless-de…`); `outboundTagInitial` начинается с `x-observe-`; `outboundWithoutPrefix.tag == baseOutboundTag`; `inboundTag` с `in-`, `routeTag` с `route-` |
| T2 | `overrides reality vision flow per target` (:60) | без override flow `xtls-rprx-vision` в обоих outbound; с `visionUdp443Override` — `xtls-rprx-vision-udp443` в обоих (initial и withoutPrefix) |
| T3 | `fails when port range is too small` (:80) | 2 entries, range 20000–20000 ⇒ ошибка `/can allocate only 1 ports, but 2 tunnels were generated/` |
| T4 | `buildStatusSnapshot exposes display name…` (:107) | (runtime-state, вне охвата) snapshot: subscriptionId `source-1`, targetAddress, countryIso2 `AT`, endpoint `127.0.0.1:20000`, displayName содержит `Вена`; ключ состояния `"<subId>::<address>"` |
| T5 | `…whether tunnel is still selected by balancer prefix` (:128) | (вне охвата) `balanced == true` пока `outboundTagCurrent == outboundTagInitial` с префиксом; после замены на `outboundWithoutPrefix.tag` — `false` |

### 17.3 `test/service.test.ts` (конфиг/загрузка/генерация)

| # | Тест (строка) | Утверждение |
|---|---|---|
| S1 | `resolves relative paths, targets and interpolates env vars` (:67) | `${XRAY_API_ADDRESS}` ⇒ `127.0.0.1:8080`; fetchTimeoutMs 7000; countryAllowlist `[de]` ⇒ `[DE]`; labelIncludeRegex сохраняется дословно; timeoutMs 5000; fixedOutbounds `[direct]`; prefix `x-observe-`; input = `resolve(configDir,'subscription.txt')`; statusServer default enabled, listen `127.0.0.1:9090`, remoteInstances `[]` |
| S2 | `parses visionUdp443Override and defaults it to false` (:121) | true у первой, false (default) у второй |
| S3 | `rejects duplicate target addresses` (:162) | `/Duplicate target address "127\.0\.0\.1:8080"/` |
| S4 | `rejects legacy targets array shape` (:198) | `targets:` вместо `target:` ⇒ `/subscriptions\.0\.target:/` (missing target; unknown key тоже будет) |
| S5 | `rejects mixed target and legacy targets` (:228) | `/subscriptions\.0: Unrecognized key: "targets"/` |
| S6 | `rejects invalid daemon cron expression` (:260) | `mode: daemon, schedule: "not-a-cron"` ⇒ `/runtime\.schedule:/` |
| S7 | `rejects unsupported country code` (:292) | `ZZ` ⇒ `/Unsupported ISO2 country code "ZZ"/` |
| S8 | `rejects invalid regex literal` (:325) | `/(/i` ⇒ `/labelIncludeRegex:/` |
| S9 | `rejects legacy speedtest config` (:357) | `target.speedtest` ⇒ ошибка содержит `speedtest` (strict target) |
| S10 | `accepts monitor.maxParallel and statusServer.runtimeState defaults` (:408) | maxParallel 4; retry default `{attempts:1, delayMs:250}`; runtimeState `{true,false,false}` |
| S11 | `accepts balancerMonitor config and rejects incomplete` (:467) | валидный: enabled, socks5.port 1080, remotePing enabled/timeout 3000/viaSocks true; `balancerMonitor: {enabled: true}` ⇒ `/balancerMonitor\.(schedule|socks5|request)/` |
| S12 | `validates monitor and balancerMonitor retry` (:542) | retry `{2,100}` / `{3,150}`; `attempts: 0` ⇒ `/attempts/`; `delayMs: -1` ⇒ `/delayMs/` |
| S13 | `validates balancerMonitor remotePing` (:655) | remotePing без url ⇒ `/remotePing\.url/`; `timeoutMs: 0` ⇒ `/timeoutMs/`; remotePing при выключенном balancerMonitor ⇒ `/requires balancerMonitor\.enabled/`; `monitor.remotePing` ⇒ `/remotePing/` (unrecognized key) |
| S14 | `rejects legacy balancerMonitor successGet` (:784) | `/successGet/` (unrecognized key) |
| S15 | `loadSubscriptions sorts enabled subscriptions by id` (:829) | вход [source-b, source-a] ⇒ loaded `[source-a, source-b]`, failed `[]` |
| S16 | `outbound applicator stabilizes manifest hash and managed ids for reordered lines` (:868) | переставленные строки ⇒ одинаковые manifestHash и managedIds; entries отсортированы по тегу |
| S17 | `outbound applicator applies target-specific observatory prefix` (:903) | после preparePlanForTarget с prefix: hash отличается от базового; managedIds[0], entries[0].tag, normalized.tag, jsonOutbound.tag начинаются с `x-observe-` |
| S18 | `keeps other subscriptions running when one source load fails` (:1377) | failed source-a + loaded source-b ⇒ report.failed 1, 2 targets: [0] = 8080 (failed 1), [1] = 8081; клиент создан только для 8081 |
| S19 | `does not call Xray API when manifest has no valid entries` (:1476) | `trojan://example` ⇒ target 8080 failed 1, клиент не создан; 8081 ок |
| S20 | `does not call Xray API when filters remove all entries` (:1569) | allowlist `[US]` на AT-строку ⇒ target failed 1, ни одного клиента |

(Остальные service-тесты :931–1306 — apply/sync-механика, вне охвата.)

### 17.4 Рекомендуемые дополнительные Go-тесты (из проверенных краевых случаев)

- §5.2/§5.3 таблица: `VLESS://…@Example.COM:0443?Type=TCP&Security=TLS#x` ⇒ ok, address `Example.COM`, port 443, query `{"type":"TCP","security":"TLS"}`, tag `vless-xx-x-tcp-tls-78eaa7cd`.
- `#100%`, `#a%FFb` ⇒ ошибка `URI malformed` уровня источника.
- `:65536`, `@:443`, `ho st`, `h^x`, `443x`, `+443` ⇒ `invalid_uri`; `host` без порта, `:0` ⇒ `missing_required_field` (address and port).
- nil UUID ⇒ `missing_required_field` (UUID); uppercase UUID принимается как есть.
- `&1=2&zz=3&&=x` ⇒ details `["1","zz",""]`.
- `sni=a&sni=b` ⇒ `b`; `path=/a+b%2Fc%zz;x` ⇒ `/a b/c%zz;x`.
- `[2001:0DB8:0:0::1]` ⇒ address `[2001:db8::1]`, city-slug `2001-db8-1`.
- `encryption=None` ⇒ unsupported_value; `headerType=` ⇒ ok с `headerType: ""`; `allowInsecure=` ⇒ unsupported_value.
- `type=tcp` без security ⇒ combo details `["type=tcp","security=","flow="]`.
- reality без sid ⇒ `REALITY VLESS requires parameter "sid".`; reality без flow ⇒ combo.
- tcp+tls+flow vision ⇒ ok, `users[0].flow` присутствует; `alpn=h2,%20http/1.1,,` ⇒ `["h2","http/1.1"]`.
- Тег от `"  "+line+"  "` ≠ тег от `line`; BOM-префикс в raw даёт третий тег.
- Строка с ведущим U+0085 ⇒ `invalid_uri`.
- Base64: url-safe, без padding, с переносами ⇒ decode; `=` в середине / `!` ⇒ plain.
- Фильтр `/Extra/y` ⇒ только label, начинающийся с `Extra`.
- sha8(`Вена`) = `3ed0e2a1`.
- Конфиг: сообщения из §2.3–2.4 (пустой файл, enum, unrecognized keys, refine-abort, `localhost:0x50` валиден, `listen: ':80'` невалиден, `listen: ''` при `enabled:false` валиден, `${NOPE}` в комментарии ⇒ ошибка).
- CLI: коды выхода и потоки из §14.

---

## 18. Рекомендации по Go-реализации (минимум для «exactly»)

- URL: WHATWG-совместимый парсер (напр. `github.com/nlnwa/whatwg-url`) или собственный парсер подмножества non-special URL; **не** `net/url` (иначе: регистр/escape хоста, `;` в query, ошибки на `%zz`, IPv6-нормализация).
- Query: собственный `application/x-www-form-urlencoded` парсер + упорядоченная map с JS-порядком ключей.
- `decodeURIComponent`: strict percent-decode с проверкой UTF-8 (ошибка ⇒ ошибка источника `URI malformed`).
- Unicode: `golang.org/x/text/unicode/norm` (NFKD), JS-совместимые `trim`/`\s`, `\p{Regional_Indicator}` = U+1F1E6..U+1F1FF.
- Regex фильтра: `github.com/dlclark/regexp2` (ECMAScript) + ручная обработка флагов; тексты ошибок валидации V8 воспроизвести хотя бы для частых случаев (`Invalid flags supplied to RegExp constructor '<flags>'`, `Invalid regular expression: /<p>/<f>: <reason>`).
- Base64: собственный Node-совместимый лениентный декодер.
- Коллация id: `golang.org/x/text/collate`.
- JSON: ordered encoder, 2-space indent, без HTML-escape, без экранирования U+2028/2029, omit только «undefined»-полей.
- YAML: декод в дерево + ручная валидация с zod-совместимыми путями/сообщениями; запрет дубликатов ключей; YAML 1.2 bool-семантика.
- Cron: парсер, совместимый с cron-parser v5 (5/6 полей, алиасы, L/W/#/H) — согласовать с документом про runtime.

---

## 19. Проверенные краевые случаи (метод)

Пакет скопирован в scratchpad, `npm ci`, `tsc -p tsconfig.build.json`, вызовы функций из `dist/` и CLI на Node 22.22.0.
Все примеры тегов/сообщений/выводов в этом документе получены оттуда (хеши в тегах зависят от точной `raw`-строки).
