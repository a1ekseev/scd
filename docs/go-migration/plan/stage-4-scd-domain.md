# Этап 4. scd: доменный слой

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development; TDD обязателен. Прочитать «Глобальные ограничения» и «Открытые вопросы» в `docs/go-migration/plan/README.md`.

**Goal:** конфиг scd, нормализация VLESS-ссылок под Xray v26.10.10 с максимальной совместимостью в scope D26 (транспорты tcp/raw, ws, xhttp; security tls, reality; шифрование none и ML-KEM), построение JSON outbound и его превращение в protobuf через `infra/conf` (D19), манифест/фильтры/`scd generate`, топология туннелей.

**Architecture:** `vless.Link` (common) → `normalize.Normalize` (scope, профиль, ML-KEM-ключи, flow, маппинг параметров, warnings, тег; 4.3–4.5) → `outbound.JSON` (форма JSON-конфига Xray v26.10.10, она же хэшируется) → `outbound.ToProto` (`conf.OutboundDetourConfig.Build()`) → `manifest` (entries/skipped/summary, фильтры) → `topology` (порты, теги туннелей). Всё — чистые функции без сети.

**Tech Stack:** `github.com/xtls/xray-core v1.260327.1-0.20261010092107-701af60772cd` (`infra/conf`, `core`, `common/uuid`, `transport/internet/tls`), `golang.org/x/text/unicode/norm`, `internal/common/*`.

**Spec:** `07` D8, D9, D11, D13, D15–D19, D21, D24–D33 (D26 заменяет D14); **`11-xray-vless-matrix.md` (источник истины для 4.3–4.7)**; `12-real-subscriptions.md` §3, §5, §7, §9 (реальные формы ссылок, `cs`/`heartbeatPeriod`, правки 4.2–4.7); `09` (§1, §1.4, §2.3, §6; там, где 09 расходится с 11, действует 11); `01` §1–§2, §6–§12, §14, §16–§17; `02` §1.3, §1.6; README плана O4, O6, O9, O13, O15, O16.

## Контекст этапа

- Теги: алгоритм `01` §6.4 не меняется (`vless-<cc>-<city>-<profile>-<sha8(raw)>`), поэтому у старых профилей теги совпадают с TS (09 §5). Новые профили — новые имена.
- `jsonOutbound` — **новая форма** (09 §6, §7 п.10): без `allowInsecure`, `wsSettings.host` вместо `headers.Host`, `realitySettings.password`+`spiderX`, lowercase `fingerprint`, `users[].email = tag`. Эталоны этапа 0 по этому полю расходятся намеренно.
- Набор профилей после этапа (11 §3): `tcp-tls`, `ws-tls`, `tcp-reality-vision` (как в TS — теги этих строк не меняются), `tcp-reality`, `xhttp-tls`, `xhttp-reality`, `xhttp-reality-vision` (только с ML-KEM), `tcp-mlkem`, `ws-mlkem`, `xhttp-mlkem`. grpc/httpupgrade/kcp и прочие транспорты, `ws+reality`, `security=none` без ML-KEM — `unsupported_combo`.
- Разделение ответственности (11 §5): normalize проверяет scope/сочетания/flow/ML-KEM-ключи/`allowInsecure`/JSON-форму `extra`/`fm`; значения параметров валидирует `conf.Build()` в манифесте (skip «Xray rejected the outbound configuration.» с текстом Xray). Так skip происходит ровно тогда, когда Xray отвергает (D26).
- Корпус `testdata/reference/scd/corpus/d26-links.txt` + `d26-expect.json` (задача 0.5) — общий вход тестов 4.3–4.7 и live-теста 5.6.
- Warnings нормализации попадают в `ManifestEntry.Warnings` и логируются в sync событием `manifest_entry_warning` (этап 6): `flow_cleared`, `allow_insecure_ignored` — уровень `warn`; `param_ignored` (только неизвестные параметры, D28) — уровень `debug`. Неприменимые к transport/security параметры (O16) warning **не** дают (D28; поправка к 11 §4).
- Реальные формы (D33, 12): обезличенные фикстуры `docker/fixtures/real-shapes/{remnawave.txt,provider2.txt,provider2.b64}` — вход тестов `…RealShape…` в 4.2–4.7. Метки не интерпретируются сверх штатного извлечения страны/города (D33): никаких проверок суффиксов вроде `-X`.

---

### Task 4.1: Конфиг scd, `validate-config`, `print-config`

> **Поправка D29/D30 (обязательна):** фильтр меток — `labelIncludeRegex` (строка или список, AND) + новый `labelExcludeRegex` (строка или список, OR) через `labelfilter.Parse` (создан в 1.3 вместе с `TestLabelFilterRealShapeEquivalence` и `allowedDiffs` `Extra\nWhiteList` — здесь не дублируется; ревью-2 F03); lookahead → обычная ошибка `regexlit` (D31: без спец-подсказок). Здесь — `subscriptions[].filters.{labelIncludeRegex,labelExcludeRegex}` (оба необязательны); тест `TestSCDRealShapeGoFormValid` — Go-форма `docker/scd/config.real-shape.go.yml` (создать в этой задаче из `docker/scd/config.real-shape.yml`: фильтры по таблице D29) проходит `Load` и `scd validate-config`. Совместимость со старой формой не требуется (D31).

**Read:** `01` §1.1, §2 (правила; тексты zod не нужны — D9), §14 (CLI validate/print), §17.3 S1–S14; `03` §6.5 (редактирование секретов); `07` D8, D12, D15, D22, D23, D24, D27 (O9, O10), D28, D29, D32, D33; `10` §6.5; `docker/scd/config.example.yml`; `testdata/reference/scd/print-config/*`.

**Files:**
- Create: `internal/scd/config/types.go`, `load.go`, `rules.go`, `load_test.go`
- Modify: `internal/scd/cli/cli.go`; Create: `internal/scd/cli/config_cmds.go`, `config_cmds_test.go`
- Create: `docker/scd/config.real-shape.go.yml` (Go-форма из `docker/scd/config.real-shape.yml`, фильтры по таблице D29; см. поправку D29/D30)
- Modify: `docker/scd/config.example.yml` (закомментированный пример `fetchHeaders: {x-hwid: "${SCD_HWID}"}` — закомментирован, т.к. scd падает на отсутствующей env), `docs/CONFIG-MIGRATION.md` (D32: `subscriptions[].fetchHeaders`; фильтры D29; `allowInsecure`, `fetchUserAgent`, `balancerSelection`, `logging.format`)

**Interfaces:**
- Consumes: `configload.Load(path, configload.EnvRawStrict, os.LookupEnv)`, `configload.ResolveInput`, `schema` (+ `Node.StringMap`, 1.3), `inputsrc.Headers`/`inputsrc.CheckHeaders` (1.6), `cron.Parse`, `listenaddr.Parse`, `labelfilter.Parse` (1.3), `country.Supported` (**создать в этой задаче** минимальный `internal/scd/country/codes.go` со списком 45 ISO2 из `01` §7 и `func Supported(iso2 string) bool`; задача 4.2 дополнит пакет).
- Produces: типы `01` §1.1 в том же порядке полей (JSON-имена те же) плюс:
  - `SubscriptionConfig.AllowInsecure string` `json:"allowInsecure"` — `deny|allow`, default `deny` (D15, O6);
  - `SubscriptionConfig.FetchUserAgent string` `json:"fetchUserAgent,omitempty"` — User-Agent загрузки подписки по http(s); trim; ключ задан, но пуст → issue `must not be empty`; отсутствует → пусто (дефолт `scd/<version>` подставляет `inputsrc`, D27 O9; имя ключа — README плана O9);
  - `SubscriptionConfig.FetchHeaders inputsrc.Headers` `json:"fetchHeaders,omitempty"` (D33) — после `fetchUserAgent`; необязательный объект `имя: значение` (`schema.StringMap`; issues `inputsrc.CheckHeaders` по пути `subscriptions.N.fetchHeaders.<имя>`); env-подстановка `${NAME}` работает как везде (EnvRawStrict); значения — секрет: `print-config` печатает `<redacted>` (через `Headers.MarshalJSON`; отличие от правила «print-config печатает секреты как есть», `01` §2.6 — D33), в логах не встречаются (`Headers.LogValue`);
  - `logging.format` — enum `json|pretty` (default `json`), `logging.level` — как в `01` §1.1; логгер всегда пишет в stderr (D27 O10);
  - `SubscriptionTargetConfig.BalancerSelection BalancerSelectionConfig` `json:"balancerSelection"` после `balancerMonitor`: `{Enabled bool "enabled"; BalancerTag string "balancerTag,omitempty"; Schedule string "schedule,omitempty"; Observatory bool "observatory"; Strategy string "strategy,omitempty"}`, default `{enabled:false, observatory:true}`, strict; `strategy` ∈ `leastPing|leastLoad|random|roundRobin`.
  - `type Loaded struct{ Path string; Config AppConfig }`; `func Load(path string) (Loaded, []string /*warnings*/, error)`.
  - Фильтры храним уже скомпилированными (D29; ревью-2 F04 — прежняя форма `LabelIncludeRegex string` + `re` противоречила D29): `SubscriptionFiltersConfig{ CountryAllowlist []string `json:"countryAllowlist,omitempty"`; Label labelfilter.Filter `json:"-"` }` (как `Output.Filter` в 3.1; 7.3 reflection-walker опирается на `json:"-"` + явный список) — в JSON `print-config` ключи `labelIncludeRegex`/`labelExcludeRegex` (списки, omitempty) встраиваются на уровень `filters` (свой `MarshalJSON`).
- Правила: `01` §2.2 (дефолты, strict/strip-объекты, trim, `countryAllowlist` → trim+upper, длина 2, поддерживаемые коды), `01` §2.3 п.1–9 (кросс-полевые, **все** проверки выполняются, без «return после daemon-schedule»), `10` §6.5 (`balancerTag` и `schedule` обязательны при `enabled`; без `observatorySubjectSelectorPrefix` — warning `balancerSelection is enabled without observatorySubjectSelectorPrefix; scd tunnels are unlikely to match the balancer selector`), URL-поля — `schema.URL`. Сообщения — Go (`<path>: <message>`), смысл — как в `01` §2.3.
- CLI: `validate-config --config` → warnings в stderr (`warning: …`), stdout `{"configPath": "<abs>", "valid": true}`; `print-config --config` → `jsonout.Write(cfg)`.

- [ ] **Step 1: Failing tests** `load_test.go` — перенос `01` §17.3 S1–S14 (проверять путь issue и ключевую подстроку, не точный текст zod): S1 (env, относительный input, `[de]`→`[DE]`, дефолты statusServer), S2, S3 (`Duplicate target address` → `duplicate target address "127.0.0.1:8080"`), S4, S5, S6 (`not-a-cron` → путь `runtime.schedule`), S7 (`ZZ`), S8 (`/(/i` → путь `labelIncludeRegex`), S9 (`target.speedtest` → unknown key), S10, S11, S12, S13, S14. Новые: `TestAllowInsecureDefaultDeny`, `TestAllowInsecureRejectsOther`, `TestBalancerSelectionRequiresTagAndSchedule`, `TestBalancerSelectionSixFieldCron` (`*/15 * * * * *` ок), `TestScheduleCronFollowsLibrary` (D28: `runtime.schedule: "0 0 L * *"` и `"0 0 31 2 *"` → issue с путём `runtime.schedule`; `"0 0 ? * *"` и `"@every 5m"` — ок), `TestFetchUserAgentOptional` (отсутствует → `""`, `"  "` → issue `subscriptions.0.fetchUserAgent`), `TestFetchHeaders` (D33: `fetchHeaders: {x-hwid: "${HW}"}` с `HW=s3cr3t` → `FetchHeaders.Plain()["x-hwid"]=="s3cr3t"`; `{User-Agent: x}` → issue `subscriptions.0.fetchHeaders.User-Agent: use fetchUserAgent instead`; `{"bad name": x}` → `invalid header name`; `{x-a: 1}` → `expected string`), `TestLoggingFormatEnum` (`pretty` ок, `text` → issue), `TestBalancerSelectionWarnsWithoutPrefix`, `TestListenEmptyWhenDisabledOK`, `TestLookaheadRegexRejected` (D8), `TestAllCrossFieldIssuesReported` (daemon без schedule + inbounds без outbounds → обе issue).
- [ ] **Step 2: Failing tests** `config_cmds_test.go`: `TestPrintConfigMinimal` (семантически = `testdata/reference/scd/print-config/minimal.json` + новые ключи `allowInsecure: "deny"`, `balancerSelection: {enabled:false, observatory:true}`; `fetchUserAgent` отсутствует (omitempty); `input` сравнивать по суффиксу), `TestPrintConfigDockerExample`, `TestValidateConfigJSON`, `TestSCDRealShapeGoFormValid` (D30), `TestPrintConfigRedactsFetchHeaders` (D33: конфиг с `fetchHeaders: {x-hwid: s3cr3t}` → stdout содержит `"x-hwid": "<redacted>"` и не содержит `s3cr3t`; `validate-config` с тем же конфигом — stdout/stderr без `s3cr3t`).
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.**
- [ ] **Step 5: Verify** `go test -race ./internal/scd/... && go build -o bin/scd ./cmd/scd && ./bin/scd validate-config --config docker/scd/config.example.yml && ./bin/scd print-config --config docker/scd/config.example.yml | jq '.subscriptions[0].allowInsecure'` → `"deny"`.
- [ ] **Step 6: Commit** `feat(scd): port scd config with allowInsecure, balancerSelection, fetchUserAgent and fetchHeaders options` (body: D8, D15, D22, D23, D24, D27, D28, D29, D33) — вместе с пунктами `docs/CONFIG-MIGRATION.md` (D32).

---

### Task 4.2: Карта стран и тег outbound'а

**Read:** `01` §6.4, §7, §16 п.1, 12–15, §17.1 P6–P7, §17.4 (sha8, теги); `07` D9, D33 (теги без изменений); `12-real-subscriptions.md` §5 (теги реальных форм), §7 п.11–13, §9 п.6.

**Files:**
- Modify/Create: `internal/scd/country/country.go`, `country_test.go` (расширить пакет из 4.1)
- Create: `internal/scd/normalize/tag.go`, `tag_test.go`

**Interfaces:**
- Produces (`country`): `type Info struct{ Emoji string "emoji"; ISO2 string "iso2"; NameEn string "nameEn"; NameRu string "nameRu" }`; `func FromLabel(label string) (Info, bool)` — первые две руны label = флаг из 45 кодов; `NameEn`/`NameRu` захардкожены по `01` §7 (значения Node 22: `Türkiye`, `Hong Kong SAR China`, `Czechia` и т.д.).
- Produces (`normalize`): `func InferCity(label string) (string, bool)`; `func Slugify(s string) string` (NFKD → убрать U+0300–U+036F → lower → `[^a-z0-9]+`→`-` → trim `-` → первые 32 символа); `func Sha8(s string) string`; `func BuildTag(label, address, profile, raw string) string` — `01` §6.4 (`\s` в InferCity — `unicode.IsSpace`, D9).

- [ ] **Step 1: Failing tests:** `TestFromLabelAustria` (P6), `TestFromLabelOutsideMap` (`🇽🇰` → false), `TestFromLabelLeadingSpace` → false; `TestSha8Vena` (`3ed0e2a1`); `TestBuildTagTable` — все строки таблицы `01` §6.4 (с `raw` = фиксированная строка; хэш-часть проверять через `Sha8(raw)`); `TestSameCountryDifferentCityDifferentTags` (P7); `TestTagDependsOnRawWhitespace`; `TestBuildTagRealShapes` (12 §9 п.6; профиль задаётся явно, хэш — `Sha8(raw)`): `🇩🇪DE-1-X` + `xhttp-tls` → `vless-de-de-1-x-xhttp-tls-<sha8>` (флаг без пробела, 12 §7 п.11); `🇺🇸 United States, Miami` + `tcp-tls` → `vless-us-united-states-tcp-tls-<sha8>` (город = часть до первой `,`, паритет с TS — 12 §7 п.12); `🇳🇬 Nigeria, Lagos` → `vless-xx-nigeria-tcp-tls-<sha8>` (вне карты); `🇨🇴 Colombia, Bogotá` → city-slug `colombia`; метка с суффиксом ` ^~2~^` → city-slug с `-2` (как TS, 12 §3).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** (`go get golang.org/x/text@latest`). **Step 4: Run** `go test -race ./internal/scd/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): add country map and outbound tag builder`.

---

### Task 4.3: Нормализация VLESS — scope, профиль, encryption/ML-KEM, flow, id, `allowInsecure`

**Read:** `11-xray-vless-matrix.md` §1, §3, §4.1, §5 (источник истины; §4 — с поправкой D28/O16 в начале раздела); `12-real-subscriptions.md` §7 п.4–6, §9 п.7; `07` D15, D16, D17, D18, D25, D26, D28; `09` §1.1 (строки `userinfo`, `flow`, `encryption`), §2.3; `01` §6.1–§6.3 (порядок проверок TS, тексты); README плана O4, O13, O15.

**Files:**
- Create: `internal/scd/normalize/normalize.go`, `scope.go`, `mlkem.go`, `security.go`, `transport.go`, `normalize_test.go`
- Modify: `docs/CONFIG-MIGRATION.md` (раздел «Подписки», см. Step 5)

**Interfaces:**
- Consumes: `vless.Link`, `vless.Skip` + коды (1.7), `country.FromLabel`, `BuildTag` (4.2), `github.com/xtls/xray-core/common/uuid` (`ParseString`), stdlib `crypto/ecdh`, `crypto/mlkem`.
- Produces:
  ```go
  type Policy struct{ AllowInsecure bool }
  type Warning struct{ Code string `json:"code"`; Message string `json:"message"` }  // flow_cleared | allow_insecure_ignored | param_ignored
  type Normalized struct {   // JSON-ключи и порядок: как 01 §1.2 NormalizedOutbound, новые — после shortId, все новые omitempty
      Kind, Protocol, Profile, Tag string; Line int; Raw, Label, Address string; Port int
      UUID, Encryption, Network, Security string          // Network: tcp|ws|xhttp; Security: tls|reality|none
      Flow string `json:"flow,omitempty"`; ALPN []string `json:"alpn"`
      SNI, Host, Path, Fingerprint string /*omitempty*/; AllowInsecure *bool `json:"allowInsecure,omitempty"`
      HeaderType *string `json:"headerType,omitempty"`; PublicKey, ShortID string /*omitempty*/
      SpiderX string `json:"spiderX,omitempty"`; Mldsa65Verify string `json:"mldsa65Verify,omitempty"`
      PinnedPeerCertSha256 []string `json:"pinnedPeerCertSha256,omitempty"`; VerifyPeerCertByName []string `json:"verifyPeerCertByName,omitempty"`
      ECHConfigList string `json:"echConfigList,omitempty"`; CipherSuites string `json:"cipherSuites,omitempty"`   // cs (D33, 4.4)
      HeartbeatPeriod uint32 `json:"heartbeatPeriod,omitempty"`   // ws heartbeatPeriod (D33, 4.5)
      XHTTPMode string `json:"xhttpMode,omitempty"`; XHTTPExtra json.RawMessage `json:"xhttpExtra,omitempty"`
      FinalMask json.RawMessage `json:"finalmask,omitempty"`
      Country *country.Info `json:"country,omitempty"`; City string `json:"city,omitempty"`
      Query Params `json:"query"`   // упорядоченный объект из vless.Link.Params (свой MarshalJSON)
  }
  func Normalize(l vless.Link, p Policy) (Normalized, []Warning, *vless.Skip)
  // хуки, которые наполняют задачи 4.4 и 4.5 (здесь — заглушки, возвращающие nil):
  func applySecurity(l vless.Link, n *Normalized) *vless.Skip    // security.go
  func applyTransport(l vless.Link, n *Normalized) *vless.Skip   // transport.go
  ```
- Порядок проверок и тексты (reason — дословно). Принцип D26: здесь проверяется только то, что не может проверить `conf.Build()` (11 §5 п.1); остальное отвергает Xray в манифесте (4.7).
  1. **Scope/профиль** (`scope.go`): `network = lower(type||"tcp")`, `raw`→`tcp`; `security = lower(security)`, `""`→`none`. `unsupported_combo` «Unsupported VLESS type/security/flow combination.» details `["type=<raw>","security=<raw>","flow=<raw>"]` (значения как в ссылке, TS-паритет), если: `network ∉ {tcp, ws, xhttp}` (grpc/httpupgrade/kcp/… — вне scope D26); `security ∉ {tls, reality, none}`; `ws+reality` (Xray: «REALITY only supports RAW, XHTTP and gRPC»); `security=none` и encryption не ML-KEM (11 §3 #15 — проверять здесь: conf пропускает приватные/`.test`-адреса).
  2. **encryption**: `""`/`none` → `none`; префикс `mlkem768x25519plus.` → строка как есть; иначе `unsupported_value` «Unsupported VLESS encryption value.» `["encryption=<v>"]` (регистр важен: `None` → skip, как conf).
  3. **Ключи ML-KEM** (`mlkem.go`, `func validateMLKEM(enc string) error`): сегменты после `<mode>.<rtt>.` короче 20 символов — padding (не проверять); остальные — `base64.RawURLEncoding`; 32 байта → `ecdh.X25519().NewPublicKey(b)`, 1184 байта → `mlkem.NewEncapsulationKey768(b)`; иная длина или ошибка → `unsupported_value` «Invalid VLESS encryption key.» (D25) `["encryption=<первые 40 символов>…"]`. Формат `<mode>`/`<rtt>` не проверять (это делает conf). Единственный случай «conf ✓, live ✗» (11 §4 строка `encryption`; Review Focus 6).
  4. **id**: `uuid.ParseString(id)` ошибка → `unsupported_value` «The VLESS user id is not accepted by Xray.» (O4); `UUID = u.String()`.
  5. **flow**: ∉ `{"", xtls-rprx-vision, xtls-rprx-vision-udp443}` → `unsupported_value` «Unsupported VLESS flow value.» `["flow=<v>"]`. Vision допустим, если (`network==tcp` и `security∈{tls,reality}`) **или** encryption ML-KEM; иначе `Flow=""` + warning `flow_cleared` «flow <v> is not supported on <network>+<security>; flow cleared.» (D16; 11 §3 #6, #10, #12).
  6. **Профиль** (11 §3 «Профили и теги»): `<network>-<securityName>` где `securityName = security`, а для `none`+ML-KEM — `mlkem`; суффикс `-vision` только при `security==reality` и непустом (сохранённом) `Flow`. Итог: `tcp-tls` (в т.ч. tls+vision), `ws-tls`, `xhttp-tls`, `tcp-reality`, `tcp-reality-vision`, `xhttp-reality`, `xhttp-reality-vision`, `tcp-mlkem`, `ws-mlkem`, `xhttp-mlkem`. Старые три профиля TS и их теги не меняются.
  7. **`allowInsecure`** (D15): значение `0|1|true|false` (без учёта регистра), иначе `unsupported_value` «allowInsecure must be 0, 1, true, or false.» `["allowInsecure=<v>"]`; если true и нет непустых `pcs`/`vcn`: `Policy.AllowInsecure==false` → `unsupported_value` «allowInsecure=1 requires pcs or vcn: Xray v26.10.10 no longer supports allowInsecure (set subscriptions[].allowInsecure: allow to keep such links).»; иначе warning `allow_insecure_ignored` «allowInsecure is not supported by Xray v26.10.10; the outbound will verify certificates (use pcs/vcn).». В JSON/proto не передаётся.
  8. `applySecurity`, затем `applyTransport` (их skip возвращается как есть).
  9. Параметры, не относящиеся к выбранным transport/security (например `alpn`/`ech`/`pcs`/`vcn` при reality, `pbk`/`sid`/`spx`/`pqv` при tls, `host`/`path` у tcp без `headerType=http`, `serviceName`/`authority`/`seed`/`mtu`/`tti`, `support-x25519mlkem768`, `x_padding_bytes`) **молча игнорируются** — как это делают Xray и TS для `path` у tcp; они остаются только в `Query` (README плана O16, O13).
  10. Tag — `BuildTag(label, address, profile, raw)`.
  11. **Неизвестные параметры** (D28): непустой `l.Ignored` → warning `param_ignored` «Ignored unknown VLESS parameter(s): a, b.» (порядок `Ignored`); ссылка не отбрасывается. Это **единственный** источник `param_ignored`: за неприменимые параметры п.9 warning нет.

- [ ] **Step 1: Failing tests** (`normalize_test.go`, table-driven; ссылки — из `testdata/reference/scd/corpus/d26-links.txt` (0.5) и `vless-corpus.txt` (0.2)):
  - профили (по строке на каждую ✓-строку 11 §3): `TestProfileMatrix` — `type=tcp|raw|ws|xhttp` × `security=tls|reality|none` × encryption none/ML-KEM × flow → ожидаемый профиль из списка п.6; `tcp+tls+vision` → `tcp-tls` с `Flow` сохранённым; `tcp+reality` без flow → `tcp-reality` (бывший O1).
  - combo: `type=grpc&security=tls` (P19) и `type=httpupgrade`, `type=kcp` → details `["type=grpc","security=tls","flow="]`; `ws+reality` → combo; `tcp` без security и без ML-KEM → `["type=tcp","security=","flow="]`; `security=xtls` → combo.
  - encryption: `encryption=None` → unsupported_value; ML-KEM при `security=none` → `tcp-mlkem`, `Encryption` = исходная строка; `TestMLKEMX25519KeyAccepted`, `TestMLKEMKeyAccepted` (ключи из `vlessenc`, 0.5); `TestMLKEMInvalidKeySkipped` (1184 случайных байта → «Invalid VLESS encryption key.»); `TestMLKEMPaddingSegmentsNotValidated` (`…native.0rtt.100-111-1111.75-0-111.<key>` → ok).
  - flow: `TestVisionClearedOnWSTLS`, `TestVisionClearedOnXHTTPReality` (warning `flow_cleared`), `TestVisionKeptWithMLKEMOnWS`, `TestVisionUdp443Kept`, `xtls-rprx-bogus` → skip.
  - id: `my-user` → `uuid.ParseString("my-user").String()`; uppercase UUID → lowercase; 33-символьный не-hex → skip (O4).
  - allowInsecure: `allowInsecure=` → unsupported_value; `1` без pcs при deny → skip; при allow → ok + `allow_insecure_ignored`; `1` + `vcn` → ok без warning.
  - `TestIrrelevantParamsIgnored`: reality-ссылка с `alpn=h2&ech=AAAA&pcs=…&cs=TLS_AES_128_GCM_SHA256&support-x25519mlkem768=true` → entry **без warnings**, поля `ALPN`/`ECHConfigList`/`PinnedPeerCertSha256`/`CipherSuites` пусты, `Query` содержит все ключи; tcp+tls с `path=%2F` без `headerType` (форма `provider2.txt`) → entry, `warnings == nil`, `Path == ""` (12 §7 п.4).
  - `TestUnknownParamWarning` (D28): ссылка с `foo=1&bar=2` → entry, ровно один warning `{param_ignored, "Ignored unknown VLESS parameter(s): foo, bar."}`.
  - `TestMLKEMNative0RTTRealShapeKey` (12 §7 п.5, §9 п.7): `encryption` строки 1 `docker/fixtures/real-shapes/remnawave.txt` (`native.0rtt`, один ключ 1184 байта) → `validateMLKEM` == nil.
- [ ] **Step 2: Run** `go test ./internal/scd/normalize/` → FAIL.
- [ ] **Step 3: Implement** (`go get github.com/xtls/xray-core@701af60772cda123492f86e383e1fdba066614a9`; проверить, что в `go.mod` строка ровно `v1.260327.1-0.20261010092107-701af60772cd`; добавить комментарий из Глобальных ограничений).
- [ ] **Step 4: Run** `go test -race ./internal/scd/... && golangci-lint run` → PASS.
- [ ] **Step 5: Commit** `feat(scd): normalize VLESS scope, profiles, encryption and flow for Xray v26.10.10` (body: D15–D18, D25, D26, D28, O4) — вместе с разделом «Подписки» `docs/CONFIG-MIGRATION.md` (D32; ревью-2 F05): политика `allowInsecure` (D15), неизвестные параметры → `param_ignored` вместо skip (D28), новые профили и их теги (D26, 11 §3), сброс `flow` с warning (D16), id без строгой UUID-проверки (D18), ws без `path` и REALITY без `sni/fp/sid` принимаются (O15).

---

### Task 4.4: Нормализация: параметры TLS и REALITY

**Read:** `11-xray-vless-matrix.md` §4 (строки `sni`, `fp`, `alpn`, `pcs`, `vcn`, `ech`, `pbk`, `sid`, `spx`, `pqv`); `12-real-subscriptions.md` §3 (строка `cs`); `09` §1.1, §1.4, §2.3; `07` D13, D17, D26, D33.

**Files:**
- Modify: `internal/scd/normalize/security.go`; Create: `internal/scd/normalize/security_test.go`

**Interfaces:**
- Consumes: `Normalized`, `applySecurity` (4.3).
- Produces: тело `applySecurity` — заполняет поля `Normalized` для выбранного `Security` (для `none` ничего не делает). Значения **не** валидируются здесь сверх перечисленного — неверные значения отвергает `conf.Build()` в 4.7 (11 §5).
  - `tls`: `SNI = sni`; `Fingerprint = lower(fp)`; `ALPN` = split `,`, trim, без пустых; `PinnedPeerCertSha256` = split `pcs` по `,`, trim, без пустых (строки как есть — `:` и регистр разбирает conf); `VerifyPeerCertByName` = split `vcn`; `ECHConfigList = ech` как есть (base64 или DNS-форма `name+https://…`; Xray не проверяет — 11 §4); `CipherSuites = cs` как есть (D33; строка имён через `:`, как `tlsSettings.cipherSuites`; conf и `AddOutbound` значение не проверяют — 12 §3, поэтому и normalize не проверяет). При `reality` `cs` молча игнорируется (O16).
  - `reality`: пустой `pbk` → `missing_required_field` `REALITY VLESS requires parameter "pbk".` (**только pbk**: `sni`, `fp`, `sid` Xray принимает пустыми — 11 §4; отличие от TS — README плана O15); `PublicKey` = pbk без `=`, `+`→`-`, `/`→`_` (conf принимает только base64url без паддинга — 11 §4 `pbk std b64 padded` ✗); `ShortID = sid` (пустой допустим, D17); `SpiderX = spx`, если пусто — `"/"` (D13); `Mldsa65Verify = pqv`; `SNI`, `Fingerprint = lower(fp)`.

- [ ] **Step 1: Failing tests** (`security_test.go`): `TestTLSFields` (`sni=front.example.test&fp=Firefox&alpn=h2,%20http/1.1,,` → `firefox`, `["h2","http/1.1"]`), `TestTLSPinsAndNames` (два `pcs` через `,` в формате OpenSSL и hex → 2 строки; `vcn=a.example.test, b.example.test` → 2), `TestTLSECHKeptVerbatim` (base64 из 0.5 и `ech.example.test+https://1.1.1.1/dns-query`), `TestRealityRequiresOnlyPbk` (без `pbk` → `missing_required_field`; без `sni`/`fp`/`sid` → ok), `TestRealityPbkNormalized` (std-base64 с `=` → base64url без паддинга), `TestRealityDefaultSpiderX` (`"/"`), `TestRealitySpxKept` (`/abc?p=10-20&c=2`), `TestRealityPqvKept` (pqv из 0.5), `TestNoneSecurityLeavesFieldsEmpty` (ML-KEM-ссылка с `sni=x`), `TestTLSCipherSuitesKept` (D33: `cs=TLS_AES_128_GCM_SHA256%3ATLS_CHACHA20_POLY1305_SHA256` → `CipherSuites=="TLS_AES_128_GCM_SHA256:TLS_CHACHA20_POLY1305_SHA256"`, warnings пусты; на reality → `CipherSuites==""`, warnings пусты).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/normalize/ && golangci-lint run` → PASS.
- [ ] **Step 5: Commit** `feat(scd): map TLS and REALITY share-link parameters` (body: D13, D17, D26, D33).

---

### Task 4.5: Нормализация: параметры транспорта (tcp `headerType`, ws, xhttp) и `fm`

**Read:** `11-xray-vless-matrix.md` §4 (строки `headerType`, `host`, `path`, `mode`, `extra`, `fm`); `12-real-subscriptions.md` §2 (ws `heartbeatPeriod`), §3, §4.1–§4.2, §7 п.1–8, §9 п.7; `09` §1.1 (строки `path`, `host`), §3.2; `07` D26, D28, D33.

**Files:**
- Modify: `internal/scd/normalize/transport.go`; Create: `internal/scd/normalize/transport_test.go`

**Interfaces:**
- Consumes: `Normalized`, `applyTransport` (4.3).
- Produces: тело `applyTransport`:
  - `tcp`: `HeaderType` = указатель на значение `headerType`, если параметр присутствует (как TS: `""` и `none` сохраняются); при `http` — `Host = host`, `Path = path` (строки как в ссылке; список через `,` разбивает билдер 4.6). Прочие значения не проверять (conf: `unknown config id`).
  - `ws`: `Host = host`, `Path = path` (пустой `path` допустим — Xray принимает; `?ed=` остаётся в строке — его вынимает conf; отличие от TS «WebSocket VLESS requires a path.» — README плана O15); `heartbeatPeriod` (D33) → `HeartbeatPeriod` через `strconv.ParseUint(v, 10, 32)`; ошибка → `unsupported_value` «ws heartbeatPeriod must be a non-negative integer.» `["heartbeatPeriod=<v>"]` (проверка здесь: conf делает `json.Unmarshal` в `uint32`, нечисло сломало бы весь JSON-outbound, 12 §3). На tcp/xhttp `heartbeatPeriod` молча игнорируется (O16).
  - `xhttp`: `Host`, `Path`, `XHTTPMode = mode` (значение не проверять — conf отвергает неизвестные); `extra` → `XHTTPExtra`: `json.Valid` и первый непробельный символ `{`, иначе `unsupported_value` «xhttp extra must be a JSON object.» `["extra=<первые 40 символов>"]`.
  - любой транспорт: `fm` → `FinalMask` с той же проверкой, иначе «fm (finalmask) must be a JSON object.» `["fm=<первые 40 символов>"]`. Причина проверки здесь: `extra`/`fm` встраиваются в JSON-outbound как `json.RawMessage`, невалидный JSON сломал бы `json.Marshal`.

- [ ] **Step 1: Failing tests** (`transport_test.go`): `TestTCPHeaderNoneKept` (`headerType=none` → `*HeaderType=="none"`; `headerType=` → `""`), `TestTCPHeaderHTTP` (`headerType=http&host=h1.example.test,h2.example.test&path=/a,/b`), `TestWSHostPathEd` (`path=%2Fws%3Fed%3D2048` → `Path="/ws?ed=2048"`), `TestWSWithoutPathAccepted`, `TestXHTTPFields` (path/host/mode/extra), `TestXHTTPBadExtra` (`extra=%5B1%5D` и `extra=%7Bx`) → skip, `TestFinalMaskKept` (`fm=` URL-encoded `{"tcp":[{"type":"fragment",…}]}`), `TestFinalMaskInvalid` → skip, `TestWSHeartbeatPeriod` (D33: `heartbeatPeriod=30` → `30`; `abc`, `-1`, `4294967296` → skip `unsupported_value` с текстом выше; на xhttp → поле 0, warnings пусты).
  - `TestNormalizeRealShapes` (12 §9 п.7; полный `Normalize(Policy{})` по всем строкам фикстур): `remnawave.txt` (4) → профиль `xhttp-tls`, `XHTTPMode=="stream-up"`, `XHTTPExtra` побайтно = декодированный `extra`, `ALPN==["h2"]`, `Fingerprint=="chrome"`, `Encryption` начинается с `mlkem768x25519plus.native.0rtt.`, warnings пусты; `provider2.txt` и `provider2.b64` (после `inputsrc.DecodeAuto`; 50) → `tcp-tls`, `SNI==""`, `Encryption=="none"`, `ALPN==["h2"]`, `HeaderType==nil`, `Path==""`, **warnings пусты** (нет `param_ignored` за `path`).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/normalize/ && golangci-lint run` → PASS.
- [ ] **Step 5: Commit** `feat(scd): map transport parameters (raw http header, ws, xhttp, finalmask)` (body: D26, D28, D33).

---

### Task 4.6: JSON outbound v26.10.10, `ToProto` через `infra/conf`, проверки параметров

**Read:** `07` D6, D13, D19, D26, D33; `11-xray-vless-matrix.md` §4 (колонка «JSON-ключ», «Ошибка Xray»); `12-real-subscriptions.md` §3 (`cs`, `heartbeatPeriod`), §5 (что проверено в proto для реальных форм), §9 п.8; §2 (программа-эксперимент `scratchpad/plan-update/matrix/main.go` — образец сборки и печати); `09` §1.2, §1.4, §6 (эталонные структуры); `02` §3, §6 п.3; `08` §2.1.

**Files:**
- Create: `internal/scd/outbound/json.go`, `proto.go`, `override.go`, `outbound_test.go`, `proto_golden_test.go`
- Create: `internal/scd/outbound/paramcheck/paramcheck.go`, `paramcheck_test.go` (обычный пакет, импортируется только тестами 4.6 и 5.6)

**Interfaces:**
- Consumes: `normalize.Normalized`.
- Produces:
  - `type Outbound struct{…}` (порядок полей структуры = порядок ключей) в форме:
    `{"tag","protocol":"vless","settings":{"vnext":[{"address","port","users":[{"id","encryption","flow"?,"email":<tag>}]}]},"streamSettings":{"network":"tcp|ws|xhttp","security":"tls|reality|none", "tlsSettings":{"serverName"?,"alpn"?,"fingerprint"?,"pinnedPeerCertSha256"?(строка через ","),"verifyPeerCertByName"?(строка через ","),"echConfigList"?,"cipherSuites"?}, "realitySettings":{"serverName"?,"fingerprint"?,"password","shortId","spiderX","mldsa65Verify"?}, "tcpSettings":{"header":{"type":"http","request":{"path"?:[…],"headers"?:{"Host":[…]}}}}, "wsSettings":{"path"?,"host"?,"heartbeatPeriod"?(число)}, "xhttpSettings":{"path"?,"host"?,"mode"?,"extra"?}, "finalmask"?}}`
    Правила: `tlsSettings` присутствует всегда при tls (может быть `{}`); секции только для своего network/security; `tcpSettings` — только при `headerType=http` (`path` = split `Path` по `,`, `headers.Host` = split `Host` по `,`, пустые — ключ не писать: тогда Xray подставит свои дефолты; заданные `headers` заменяют весь набор по умолчанию — 11 §4); `wsSettings` пишется всегда для ws (пустой объект допустим); `extra`/`finalmask` — `json.RawMessage` как есть; `encryption` — `none` или ML-KEM-строка.
  - `func Build(n normalize.Normalized) Outbound`
  - `func ToProto(o Outbound) (*core.OutboundHandlerConfig, error)` — `json.Marshal(o)` → `json.Unmarshal` в `conf.OutboundDetourConfig` → `.Build()`; ошибка оборачивается `fmt.Errorf("xray rejected outbound %s: %w", tag, err)`.
  - `func Hash(list []Outbound) string` — `sha256` hex от `json.Marshal(list)`.
  - `func ForTarget(n normalize.Normalized, tag string, visionUdp443Override bool) normalize.Normalized` — копия с новым `Tag` и `Flow = xtls-rprx-vision-udp443`, если override и `Security=="reality"` и `Flow=="xtls-rprx-vision"` (`01` §12.1).
  - (`paramcheck`) `func Check(param, value string, h *core.OutboundHandlerConfig) error` — «параметр ссылки `param` со значением `value` присутствует в proto `h`» (адресная проверка поля, а не только `proto.Equal`); `var Covered []string` — параметры, для которых есть проверка (22): `type, security, encryption, flow, sni, fp, alpn, pcs, vcn, ech, cs, pbk, sid, spx, pqv, headertype, host, path, heartbeatperiod, mode, extra, fm`. Соответствие поле↔параметр — 11 §4 (пример: `sni` → `tls.Config.ServerName` или `reality.Config.ServerName`; `pcs` → `len(PinnedPeerCertSha256)` = числу значений и каждый 32 байта; `ech` → `EchConfigList == value`; `cs` → `tls.Config.CipherSuites == value` (D33); `heartbeatperiod` → `websocket.Config.HeartbeatPeriod == uint32(value)` (D33); `spx` → `SpiderX` без `p,c,t,i,r` и `SpiderY` из них; `pqv` → `len(Mldsa65Verify)==1952`; `headertype=http` → `tcp.Config.HeaderSettings` типа `xray.transport.internet.headers.http.Config` с `Uri`/`Host` из ссылки; `mode` → `splithttp.Config.Mode`; `extra` → хотя бы одно поле `splithttp.Config` из extra (например `XPaddingBytes`); `fm` → `len(Tcpmasks)+len(Udpmasks)>0` или `QuicParams!=nil`; `encryption` ML-KEM → `Account.Encryption` = ключи без префикса, `XorMode`/`Seconds`/`Padding` по 11 §4).
  - Подавление логов Xray — **обязательно** (ревью B1): `common/log` в `init()` регистрирует логгер в **stdout** (`common/log/logger.go:186`), а `conf.Build()` для ws печатает `[Warning] … WebSocket transport … is deprecated…`. В `proto.go` `init()` вызвать `xlog.RegisterHandler(h)` (`xlog "github.com/xtls/xray-core/common/log"`), `h` отбрасывает сообщения. Проверено ревью и экспериментом 11 §2.
- Pitfalls: `conf` требует `"encryption":"none"` явно; `password` (а не `publicKey`) для REALITY; адрес IP → `ip`, домен → `domain` (conf); `security:"none"` без ML-KEM conf отвергает только для публичных адресов — но такие ссылки сюда не доходят (4.3 п.1).

- [ ] **Step 1: Failing tests (`outbound_test.go`):** `TestBuildJSONTcpTLS`/`WsTLS`/`Reality` — `json.Marshal(Build(n))` равен эталонной строке 09 §6.1–§6.3 (с добавленным `email`); `TestBuildJSONWsTLS`/`Reality` + `proto_golden_test.go` — порт P21/P22 (`01` §17.1); `TestBuildJSONOmitsAllowInsecure`; `TestBuildJSONHeaderHTTP` (`tcpSettings.header.request.path==["/a","/b"]`, `headers.Host==["h1.example.test","h2.example.test"]`); `TestBuildJSONXHTTPExtraAndFinalMaskRaw` (байты `extra`/`finalmask` вставлены без изменений); `TestBuildJSONNoneSecurityMLKEM` (нет `tlsSettings`/`realitySettings`); `TestHashStableForSameInput`; `TestForTargetUdp443OnlyReality` (`01` §17.2 T2-логика).
- [ ] **Step 2: Failing tests (`proto_golden_test.go`, `paramcheck_test.go`)** — «каждый параметр доходит до proto» (D6, D19, D26): для **каждой** `entry`-строки `testdata/reference/scd/corpus/d26-links.txt` (через `vless.Parse` → `normalize.Normalize(Policy{AllowInsecure:true})` → `Build` → `ToProto`): ошибок нет; для каждого параметра из поля `params` этой строки в `d26-expect.json` — `paramcheck.Check(param, value, h) == nil` (`value` — значение параметра в ссылке). Плюс:
  - `TestCorpusCoversEveryParam`: каждый элемент `Covered` встречается в `params` хотя бы одной `entry`-строки корпуса, каждый элемент `params` есть в `Covered`, а каждая ✓-строка матрицы 11 §3 — хотя бы одной строкой (по профилю + наличию ML-KEM/flow).
  - `TestRealityAlwaysHasSpiderY` (регресс D13): каждая reality-entry → `len(SpiderY)==10`; пустой `sid` → `ShortId` 8 нулевых байт.
  - `TestXrayRejectsLikeMatrix`: для каждой `skip`-строки корпуса с `reasonCode=="unsupported_value"` и `stage=="xray"` (поле из `d26-expect.json`) — `ToProto` возвращает ошибку, содержащую `expectContains` (тексты Xray из 11 §4: `invalid "shortId"`, `incorrect pinnedPeerCertSha256 length`, `unknown "fingerprint"`, `invalid "fingerprint": unsafe`, `invalid "spiderX"`, `invalid "mldsa65Verify"`, `unsupported mode`, `unsupported padding placement`, `"headers" can't contain "host"`, `unknown config id`, `invalid "password"`).
  - `TestMLKEMAccountTransform`: `xorpub.1rtt` → `XorMode==1, Seconds==0`; `random.0rtt` → `2, 1`; padding-сегменты → `Account.Padding`.
  - `TestToProtoRealShapes` (12 §9 п.8): строка `remnawave.txt` → `vless.Account`: `XorMode==0`, `Seconds==1`, `Padding==""`; `splithttp.Config`: `Mode=="stream-up"`, `Xmux` = диапазоны из `extra` (`MaxConcurrency` 8–16, `HMaxRequestTimes` 600–900, `HMaxReusableSecs` 1800–3000, нули — нули), `XPaddingBytes` 100–1000, `NoGRPCHeader==false`; `tls.Config.NextProtocol==[h2]`. Строка `provider2.txt` → у `tcp.Config` нет `HeaderSettings`, `tls.Config.ServerName==""`, `NextProtocol==[h2]`, `Account.Encryption=="none"`.
  - `TestToProtoIsSilent`: перехватить `os.Stdout`/`os.Stderr` (pipe) и вызвать `ToProto` для ws-tls → в pipe пусто.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/scd/... && golangci-lint run` → PASS; `go build -o bin/scd ./cmd/scd && ls -la bin/scd` (ожидаемо ~50 МБ без `-s -w`; ~35 МБ с `-ldflags="-s -w"`; замер прототипа ревью: 50.8 / 34.7 МБ).
- [ ] **Step 6: Commit** `feat(scd): build v26.10.10 JSON outbounds and protobuf via infra/conf` (body: D6, D13, D19, D26, D33).

---

### Task 4.7: Манифест, фильтры, загрузка подписок и `scd generate`

> **Поправка D29:** фильтры манифеста применяют `labelfilter.Filter.Match` (include AND, exclude OR) вместо одного регулярного выражения.

**Read:** `01` §8, §9, §10, §13, §14.1–§14.2, §16, §17.1 (P1–P5, P7–P10, P19–P20, P25), §17.3 S15; `07` D8, D9, D23, D26, D27 (O9), D28, D29, D33; `11-xray-vless-matrix.md` §5; `12-real-subscriptions.md` §5, §6.1, §9 п.8, §10 п.3; `testdata/reference/scd/generate/*` (вкл. `real-shapes-*`, 0.2), `testdata/reference/scd/corpus/ts-parse.json`, `d26-expect.json`.

**Files:**
- Create: `internal/scd/manifest/manifest.go`, `filter.go`, `subscriptions.go`, `manifest_test.go`, `filter_test.go`, `subscriptions_test.go`
- Create: `internal/scd/cli/generate.go`, `generate_test.go`
- Modify: `internal/scd/cli/cli.go` (`Commands()` + `generate`)

**Interfaces:**
- Consumes: `subscription.ScanLines`, `vless.Parse`, `normalize.Normalize`, `outbound.Build/ToProto`, `inputsrc.Load` (+ `Options.UserAgent`, `Options.Headers`, 1.6), `config.*` (+ `SubscriptionConfig.FetchUserAgent`, `SubscriptionConfig.FetchHeaders`, 4.1).
- Produces:
  ```go
  type Entry struct { Kind, Tag, Label, Profile string; Line int; Country *country.Info; City string
      Normalized normalize.Normalized; JSONOutbound outbound.Outbound; Warnings []normalize.Warning }
      // json: kind, tag, label, profile, line, country?, city?, normalized, jsonOutbound, warnings?
  type Summary struct{ /* 13 счётчиков 01 §1.2 ManifestSummary, те же json-имена */ }
  type Manifest struct { Version int; GeneratedAt, SourceFile string; Entries []Entry; Skipped []vless.Skip; Summary Summary }
  func Build(text, sourceFile string, p normalize.Policy, now time.Time) Manifest
  func ApplyFilters(m Manifest, f *config.SubscriptionFiltersConfig) Manifest
  func Validate(m Manifest, sourceID string) error   // ErrNoValidEntries, текст 01 §10
  type ParseLog struct{ GeneratedAt string; Summary Summary; Skipped []vless.Skip }
  type LoadedSubscription struct{ Config config.SubscriptionConfig; Source, Content, Encoding string }
  type FailedSubscription struct{ Config config.SubscriptionConfig; Error string }
  func LoadSubscriptions(ctx context.Context, subs []config.SubscriptionConfig, load func(ctx context.Context, input string, o inputsrc.Options) (inputsrc.Loaded, error)) ([]LoadedSubscription, []FailedSubscription)
  func FromSubscription(s LoadedSubscription, now time.Time) (Manifest, error)  // Build(strings.TrimSpace(content), id, PolicyFor(s.Config), now) → filters → Validate
  func PolicyFor(c config.SubscriptionConfig) normalize.Policy   // Policy{AllowInsecure: c.AllowInsecure == "allow"} (D15) — используют sync (6.1) и runtime-state expected (6.4)
  ```
- `Build`: алгоритм `01` §8; после нормализации `outbound.ToProto(outbound.Build(n))` — ошибка → skip `unsupported_value` «Xray rejected the outbound configuration.» details `[err.Error()]` (D19, D26: это **основной** механизм отказа по значениям параметров — 11 §5); дубль тега → `duplicate_tag` `Duplicate generated outbound tag "<tag>".`. `ApplyFilters`: сначала `countryAllowlist` (пустой — выключен), затем `labelfilter.Match`; `filteredByLabelRegex` считает **все** отброшенные label-фильтром entries (не прошёл include **или** совпал exclude — одним счётчиком, имя поля JSON не меняется, D29; ревью-2 F11); `parsed = len(entries)` после фильтров (01 §9). `Summary.UnsupportedParam` остаётся в JSON (форма манифеста и текст `ErrNoValidEntries` 01 §10 не меняются), но в Go всегда 0 — код `unsupported_param` больше не выдаётся (D28). `LoadSubscriptions`: только enabled, сортировка `strings.Compare` по id, затем input (D23), последовательно; `inputsrc.Options{Format: sub.Format, FetchTimeout: sub.FetchTimeoutMs мс}` (ревью-2 F12), `inputsrc.Options.UserAgent = sub.FetchUserAgent` (пусто → дефолт `scd/<version>` внутри `inputsrc`, O9), `inputsrc.Options.Headers = sub.FetchHeaders` (D33). `FailedSubscription.Error` — текст ошибки `inputsrc` (значений заголовков в нём нет, 1.6). `GeneratedAt` — `now.UTC().Format("2006-01-02T15:04:05.000Z")`.
- `scd generate --input --output --log` (`01` §14.1): `inputsrc.Load` (auto, 5s, UA по умолчанию), `Build(content, source, Policy{})` (без trim и фильтров), `Validate`, запись двух файлов (`jsonout.Write`), stdout — summary; при ошибке файлы не создаются.

- [ ] **Step 1: Failing tests (`manifest_test.go`):** P1/P2 — фикстура `testdata/reference/scd/fixtures/vpn-sample.txt` содержит 2 ws-строки с `allowInsecure=1` без `pcs`/`vcn` (строки 9–10): `TestBuildVpnSampleAllowPolicy` (`Policy{AllowInsecure: true}` → профили tcp-tls, ws-tls, tcp-reality-vision; parsed==len(entries)==12; у двух ws-entries warning `allow_insecure_ignored`) и `TestBuildVpnSampleDenyPolicy` (`Policy{}` → 10 entries без ws-tls, 2 skipped `unsupported_value` с текстом про `allowInsecure=1 requires pcs or vcn`), P7, P8 (дубль → duplicate_tag), P9, P10 (изм., D28: `foo=bar` ⇒ **entry** с одним warning `param_ignored`, `summary.unsupportedParam==0`), P19, P20, `TestBuildManifestCRLFEqualsLF` (Review Focus 3), `TestBuildSkipsXrayRejected` (reality `sid=abc` → `unsupported_value` «Xray rejected the outbound configuration.», details содержит `invalid "shortId"`), `TestBuildD26Corpus` (корпус policy-инвариантен, 0.5: запускается **дважды** — `Policy{}` и `Policy{AllowInsecure:true}`; для каждой строки `d26-links.txt` результат совпадает с `d26-expect.json`: entry/skip, `profile`, `reasonCode`, подстрока `reason`/`details`, коды warnings), `TestBuild3xuiRealityLink` (Review Focus 7: строка 3x-ui с `spx`, `pqv`, `support-x25519mlkem768=true`, `flow=xtls-rprx-vision` → entry `tcp-reality-vision`), `TestBuildRealShapeProvider2MatchesTS` (12 §9 п.8: `Build` по декодированному `provider2.b64` с `Policy{}` → множество тегов и `summary` = `testdata/reference/scd/generate/real-shapes-provider2.manifest.json` (50/50 тегов побайтно); затем `ApplyFilters` с фильтрами из `docker/scd/config.real-shape.go.yml` (allowlist 24 кода + `labelExcludeRegex: '/Extra|WhiteList/i'`) → 25 entries, `filteredByCountry==25`, `filteredByLabelRegex==0`), `TestBuildRealShapeRemnawave` (4 entries `xhttp-tls`, skipped пусто — TS: 0 parsed (`real-shapes-remnawave.error.txt`), намеренное расхождение D26/D28; те же фильтры оставляют 4), `TestBuildLabelWithServerDescription` (12 §10 п.3: label `🇩🇪 Berlin?serverDescription=QUJD` (так Remnawave пишет его extended-клиентам) → entry, `City`/тег содержат slug `berlin-serverdescription-qujd` — поведение как у TS, фиксируется тестом, не чинится), `TestBuildCountsSummary`, `TestBuildMatchesTSForLegacyProfiles` — для `testdata/reference/scd/generate/vpn-sample.manifest.json`: множество тегов entries совпадает, `skipped[].reasonCode` по номерам строк совпадают (кроме allowlist расхождений с комментарием: ws-ссылки с `allowInsecure=1` → skip при deny, D15; бывшие `unsupported_param`/`missing_required_field` строки, ставшие entries по D26).
- [ ] **Step 2: Failing tests (`filter_test.go`):** P3, P4 (изм.: regex `/,\s*Extra\s*$/i`; ожидание — `Extra Whitelist#2` отфильтрован), P5, `TestEmptyAllowlistDisablesFilter`, `TestExcludeCountsAsLabelFiltered` (include `/DE/` + exclude `/Extra/i` на трёх метках `DE`, `DE Extra`, `NL` → 1 entry, `filteredByLabelRegex==2`).
- [ ] **Step 3: Failing tests (`subscriptions_test.go`):** S15, `TestLoadSubscriptionsContinuesAfterFailure`, `TestLoadSubscriptionsPassesUserAgent` (фейковый `load` видит `o.UserAgent == "Happ/1.0"` из конфига и `""` без ключа), `TestLoadSubscriptionsPassesFetchHeaders` (D33: `o.Headers.Plain()["x-hwid"]=="s3cr3t"`; при ошибке загрузки `FailedSubscription.Error` не содержит `s3cr3t`), `TestValidateNoValidEntriesMessage` (точный текст `01` §10 со всеми счётчиками), `TestFromSubscriptionTrimsContent`, `TestFromSubscriptionUsesAllowInsecurePolicy` (ws-строка `allowInsecure=1` без pcs/vcn: `allowInsecure: allow` → entry с warning, default `deny` → skip), `TestLoadSubscriptionsPassesFormatAndTimeout`.
- [ ] **Step 4: Failing tests (`generate_test.go`):** P25 (`trojan://example\n` → ошибка `produced no valid outbound entries`, файлы не созданы), `TestGenerateDockerFixture` (summary совпадает с `testdata/reference/scd/generate/docker-fixture.summary.json`).
- [ ] **Step 5: Run** → FAIL. **Step 6: Implement.**
- [ ] **Step 7: Verify** `go test -race ./internal/scd/... && go build -o bin/scd ./cmd/scd && ./bin/scd generate --help >/dev/null && ./bin/scd generate --input docker/fixtures/subscription.txt --output /tmp/m.json --log /tmp/l.json | jq .parsed` → `8` (`jq` работает только если stdout не засорён логами Xray — см. 4.6 «Подавление логов»); `./bin/scd generate --input testdata/reference/scd/corpus/d26-links.txt --output /tmp/m.json --log /tmp/l.json | jq .parsed` = числу `entry` в `d26-expect.json`; `./bin/scd generate --input docker/fixtures/real-shapes/provider2.b64 --output /tmp/m.json --log /tmp/l.json | jq .parsed` → `50`; то же для `remnawave.txt` → `4`.
- [ ] **Step 8: Commit** `feat(scd): port manifest building, filters and generate command` (body: D8, D19, D23, D26, D28, D29, D33, O9).

---

### Task 4.8: Топология туннелей target'а

**Read:** `01` §1.3 (TunnelMapping), §12.2, §16 п.17–18, §17.2 T1–T3; `07` D21, D23.

**Files:**
- Create: `internal/scd/topology/topology.go`, `topology_test.go`

**Interfaces:**
- Consumes: `manifest.Manifest`, `config.SubscriptionTargetConfig`, `outbound.ForTarget`, `outbound.Build`.
- Produces:
  ```go
  type PreparedOutbound struct{ Tag string; Normalized normalize.Normalized; JSON outbound.Outbound }
  type Tunnel struct { BaseTunnelID, DisplayName, CountryISO2, BaseOutboundTag, OutboundTagInitial, OutboundTagCurrent, InboundTag, RouteTag, Listen string; Port int
      OutboundInitial, OutboundWithoutPrefix PreparedOutbound }
  type Topology struct{ Tunnels []Tunnel }
  func Build(m manifest.Manifest, t config.SubscriptionTargetConfig) (Topology, error)
  var ErrPortRangeExhausted   // текст: target "<addr>" inboundSocks.portRange can allocate only <N> ports, but <M> tunnels were generated
  ```
  Без `inboundSocks` → пустая topology. Сортировка `strings.Compare` по тегу (затем line). Порт = `start + i`; `in-<tag>`, `route-<tag>`; prefix только у outbound-тега.

- [ ] **Step 1: Failing tests:** T1 (порты 20000/20001, prefix, `in-`/`route-`), T2 (override в обоих outbound для reality; tcp-tls не трогается), T3 (`errors.Is(err, ErrPortRangeExhausted)`, текст содержит `can allocate only 1 ports, but 2 tunnels`), `TestNoInboundSocksEmpty`, `TestPortsFollowTagOrderNotLineOrder`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./... && golangci-lint run` → PASS.
- [ ] **Step 5: Commit** `feat(scd): port tunnel topology with port-range capacity check` (body: D21).
