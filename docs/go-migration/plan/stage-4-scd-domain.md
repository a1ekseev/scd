# Этап 4. scd: доменный слой

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development; TDD обязателен. Прочитать «Глобальные ограничения» и «Открытые вопросы» в `docs/go-migration/plan/README.md`.

**Goal:** конфиг scd, нормализация VLESS-ссылок под Xray v26.10.10 (D11, D14–D18), построение JSON outbound и его превращение в protobuf через `infra/conf` (D19), манифест/фильтры/`scd generate`, топология туннелей.

**Architecture:** `vless.Link` (common) → `normalize.Normalize` (профиль, проверки, канонизация, warnings, тег) → `outbound.JSON` (форма JSON-конфига Xray v26.10.10, она же хэшируется) → `outbound.ToProto` (`conf.OutboundDetourConfig.Build()`) → `manifest` (entries/skipped/summary, фильтры) → `topology` (порты, теги туннелей). Всё — чистые функции без сети.

**Tech Stack:** `github.com/xtls/xray-core v1.260327.1-0.20261010092107-701af60772cd` (`infra/conf`, `core`, `common/uuid`, `transport/internet/tls`), `golang.org/x/text/unicode/norm`, `internal/common/*`.

**Spec:** `07` D8, D9, D11, D13–D19, D21; `09` (весь, особенно §1, §2.3, §4, §6, §7); `01` §1–§2, §6–§12, §14, §16–§17; `02` §1.3, §1.6; README плана O1–O4, O6.

## Контекст этапа

- Теги: алгоритм `01` §6.4 не меняется (`vless-<cc>-<city>-<profile>-<sha8(raw)>`), поэтому у старых профилей теги совпадают с TS (09 §5). Новые профили — новые имена.
- `jsonOutbound` — **новая форма** (09 §6, §7 п.10): без `allowInsecure`, `wsSettings.host` вместо `headers.Host`, `realitySettings.password`+`spiderX`, lowercase `fingerprint`, `users[].email = tag`. Эталоны этапа 0 по этому полю расходятся намеренно.
- Набор профилей после этапа: `tcp-tls`, `ws-tls`, `tcp-reality-vision` (как в TS), `xhttp-tls`, `xhttp-reality` (D14), `tcp-mlkem`, `ws-mlkem`, `xhttp-mlkem` (O2). `tcp+reality` без vision — `unsupported_combo` (O1).
- Warnings нормализации попадают в `ManifestEntry.Warnings` и логируются в sync событием `manifest_entry_warning` (этап 6).

---

### Task 4.1: Конфиг scd, `validate-config`, `print-config`

**Read:** `01` §1.1, §2 (правила; тексты zod не нужны — D9), §14 (CLI validate/print), §17.3 S1–S14; `07` D8, D12, D15, D22, D23; `10` §6.5; `docker/scd/config.example.yml`; `testdata/reference/scd/print-config/*`, `testdata/reference/scd/config-cases.json`.

**Files:**
- Create: `internal/scd/config/types.go`, `load.go`, `rules.go`, `load_test.go`
- Modify: `internal/scd/cli/cli.go`; Create: `internal/scd/cli/config_cmds.go`, `config_cmds_test.go`

**Interfaces:**
- Consumes: `configload.Load(path, configload.EnvRawStrict, os.LookupEnv)`, `configload.ResolveInput`, `schema`, `cron.Parse`, `listenaddr.Parse`, `regexlit.Compile`, `country.Supported` (**создать в этой задаче** минимальный `internal/scd/country/codes.go` со списком 45 ISO2 из `01` §7 и `func Supported(iso2 string) bool`; задача 4.2 дополнит пакет).
- Produces: типы `01` §1.1 в том же порядке полей (JSON-имена те же) плюс:
  - `SubscriptionConfig.AllowInsecure string` `json:"allowInsecure"` — `deny|allow`, default `deny` (D15, O6);
  - `SubscriptionTargetConfig.BalancerSelection BalancerSelectionConfig` `json:"balancerSelection"` после `balancerMonitor`: `{Enabled bool "enabled"; BalancerTag string "balancerTag,omitempty"; Schedule string "schedule,omitempty"; Observatory bool "observatory"; Strategy string "strategy,omitempty"}`, default `{enabled:false, observatory:true}`, strict; `strategy` ∈ `leastPing|leastLoad|random|roundRobin`.
  - `type Loaded struct{ Path string; Config AppConfig }`; `func Load(path string) (Loaded, []string /*warnings*/, error)`.
  - Фильтры храним уже скомпилированными: `SubscriptionFiltersConfig.LabelIncludeRegex string` + неэкспортируемое поле `re *regexp.Regexp` с методом `LabelRegex() *regexp.Regexp`.
- Правила: `01` §2.2 (дефолты, strict/strip-объекты, trim, `countryAllowlist` → trim+upper, длина 2, поддерживаемые коды), `01` §2.3 п.1–9 (кросс-полевые, **все** проверки выполняются, без «return после daemon-schedule»), `10` §6.5 (`balancerTag` и `schedule` обязательны при `enabled`; без `observatorySubjectSelectorPrefix` — warning `balancerSelection is enabled without observatorySubjectSelectorPrefix; scd tunnels are unlikely to match the balancer selector`), URL-поля — `schema.URL`. Сообщения — Go (`<path>: <message>`), смысл — как в `01` §2.3.
- CLI: `validate-config --config` → warnings в stderr (`warning: …`), stdout `{"configPath": "<abs>", "valid": true}`; `print-config --config` → `jsonout.Write(cfg)`.

- [ ] **Step 1: Failing tests** `load_test.go` — перенос `01` §17.3 S1–S14 (проверять путь issue и ключевую подстроку, не точный текст zod): S1 (env, относительный input, `[de]`→`[DE]`, дефолты statusServer), S2, S3 (`Duplicate target address` → `duplicate target address "127.0.0.1:8080"`), S4, S5, S6 (`not-a-cron` → путь `runtime.schedule`), S7 (`ZZ`), S8 (`/(/i` → путь `labelIncludeRegex`), S9 (`target.speedtest` → unknown key), S10, S11, S12, S13, S14. Новые: `TestAllowInsecureDefaultDeny`, `TestAllowInsecureRejectsOther`, `TestBalancerSelectionRequiresTagAndSchedule`, `TestBalancerSelectionSixFieldCron` (`*/15 * * * * *` ок), `TestBalancerSelectionWarnsWithoutPrefix`, `TestListenEmptyWhenDisabledOK`, `TestLookaheadRegexRejected` (D8), `TestAllCrossFieldIssuesReported` (daemon без schedule + inbounds без outbounds → обе issue).
- [ ] **Step 2: Failing tests** `config_cmds_test.go`: `TestPrintConfigMinimal` (семантически = `testdata/reference/scd/print-config/minimal.json` + новые ключи `allowInsecure: "deny"`, `balancerSelection: {enabled:false, observatory:true}`; `input` сравнивать по суффиксу), `TestPrintConfigDockerExample`, `TestValidateConfigJSON`.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.**
- [ ] **Step 5: Verify** `go test -race ./internal/scd/... && go build -o bin/scd ./cmd/scd && ./bin/scd validate-config --config docker/scd/config.example.yml && ./bin/scd print-config --config docker/scd/config.example.yml | jq '.subscriptions[0].allowInsecure'` → `"deny"`.
- [ ] **Step 6: Commit** `feat(scd): port scd config with allowInsecure and balancerSelection options` (body: D8, D15, D22, D23).

---

### Task 4.2: Карта стран и тег outbound'а

**Read:** `01` §6.4, §7, §16 п.1, 12–15, §17.1 P6–P7, §17.4 (sha8, теги); `07` D9.

**Files:**
- Modify/Create: `internal/scd/country/country.go`, `country_test.go` (расширить пакет из 4.1)
- Create: `internal/scd/normalize/tag.go`, `tag_test.go`

**Interfaces:**
- Produces (`country`): `type Info struct{ Emoji string "emoji"; ISO2 string "iso2"; NameEn string "nameEn"; NameRu string "nameRu" }`; `func FromLabel(label string) (Info, bool)` — первые две руны label = флаг из 45 кодов; `NameEn`/`NameRu` захардкожены по `01` §7 (значения Node 22: `Türkiye`, `Hong Kong SAR China`, `Czechia` и т.д.).
- Produces (`normalize`): `func InferCity(label string) (string, bool)`; `func Slugify(s string) string` (NFKD → убрать U+0300–U+036F → lower → `[^a-z0-9]+`→`-` → trim `-` → первые 32 символа); `func Sha8(s string) string`; `func BuildTag(label, address, profile, raw string) string` — `01` §6.4 (`\s` в InferCity — `unicode.IsSpace`, D9).

- [ ] **Step 1: Failing tests:** `TestFromLabelAustria` (P6), `TestFromLabelOutsideMap` (`🇽🇰` → false), `TestFromLabelLeadingSpace` → false; `TestSha8Vena` (`3ed0e2a1`); `TestBuildTagTable` — все строки таблицы `01` §6.4 (с `raw` = фиксированная строка; хэш-часть проверять через `Sha8(raw)`); `TestSameCountryDifferentCityDifferentTags` (P7); `TestTagDependsOnRawWhitespace`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** (`go get golang.org/x/text@latest`). **Step 4: Run** `go test -race ./internal/scd/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): add country map and outbound tag builder`.

---

### Task 4.3: Нормализация VLESS: профили tls/ws/reality, параметры v26.10.10

**Read:** `09` §1 (вся таблица), §1.3, §1.4, §2.3, §4 п.1–8, §7; `01` §6.1–§6.3, §16 п.5–7; `07` D11, D14, D15, D16, D17, D18; README плана O1, O3, O4.

**Files:**
- Create: `internal/scd/normalize/normalize.go`, `params.go`, `normalize_test.go`

**Interfaces:**
- Consumes: `vless.Link`, `vless.Skip` + коды, `country.FromLabel`, `BuildTag`, `github.com/xtls/xray-core/common/uuid` (`ParseString`), `github.com/xtls/xray-core/transport/internet/tls` (`GetFingerprint`).
- Produces:
  ```go
  type Policy struct{ AllowInsecure bool }
  type Warning struct{ Code string `json:"code"`; Message string `json:"message"` }  // flow_cleared | allow_insecure_ignored
  type Normalized struct {   // JSON-ключи и порядок: как 01 §1.2 NormalizedOutbound + новые после shortId
      Kind, Protocol, Profile, Tag string; Line int; Raw, Label, Address string; Port int
      UUID, Encryption, Network, Security string
      Flow string `json:"flow,omitempty"`; ALPN []string `json:"alpn"`
      SNI, Host, Path, Fingerprint string /*omitempty*/; AllowInsecure *bool `json:"allowInsecure,omitempty"`
      HeaderType *string `json:"headerType,omitempty"`; PublicKey, ShortID string /*omitempty*/
      SpiderX string `json:"spiderX,omitempty"`; PinnedPeerCertSha256 []string `json:"pinnedPeerCertSha256,omitempty"`
      VerifyPeerCertByName []string `json:"verifyPeerCertByName,omitempty"`
      XHTTPMode string `json:"xhttpMode,omitempty"`; XHTTPExtra json.RawMessage `json:"xhttpExtra,omitempty"`
      Country *country.Info `json:"country,omitempty"`; City string `json:"city,omitempty"`
      Query Params `json:"query"`   // упорядоченный объект из vless.Link.Params (свой MarshalJSON)
  }
  func Normalize(l vless.Link, p Policy) (Normalized, []Warning, *vless.Skip)
  ```
- Порядок проверок и тексты (reason дословно):
  1. Профиль: `network = lower(type||"tcp")` (`raw`→`tcp`), `security = lower(security)`; `tcp+tls`→`tcp-tls`, `ws+tls`→`ws-tls`, `tcp+reality+flow∈{vision, vision-udp443}`→`tcp-reality-vision`; xhttp/mlkem — задача 4.4 (здесь → combo); иначе `unsupported_combo` «Unsupported VLESS type/security/flow combination.» details `["type=<raw>","security=<raw>","flow=<raw>"]`.
  2. `encryption`: `""`/`none` → `none`; иначе (до 4.4) `unsupported_value` «Unsupported VLESS encryption value.» `["encryption=<v>"]`.
  3. id: `uuid.ParseString(id)` ошибка → `unsupported_value` «The VLESS user id is not accepted by Xray.» (O4); `UUID = u.String()`.
  4. ws: пустой `path` → `missing_required_field` «WebSocket VLESS requires a path.»
  5. reality: первый пустой из `sni, fp, pbk` → `missing_required_field` `REALITY VLESS requires parameter "<name>".` (`sid` может быть пустым — D17).
  6. `fp`: lowercase; непустой и `tls.GetFingerprint(fp)==nil`, либо reality и `fp∈{unsafe,hellogolang}` → `unsupported_value` «Unsupported TLS fingerprint.» `["fp=<v>"]`.
  7. `pbk`: убрать `=`, `+/`→`-_`, `base64.RawURLEncoding`, длина 32, иначе «REALITY pbk must be 32-byte base64url.» `["pbk=<v>"]`; хранить нормализованную строку.
  8. `sid`: `^[0-9a-fA-F]{0,16}$` и чётная длина, иначе «REALITY sid must be up to 16 hex characters of even length.»
  9. `spx` (только reality): непустой и не с `/` → «REALITY spx must start with "/".»; `SpiderX = spx` или `"/"`.
  10. `pcs` (только tls): split `,`, trim, удалить `:`, hex 32 байта каждый, иначе «pcs must be comma-separated SHA-256 hex.»; `vcn`: split `,`, trim, пустые выкинуть.
  11. `allowInsecure`: `0|1|true|false` (без учёта регистра), иначе «allowInsecure must be 0, 1, true, or false.» `["allowInsecure=<v>"]`; если true и нет `pcs`/`vcn`: `Policy.AllowInsecure==false` → `unsupported_value` «allowInsecure=1 requires pcs or vcn: Xray v26.10.10 no longer supports allowInsecure (set subscriptions[].allowInsecure: allow to keep such links).»; иначе warning `allow_insecure_ignored` «allowInsecure is not supported by Xray v26.10.10; the outbound will verify certificates (use pcs/vcn).» (D15).
  12. `headerType`: `""`/`none` иначе «Only headerType=none is supported.» `["headerType=<v>"]`.
  13. `flow`: ∉ `{"", xtls-rprx-vision, xtls-rprx-vision-udp443}` → «Unsupported VLESS flow value.» `["flow=<v>"]`; vision при не (`network==tcp && security∈{tls,reality}`) и не mlkem → `Flow=""` + warning `flow_cleared` «flow <v> is not supported on <network>+<security>; flow cleared.» (D16, O3).
  14. ALPN: split `,`, trim, без пустых. Tag — `BuildTag(label, address, profile, raw)`.

- [ ] **Step 1: Failing tests** (`normalize_test.go`, table-driven; строки — из `testdata/reference/scd/corpus/vless-corpus.txt` по смыслу):
  - профили: tcp-tls, ws-tls, reality-vision (фикстуры 09 §6.1–§6.3), `type=raw`→tcp-tls, `type=grpc&security=tls` → combo (P19), tcp без security → details `["type=tcp","security=","flow="]`, reality без flow → combo (O1).
  - `encryption=None` → unsupported_value; `headerType=` → ok, `HeaderType` = `""`; `allowInsecure=` → unsupported_value.
  - `fp=Chrome` → `chrome`; `fp=bogus` → skip; reality `fp=unsafe` → skip.
  - pbk: валидный 43-символьный → ok; `PBK` → skip; std-алфавит с `=` → нормализован.
  - sid: пустой → ok (D17); `abc` (нечётный) → skip; 18 символов → skip.
  - spx: `/abc?p=10-20` → `SpiderX="/abc?p=10-20"`; нет spx → `"/"`; `abc` → skip.
  - pcs/vcn: валидные → поля заполнены; `pcs=zz` → skip.
  - allowInsecure: `1` без pcs при deny → skip; при allow → ok + warning `allow_insecure_ignored`; `1` + `vcn` → ok без warning.
  - flow: ws+tls+vision → `Flow==""` + `flow_cleared` (D16); tcp+tls+vision → сохранён; `xtls-rprx-bogus` → skip.
  - id: `my-user` → UUID = `uuid.ParseString("my-user").String()`; uppercase UUID → lowercase; 33-символьный не-hex → skip (O4).
  - ALPN `h2,%20http/1.1,,` → `["h2","http/1.1"]`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** (`go get github.com/xtls/xray-core@701af60772cda123492f86e383e1fdba066614a9`; проверить, что в `go.mod` строка ровно `v1.260327.1-0.20261010092107-701af60772cd`; добавить комментарий из Глобальных ограничений).
- [ ] **Step 4: Run** `go test -race ./internal/scd/... && golangci-lint run` → PASS.
- [ ] **Step 5: Commit** `feat(scd): normalize VLESS links for Xray v26.10.10` (body: D11, D14–D18, O1, O3, O4).

---

### Task 4.4: Нормализация: xhttp и VLESS ML-KEM

**Read:** `09` §1.1 (строка `encryption`), §3.2 (`mode`, `extra`), §4 п.10, п.13; `07` D14, D16; README плана O2, O3; исходники Xray для справки (только чтение, через `go doc`/`$(go env GOMODCACHE)/github.com/xtls/xray-core@v1.260327.1-0.20261010092107-701af60772cd/infra/conf/transport_method.go` — `SplitHTTPConfig`, `infra/conf/vless.go:336-378`).

**Files:**
- Modify: `internal/scd/normalize/normalize.go`, `params.go`; Create: `internal/scd/normalize/xhttp_mlkem_test.go`

**Interfaces:**
- Produces: новые профили в `Normalize`:
  - `xhttp+tls` → `xhttp-tls`; `xhttp+reality` → `xhttp-reality` (reality-проверки п.5–9 задачи 4.3 применяются);
  - `security` пуст/`none` + `encryption` с префиксом `mlkem768x25519plus.` → `<network>-mlkem` для network ∈ {tcp, ws, xhttp} (O2); `security=none` без mlkem → combo.
  - `encryption` с префиксом `mlkem768x25519plus.` допустим на всех профилях; формат (точки, режим `native|xorpub|random`, `1rtt|0rtt`) проверяет `ToProto` (задача 4.5/4.6); прочие значения → «Unsupported VLESS encryption value.»
  - **Ключи ML-KEM проверяются здесь** (ревью: `conf.Build()` проверяет только длину base64url-ключа — 32 или 1184 байта, — а живой Xray 26.10.10 на `AddOutbound` падает `proxy/vless/outbound: failed to use encryption > mlkem: invalid polynomial encoding` для ключа правильной длины, но не являющегося ключом ML-KEM-768; одна такая ссылка валила бы outbound-apply всего target'а с rollback). Правило (повторяет `proxy/vless/encryption/client.go` `ClientInstance.Init` и `infra/conf/vless.go:336-378`): сегменты после `<mode>.<rtt>.` короче 20 символов — padding (не проверяются); остальные — `base64.RawURLEncoding`; 32 байта → `ecdh.X25519().NewPublicKey(b)`, 1184 байта → `mlkem.NewEncapsulationKey768(b)` (stdlib `crypto/mlkem`); ошибка → `unsupported_value` «Invalid VLESS encryption key.» `["encryption=<первые 40 символов>…"]`.
  - xhttp: `path` (default `/`), `host`, `mode` ∈ `{"", auto, packet-up, stream-up, stream-one}` иначе «Unsupported xhttp mode.» `["mode=<v>"]`; `extra` — URL-декодированный JSON-объект (`json.Valid` и первый непробельный символ `{`), иначе «xhttp extra must be a JSON object.»; хранится в `XHTTPExtra`.
  - flow vision при mlkem разрешён на любом транспорте (O3).

- [ ] **Step 1: Failing tests:** `TestXHTTPTLS` (path/host/mode/extra в полях), `TestXHTTPReality`, `TestXHTTPDefaultPath`, `TestXHTTPBadMode`, `TestXHTTPBadExtra`, `TestMLKEMNoneSecurity` (`tcp-mlkem`, Encryption = исходная строка; ключ — валидная строка из корпуса 0.2, снятая `xray vlessenc`), `TestMLKEMOverTLS` (`tcp-tls`, Encryption = строка), `TestMLKEMX25519KeyAccepted` (32-байтный ключ), `TestMLKEMInvalidKeySkipped` (1184 случайных байта → `unsupported_value` «Invalid VLESS encryption key.»; Review Focus 6), `TestNoneWithoutMLKEMIsCombo`, `TestVisionKeptWithMLKEMOnWS`, `TestEchAndFmSkipped` (через `vless.Parse` → `unsupported_param`, D14).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test -race ./internal/scd/...` → PASS.
- [ ] **Step 5: Commit** `feat(scd): support xhttp transport and VLESS ML-KEM encryption` (body: D14, D16, O2, O3).

---

### Task 4.5: JSON outbound v26.10.10 и `ToProto` через `infra/conf`

**Read:** `07` D6, D13, D19; `09` §1.2, §1.4, §3.4 (вариант B), §6 (эталонные структуры), §7 п.5, 10–11; `02` §3 (типы), §6 п.3; `08` §2.1; scratchpad-пример `/tmp/claude-0/-home-user-scd/b738d151-5a61-5b3f-9a86-51bb9c74ebc7/scratchpad/x2610/subs/main.go` (если доступен; иначе 09 §6).

**Files:**
- Create: `internal/scd/outbound/json.go`, `proto.go`, `override.go`, `outbound_test.go`, `proto_golden_test.go`

**Interfaces:**
- Consumes: `normalize.Normalized`.
- Produces:
  - `type Outbound struct{…}` (поля — структура JSON ниже; порядок полей структуры = порядок ключей) в форме:
    `{"tag","protocol":"vless","settings":{"vnext":[{"address","port","users":[{"id","encryption","flow"?,"email":<tag>}]}]},"streamSettings":{"network":"tcp|ws|xhttp","security":"tls|reality|none", "tlsSettings":{"serverName"?,"alpn"?,"fingerprint"?,"pinnedPeerCertSha256"?(строка через ","),"verifyPeerCertByName"?(строка через ",")}, "realitySettings":{"serverName","fingerprint","password","shortId","spiderX"}, "wsSettings":{"path","host"?}, "xhttpSettings":{"path","host"?,"mode"?,"extra"?}}}`
    (`tlsSettings` присутствует всегда при tls, может быть `{}`; секции только для своего network/security; `encryption` — `none` или mlkem-строка).
  - `func Build(n normalize.Normalized) Outbound`
  - `func ToProto(o Outbound) (*core.OutboundHandlerConfig, error)` — `json.Marshal(o)` → `json.Unmarshal` в `conf.OutboundDetourConfig` → `.Build()`; ошибка оборачивается `fmt.Errorf("xray rejected outbound %s: %w", tag, err)`.
  - `func Hash(list []Outbound) string` — `sha256` hex от `json.Marshal(list)`.
  - `func ForTarget(n normalize.Normalized, tag string, visionUdp443Override bool) normalize.Normalized` — копия с новым `Tag` и `Flow = xtls-rprx-vision-udp443`, если override и `Security=="reality"` и `Flow=="xtls-rprx-vision"` (`01` §12.1).
  - Подавление логов Xray — **обязательно** (проверено ревью): `common/log` в `init()` регистрирует логгер в **stdout** (`common/log/logger.go:186`), и `conf.Build()` для каждого ws-outbound печатает `[Warning] common/errors: The feature WebSocket transport … is deprecated…` в stdout. Это ломает JSON-вывод `scd generate`/`scd sync`/`print-config` (`| jq` в шагах 4.6/6.1). В `proto.go` `init()` вызвать `xlog.RegisterHandler(h)` (`xlog "github.com/xtls/xray-core/common/log"`), где `h` реализует `Handle(xlog.Message)`: отбрасывает сообщения (или, если позже понадобится, передаёт в slog на уровне debug). Проверено: после регистрации stdout чист, сообщения приходят в handler.
- Pitfalls: `conf` требует `"encryption":"none"` явно; `password` (а не `publicKey`) для REALITY; `?ed=` из ws path вынимает сам conf (в JSON path хранится как в ссылке, 09 §6.2); адрес IP → `ip`, домен → `domain` (conf).

- [ ] **Step 1: Failing tests (`outbound_test.go`):** `TestBuildJSONTcpTLS`/`WsTLS`/`Reality` — `json.Marshal(Build(n))` равен эталонной строке 09 §6.1–§6.3 (с добавленным `email`); `TestBuildJSONOmitsAllowInsecure`; `TestHashStableForSameInput`; `TestForTargetUdp443OnlyReality` (`01` §17.2 T2-логика: tcp-tls с vision не меняется).
- [ ] **Step 2: Failing tests (`proto_golden_test.go`)** — «каждый параметр доходит до proto» (D6, D19): для каждого кейса `ToProto` → `SenderSettings.GetInstance()` → `*proxyman.SenderConfig` → `StreamSettings.GetEffectiveSecuritySettings()`/`GetEffectiveTransportSettings()`, `ProxySettings.GetInstance()` → `*outbound.Config` → `Vnext.User.Account.GetInstance()` → `*vless.Account`:
  - tcp-tls: `tls.Config{ServerName, NextProtocol, Fingerprint}`; pcs → `PinnedPeerCertSha256` (32 байта), vcn → `VerifyPeerCertByName`.
  - ws-tls: `websocket.Config{Host:"cdn-1.example.test", Path:"/stream/updates/case-a", Ed:2048}`, `Header` пуст; `TransportConfig.ProtocolName=="websocket"` (D6).
  - reality: `PublicKey` 32 байта, `ShortId` 8 байт (из `abe4a59b9f2407e3` и из пустого sid — 8 нулей), `SpiderX`, **`len(SpiderY)==10`** (`TestRealityAlwaysHasSpiderY` — регресс D13, для каждого reality-кейса корпуса).
  - spx `/abc?p=10-20&c=2` → `SpiderY[0..3]=10,20,2,2`, `SpiderX="/abc"`.
  - xhttp: `splithttp.Config{Host, Path, Mode}` и поля из `extra` (например `xPaddingBytes`).
  - mlkem: `vless.Account.Encryption` = только ключи (conf отрезает `mlkem768x25519plus.<mode>.<rtt>.`, сегменты-padding (<20 символов) уходят в `Account.Padding`); `XorMode` = 0/1/2 для `native/xorpub/random`, `Seconds` = 1 для `0rtt`, 0 для `1rtt` (проверено ревью).
  - IP-адрес → `Address.GetIp()` 4 байта; домен → `GetDomain()`.
  - `User.Email == tag`; flow `xtls-rprx-vision-udp443` проходит.
  - негатив (09 §7 п.11, neg[0..12] из scratchpad при наличии): mlkem с битым ключом → ошибка; `encryption` пустая в JSON недопустима (проверка что `Build` всегда ставит `none`).
  - `TestToProtoIsSilent`: перехватить `os.Stdout`/`os.Stderr` (pipe) и вызвать `ToProto` для ws-tls → в pipe пусто.
- [ ] **Step 3: Run** → FAIL. **Step 4: Implement.** **Step 5: Run** `go test -race ./internal/scd/... && golangci-lint run` → PASS; `go build -o bin/scd ./cmd/scd && ls -la bin/scd` (ожидаемо ~50 МБ без `-s -w`; ~35 МБ в образе с `-ldflags="-s -w"`, D19; замер прототипа ревью: 50.8 / 34.7 МБ).
- [ ] **Step 6: Commit** `feat(scd): build v26.10.10 JSON outbounds and protobuf via infra/conf` (body: D6, D13, D19).

---

### Task 4.6: Манифест, фильтры, загрузка подписок и `scd generate`

**Read:** `01` §8, §9, §10, §13, §14.1–§14.2, §16, §17.1 (P1–P5, P7–P10, P19–P20, P25), §17.3 S15; `07` D8, D9, D23; `testdata/reference/scd/generate/*`, `testdata/reference/scd/corpus/ts-parse.json`.

**Files:**
- Create: `internal/scd/manifest/manifest.go`, `filter.go`, `subscriptions.go`, `manifest_test.go`, `filter_test.go`, `subscriptions_test.go`
- Create: `internal/scd/cli/generate.go`, `generate_test.go`

**Interfaces:**
- Consumes: `subscription.ScanLines`, `vless.Parse`, `normalize.Normalize`, `outbound.Build/ToProto`, `inputsrc.Load`, `config.*`.
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
  func FromSubscription(s LoadedSubscription, now time.Time) (Manifest, error)  // Build(strings.TrimSpace(content), id) → filters → Validate
  ```
- `Build`: алгоритм `01` §8; после нормализации `outbound.ToProto(outbound.Build(n))` — ошибка → skip `unsupported_value` «Xray rejected the outbound configuration.» details `[err.Error()]` (D19, 09 §3.4); дубль тега → `duplicate_tag` `Duplicate generated outbound tag "<tag>".`. `LoadSubscriptions`: только enabled, сортировка `strings.Compare` по id, затем input (D23), последовательно. `GeneratedAt` — `now.UTC().Format("2006-01-02T15:04:05.000Z")`.
- `scd generate --input --output --log` (`01` §14.1): `inputsrc.Load` (auto, 5s), `Build(content, source, Policy{})` (без trim и фильтров), `Validate`, запись двух файлов (`jsonout.Write`), stdout — summary; при ошибке файлы не создаются.

- [ ] **Step 1: Failing tests (`manifest_test.go`):** P1/P2 — фикстура `testdata/reference/scd/fixtures/vpn-sample.txt` содержит 2 ws-строки с `allowInsecure=1` без `pcs`/`vcn` (строки 9–10), поэтому при D15-дефолте они пропускаются: `TestBuildVpnSampleAllowPolicy` (`Policy{AllowInsecure: true}` → профили tcp-tls, ws-tls, tcp-reality-vision; parsed==len(entries)==12; у двух ws-entries warning `allow_insecure_ignored`) и `TestBuildVpnSampleDenyPolicy` (`Policy{}` → 10 entries без ws-tls, 2 skipped `unsupported_value` с текстом про `allowInsecure=1 requires pcs or vcn`), P7, P8 (дубль → duplicate_tag), P9, P10, P19, P20, `TestBuildManifestCRLFEqualsLF` (Review Focus 3: одинаковые entries/теги для `\r\n` и `\n`), `TestBuildSkipsXrayRejected` (mlkem с битым ключом → `unsupported_value` с details), `TestBuildCountsSummary`, `TestBuildMatchesTSForLegacyProfiles` — для `testdata/reference/scd/generate/vpn-sample.manifest.json`: множество тегов entries совпадает, `skipped[].reasonCode` по номерам строк совпадают (кроме allowlist расхождений с комментарием: ws-ссылки с `allowInsecure=1` → теперь skip при deny, D15).
- [ ] **Step 2: Failing tests (`filter_test.go`):** P3, P4 (изм.: regex `/,\s*Extra\s*$/i`; ожидание — `Extra Whitelist#2` отфильтрован, т.к. не оканчивается на `Extra`), P5, `TestEmptyAllowlistDisablesFilter`.
- [ ] **Step 3: Failing tests (`subscriptions_test.go`):** S15 (`[source-b, source-a]` → `[source-a, source-b]`), `TestLoadSubscriptionsContinuesAfterFailure`, `TestValidateNoValidEntriesMessage` (точный текст `01` §10 со всеми счётчиками), `TestFromSubscriptionTrimsContent`.
- [ ] **Step 4: Failing tests (`generate_test.go`):** P25 (`trojan://example\n` → ошибка `produced no valid outbound entries`, файлы не созданы), `TestGenerateDockerFixture` (summary совпадает с `testdata/reference/scd/generate/docker-fixture.summary.json`).
- [ ] **Step 5: Run** → FAIL. **Step 6: Implement.**
- [ ] **Step 7: Verify** `go test -race ./internal/scd/... && go build -o bin/scd ./cmd/scd && ./bin/scd generate --input docker/fixtures/subscription.txt --output /tmp/m.json --log /tmp/l.json | jq .parsed` → `8` (TS на `node:24-alpine` даёт тот же summary: `totalLines 8, parsed 8, skipped 0`; `jq` работает только если stdout не засорён логами Xray — см. 4.5 «Подавление логов»).
- [ ] **Step 8: Commit** `feat(scd): port manifest building, filters and generate command` (body: D8, D19, D23).

---

### Task 4.7: Топология туннелей target'а

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
