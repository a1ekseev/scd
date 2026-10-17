# scd

`scd` is one Go binary with three services:

| Command | Service | What it does |
|---|---|---|
| `scd …` | **scd** — Subscription Control Daemon | Turns VLESS subscriptions into Xray outbounds (plus SOCKS inbounds and routing rules per tunnel), applies them to Xray over the gRPC API, monitors and repairs tunnels, and serves a read-only status dashboard and JSON API. |
| `scd sfc …` | **sfc** — Subscription Filter Control | Downloads upstream subscriptions, keeps VLESS lines whose label matches per-output filters, repacks them to base64 and serves them by public URL. Does not talk to Xray. |
| `scd gtl …` | **gtl** — Gen Test Load | HTTP server that streams a deterministic payload of the requested size, for download/load tests. |

There is one image, `ghcr.io/a1ekseev/scd`, for all three. The target Xray version is **v26.10.10**.

Upgrading from the pre-Go version: config keys changed. Read
[`docs/CONFIG-MIGRATION.md`](docs/CONFIG-MIGRATION.md) (in Russian). It has
before/after examples for every changed key and walks through real-shape
configs step by step.

- [Build](#build)
- [Commands](#commands)
- [Docker](#docker)
- [scd](#scd-subscription-control-daemon)
- [Label filters](#label-filters)
- [Supported VLESS links](#supported-vless-links)
- [Fetching subscriptions](#fetching-subscriptions)
- [Balancer selection](#balancer-selection)
- [scd sfc](#scd-sfc)
- [scd gtl](#scd-gtl)
- [Cron schedules](#cron-schedules)
- [Logging](#logging)
- [Known limitations](#known-limitations)
- [Migration notes](#migration-notes)
- [Development](#development)

## Build

Requires Go 1.27 (`go.mod`: `go 1.27.0`, `toolchain go1.27.2`).

```bash
go build -o bin/scd ./cmd/scd
./bin/scd --version          # "dev" unless set at build time
go build -ldflags "-X main.version=1.2.3" -o bin/scd ./cmd/scd
```

`--version` exists only on the root command. Subscription downloads use
`User-Agent: scd/<version>` by default.

## Commands

All commands take `--config <file>`, except `generate`. Machine-readable
output goes to stdout and logs go to stderr.

| Command | Purpose |
|---|---|
| `scd validate-config --config f` | Validate. Prints `{"configPath": "<abs>", "valid": true}`. Warnings go to stderr as `warning: <path>: <text>`, exit code 0. |
| `scd print-config --config f [--include-secrets]` | Print the normalized config as JSON (defaults applied, `input` made absolute). Secrets are redacted unless `--include-secrets`: an http(s) `input` and the `monitor.request.url`, `balancerMonitor.request.url` and `balancerMonitor.remotePing.url` show as `<scheme>://<host>/<redacted>`, `fetchHeaders` values as `<redacted>`. |
| `scd sync --config f` | Run one sync and print the JSON report. Exit code 1 when `failed > 0`. On SIGINT/SIGTERM the Xray apply in flight finishes, the remaining subscriptions and targets are skipped, `sync_canceled` (warn) is logged, the partial report is printed and the exit code is 130 (SIGINT) or 143 (SIGTERM). |
| `scd daemon --config f` | Run sync, monitors, balancer selection, the status server and remote status polling on cron schedules. Needs `runtime.mode: daemon`. |
| `scd generate --input <file\|url\|-> --output m.json --log log.json` | Low-level: build the outbound manifest and parse log from one subscription without touching Xray. |
| `scd sfc serve\|refresh\|validate-config\|print-config --config f` | sfc: serve with background refresh / one refresh with JSON report / validate / print (`input` and `fetchHeaders` redacted as in scd unless `--include-secrets`). An interrupted `refresh` prints its partial report and exits 130/143 like `scd sync`. |
| `scd gtl serve\|validate-config\|print-config --config f` | gtl: serve / validate (`Config is valid`) / print. |

None of the services hot-reload the config. Restart after a change.
`SIGINT`/`SIGTERM` stop all loops right away. Operations already in flight
finish first: Xray apply/repair/rejoin, the current sfc refresh, open HTTP
requests (up to 10 s), and pending remote pings. A second signal exits at once.

## Docker

The image is `ghcr.io/a1ekseev/scd:latest` or `:<X.Y.Z>`, for `linux/amd64` and
`linux/arm64`. It is published from release tags `vX.Y.Z`. The image is distroless
(`gcr.io/distroless/static-debian13`: CA certificates and tzdata, no shell). It
runs `CMD ["scd", "daemon", "--config", "/app/config/config.yml"]`. Choose the
service with `command:`:

| Service | `command:` |
|---|---|
| scd | default CMD |
| sfc | `["scd", "sfc", "serve", "--config", "/app/config/config.yml"]` |
| gtl | `["scd", "gtl", "serve", "--config", "/app/config/config.yml"]` |

The root `docker-compose.yml` runs Xray `ghcr.io/xtls/xray-core:26.10.10` and
all three services on one locally built image. It mounts the git-ignored
`docker/<svc>/config.yml` files. To start from the examples, run
`cp docker/<svc>/config.example.yml docker/<svc>/config.yml`.

```bash
docker compose up -d --build
docker compose logs -f scd
docker compose down
bash scripts/compose-smoke.sh   # self-contained smoke test on the example configs
```

Published ports: the scd dashboard at <http://127.0.0.1:9090/>, sfc at
<http://127.0.0.1:9091/s/test-all>, and gtl at <http://127.0.0.1:9092/load/32>.
See [`docker/README.md`](docker/README.md) for details. To validate a config
with the image:
`docker run --rm -v "$PWD/docker/scd/config.yml":/app/config/config.yml:ro ghcr.io/a1ekseev/scd scd validate-config --config /app/config/config.yml`.

## scd (Subscription Control Daemon)

### Config

Complete example: [`docker/scd/config.example.yml`](docker/scd/config.example.yml).

```yaml
subscriptions:
  - id: main
    input: https://panel.example.com/sub/<token>   # file path, http(s) URL or "-" (stdin)
    enabled: true
    format: auto                 # auto | plain | base64
    fetchTimeoutMs: 5000
    allowInsecure: deny          # deny | allow (links with allowInsecure=1)
    # fetchUserAgent: scd/1.0    # default scd/<version>; read the warning below
    # fetchHeaders:
    #   x-hwid: "${SCD_HWID}"    # values are secrets
    filters:
      countryAllowlist: [DE, NL]
      labelIncludeRegex: '/,\s*Extra\s*$/i'
      labelExcludeRegex: '/WhiteList/i'
    target:
      address: 127.0.0.1:8080    # Xray gRPC API
      timeoutMs: 5000
      fixedOutbounds: [direct, blocked]
      fixedInbounds: []
      fixedRouting: []
      visionUdp443Override: false
      observatorySubjectSelectorPrefix: x-observe-
      inboundSocks:
        listen: 127.0.0.1
        portRange: {start: 20000, end: 20099}
      monitor:
        enabled: true
        schedule: "*/2 * * * *"
        maxParallel: 10
        retry: {attempts: 2, delayMs: 250}
        request: {url: https://example.com/, method: GET, expectedStatus: 200, timeoutMs: 5000}
      balancerMonitor:
        enabled: true
        schedule: "*/2 * * * *"
        socks5: {host: 127.0.0.1, port: 1080}
        request: {url: https://example.com/health, expectedStatus: 200}
        remotePing:
          enabled: true
          url: https://uptime.example.com/api/push/<token>
          timeoutMs: 5000
          viaSocks: true
      balancerSelection:
        enabled: true
        balancerTag: x-balancer
        schedule: "*/15 * * * * *"
runtime:
  mode: daemon                   # run-once | daemon
  schedule: "*/5 * * * *"
logging: {level: info, format: json}
resources:
  outbounds: {enabled: true}
  inbounds: {enabled: true}
  routing: {enabled: true}
statusServer:
  enabled: true
  listen: 127.0.0.1:9090
  instanceId: control-eu-1
  remoteInstances:
    - {id: edge-us-1, apiUrl: "http://edge-us-1:9090/api/instance-status", schedule: "*/1 * * * *", timeoutMs: 5000}
  runtimeState: {enabled: true, includeRaw: false, includeSecrets: false}
```

| Key | Meaning | Default |
|---|---|---|
| `subscriptions[]` | Subscription sources. At least one must be enabled. Every `target.address` must be unique. | — |
| `.id` | Source id used in logs, reports and the dashboard. | required |
| `.input` | File path (relative to the config file), `http(s)` URL, or `-` for stdin. | required |
| `.enabled` / `.format` / `.fetchTimeoutMs` | On/off; decoding `auto\|plain\|base64`; download timeout (does not apply to Xray calls). | `true` / `auto` / `5000` |
| `.allowInsecure` | Policy for links with `allowInsecure=1` and neither `pcs` nor `vcn`. `deny` skips the link. `allow` accepts it, but Xray v26.10.10 verifies the certificate anyway; logs warning `allow_insecure_ignored`. | `deny` |
| `.fetchUserAgent`, `.fetchHeaders` | Request `User-Agent` and extra request headers, see [Fetching subscriptions](#fetching-subscriptions). | `scd/<version>`, none |
| `.filters.countryAllowlist` | ISO2 codes. Entries without a recognized country are dropped. | no filter |
| `.filters.labelIncludeRegex`, `.filters.labelExcludeRegex` | Label filters, see [Label filters](#label-filters). | no filter |
| `.target.address` | Xray gRPC API `host:port`. | required |
| `.target.timeoutMs` | Timeout of each Xray API call. | `5000` |
| `.target.fixedOutbounds` / `fixedInbounds` / `fixedRouting` | Outbound/inbound tags and routing `ruleTag`s that scd never deletes. | `[]` |
| `.target.visionUdp443Override` | Rewrite REALITY `xtls-rprx-vision` to `xtls-rprx-vision-udp443` on this target. | `false` |
| `.target.observatorySubjectSelectorPrefix` | Prefix added to every applied outbound tag, so that the static `observatory.subjectSelector` / `balancers[].selector` match them. | none |
| `.target.inboundSocks.listen`, `.portRange.start/end` | IP address or `localhost` for the generated SOCKS inbounds, and an inclusive port range (`1..65535`). Required when inbounds or routing are enabled or the monitor is on. | — |
| `.target.monitor` | Per-tunnel HTTP check through each generated SOCKS inbound: `enabled`, `schedule`, `maxParallel`, `retry.{attempts,delayMs}`, `request.{url,method GET\|HEAD\|POST,expectedStatus,timeoutMs}`. Requires all three resource kinds. | off; `10`; `1`/`250`; `GET`, `5000` |
| `.target.balancerMonitor` | Observe-only check of the Xray balancer through an external SOCKS5: `enabled`, `schedule`, `socks5.{host,port}`, `request`, `retry`, `remotePing.{enabled,url,timeoutMs,viaSocks}`. | off |
| `.target.balancerSelection` | Dashboard view of the outbound the Xray balancer selects, see [Balancer selection](#balancer-selection). | `{enabled: false, observatory: true}` |
| `runtime.mode` / `runtime.schedule` | `run-once` or `daemon`. A schedule is required for `daemon`. | `run-once` |
| `logging.level` / `logging.format` | `fatal\|error\|warn\|info\|debug\|trace\|silent`; `json\|pretty`. | `info` / `json` |
| `resources.{outbounds,inbounds,routing}.enabled` | Resource kinds to manage. Inbounds require outbounds. Routing requires both. | `true` / `false` / `false` |
| `statusServer.enabled` / `listen` / `instanceId` | Status server; `host:port` (`:8080` = all interfaces, IPv6 as `[::]:8080`); instance name. | `true` / `127.0.0.1:9090` / hostname |
| `statusServer.remoteInstances[]` | Other scd instances to poll: `id`, `apiUrl` (their `/api/instance-status`), `schedule`, `timeoutMs`. | `[]`; `5000` |
| `statusServer.runtimeState.{enabled,includeRaw,includeSecrets}` | `/api/runtime-state` endpoint, raw protobuf blobs, unredacted secrets. | `true`/`false`/`false` |

Values can reference environment variables with `${NAME}`. Only values are
expanded, not keys or comments, and an undefined variable is an error. Unknown
keys are errors in every object, so a typo such as `statusServer: {enable: true}`
is reported instead of silently ignored.
Validation errors list every problem with its field path, for example:
`subscriptions.0.target.inboundSocks.listen: must be an IP address or "localhost" (Xray cannot listen on a domain)`.

### Sync

- scd owns every non-fixed outbound, inbound and routing rule on a target.
  Apply order is outbound → inbound → routing. Each kind is fully replaced.
  Fixed resources are kept. If an add fails, the outbound kind is rolled back
  best-effort. Resource hashes are stored only when all kinds succeed.
- Topology: one outbound per filtered entry. With inbounds and routing
  enabled, each outbound also gets one SOCKS inbound (`in-…`) and one routing
  rule (`route-…`). Ports are allocated sequentially from `portRange`. If the
  range is too small, only that target fails, with a message naming the
  capacity.
- A target whose manifest did not change during this process lifetime is
  skipped with no Xray calls. This state is memory-only, so the first sync
  after a restart always talks to Xray.
- Empty input, or input with no parsed lines, fails closed for that source:
  its target is marked failed and gets no API calls. Failures stay local to
  one target.
- Filters run once per source after parsing, in this order: `countryAllowlist`,
  `labelIncludeRegex`, `labelExcludeRegex`. The report counts `filtered`
  separately from `skipped` (parse/validation).
- Sync, repair and rejoin share one per-target mutex. A cron slot that
  arrives while the previous tick is still running is skipped with
  `*_tick_skipped_overrun`.
- Subscription URLs appear in logs and in the sync report as
  `<scheme>://<host>/<redacted>`.
- The `scd sync` report, like the `generate` manifest and log, lists skipped
  lines with their full `raw` link, which contains secrets (UUID, keys,
  path); treat that output like the subscription itself.

### Monitoring and repair

- The tunnel monitor (`daemon` only) probes every tunnel through its generated
  SOCKS inbound, at most `maxParallel` at a time. Retries count per check.
  After the final failure, scd keeps the inbound, removes the tunnel's
  outbound and rule, and recreates them **without**
  `observatorySubjectSelectorPrefix`. This takes the tunnel out of the
  balancer. After the next successful check, scd restores the prefixed
  outbound and rule (rejoin).
- `balancerMonitor` checks the balancer end-to-end through an external SOCKS5.
  It never repairs or applies anything. `remotePing` then pushes one group
  result (`status`, `msg`, `ping` query parameters) to an Uptime-Kuma-style
  push URL. With `viaSocks: true` the push goes through the balancer SOCKS5.
  Otherwise it is sent directly and honours `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`.
- Both monitors start working only after the first sync has reached the
  target. Until then their ticks do nothing.
- Probes use one deadline per check (`request.timeoutMs`). Latency is measured
  from sending the request to receiving the response headers. The SOCKS5
  client sends IPv4 targets as ATYP 1, IPv6 as ATYP 4 and domains as ATYP 3.

### Status server

| Path | Content |
|---|---|
| `/`, `/status` (any other path) | HTML dashboard: instance tabs (local first), then subscription → target, balancer cards first, tunnel cards sorted by latency. |
| `/api/status` | `{tunnels}` |
| `/api/instance-status` | Local snapshot `{version: 1, instanceId, capturedAt, subscriptions, tunnels, …}`, which remote collectors poll. |
| `/api/instances` | Local snapshot plus the last successful snapshot of each remote instance (`lastError` on failure). |
| `/api/runtime-state?subscriptionId=…&targetAddress=…` | Diagnostic view from the loaded config and live Xray API data. |

Routing looks only at the path, so `/api/status?x=1` returns JSON. The JSON
schemas are a contract and grow only by adding fields; `version` stays `1`. A
panic in a handler returns 500 and logs `http_handler_panic`. The dashboard is
read-only and has no links to the JSON endpoints.

`/api/runtime-state` hides secrets unless `includeSecrets: true`. Hidden values
include subscription and monitor/push URLs (shown as `<scheme>://<host>/<redacted>`),
`fetchHeaders` values, UUIDs and keys, outbound `path` and the xhttp `host`,
which are `<redacted>`. **`includeRaw: true` adds `rawBase64` protobuf blobs
that contain everything unredacted**, whatever `includeSecrets` says.

## Label filters

Filters use Go `regexp` (RE2: linear time, no lookaround). They apply the same
way in scd (`subscriptions[].filters`) and sfc (`outputs[]`):

- `labelIncludeRegex` is a string or a list. A label is kept only if it
  matches **all** of them.
- `labelExcludeRegex` is a string or a list. A label is dropped if it matches
  **any** of them.
- An element is a bare RE2 pattern (`Extra`) or a literal `/pattern/flags`.
  The only allowed flags are `i`, `m` and `s`. Matching is a substring search.
- Lookahead, lookbehind and backreferences (`(?=`, `(?!`, `(?<=`, `(?<!`, `\1`)
  are `validate-config` errors.

```yaml
# Only "Extra", but not "Extra WhiteList"
labelIncludeRegex: '/,\s*Extra\s*$/i'

# Everything except Extra and WhiteList (was '/^(?!.*Extra)(?!.*WhiteList).*$/i')
labelExcludeRegex: '/Extra|WhiteList/i'

# "Extra" or "Premium", but not "Whitelist"
labelIncludeRegex: '/\b(?:Extra|Premium)\b/i'
labelExcludeRegex: '/\bWhitelist\b/i'

# Both "Extra" and "WhiteList", in any order
labelIncludeRegex: ['/Extra/i', '/WhiteList/i']

# Germany or Netherlands, without "Trial" or "Backup"
labelIncludeRegex: '/\b(?:Germany|Netherlands|DE|NL)\b/i'
labelExcludeRegex: ['/\bTrial\b/i', '/\bBackup\b/i']
```

scd reads two things from a label: the flag emoji (country) and the text
before the first comma (city). Both go into the tag. Anything else in a label
has no meaning to scd. Select entries with the filters above.

## Supported VLESS links

The scope is fixed (D26): transports `tcp`/`raw`, `ws`, `xhttp`; security
`tls`, `reality`; VLESS encryption `none` and ML-KEM
(`mlkem768x25519plus.*`, also with `security=none`). Inside that scope, every
combination and parameter that Xray v26.10.10 accepts is accepted and reaches
Xray over the API. The manifest JSON goes through Xray's own `infra/conf` and
then `AddOutbound`. A link is skipped only when Xray would reject it or it is
out of scope.

| network | security | encryption | flow | Result | Profile |
|---|---|---|---|---|---|
| tcp/raw | tls | none | — / vision / vision-udp443 | accepted | `tcp-tls` |
| tcp | reality | none | — | accepted | `tcp-reality` |
| tcp | reality | none | vision | accepted | `tcp-reality-vision` |
| ws | tls | none | — | accepted | `ws-tls` |
| ws / xhttp | tls (/reality) | none | vision | `flow` cleared, warning `flow_cleared` (Xray would fail every connection) | `ws-tls` / `xhttp-tls` / `xhttp-reality` |
| ws | reality | any | any | skipped `unsupported_combo` | — |
| xhttp | tls | none | — | accepted | `xhttp-tls` |
| xhttp | reality | none | — | accepted | `xhttp-reality` |
| tcp/ws/xhttp | none | ML-KEM | — / vision | accepted, flow kept | `tcp-mlkem` / `ws-mlkem` / `xhttp-mlkem` |
| tcp/xhttp | tls/reality | ML-KEM | — / vision | accepted, flow kept | as without ML-KEM; reality+vision → `<net>-reality-vision` (`xhttp-reality-vision`) |
| ws | tls | ML-KEM | vision | accepted, flow kept | `ws-tls` |
| any | none | none | any | skipped `unsupported_combo` | — |
| any | xtls / unknown | — | — | skipped `unsupported_combo` | — |
| grpc, httpupgrade, kcp, http/h2/quic, … | | | | skipped `unsupported_combo` (out of scope) | — |

Tags have the form `vless-<cc>-<city>-<profile>-<sha8(raw line)>`. Tags of the
three original profiles (`tcp-tls`, `ws-tls`, `tcp-reality-vision`) are
unchanged.

How parameters map to Xray, by key (keys are case-insensitive; values are
URL-decoded):

| Parameter | Applies to | Xray field |
|---|---|---|
| `type` | all | `streamSettings.network` (`raw` → `tcp`) |
| `security` | all | `streamSettings.security` |
| `encryption` | all | `users[0].encryption` (ML-KEM keys are checked; a key of the right length that is not a key → skip «Invalid VLESS encryption key.») |
| `flow` | all | `users[0].flow`: `""`, `xtls-rprx-vision`, `xtls-rprx-vision-udp443`; others → skip |
| user id | all | `users[0].id`: any non-empty string Xray accepts (UUID, or ≤30 chars → UUIDv5); otherwise skip «The VLESS user id is not accepted by Xray.» |
| `sni`, `fp` | tls, reality | `tlsSettings`/`realitySettings.serverName`, `.fingerprint` (lower-cased) |
| `alpn`, `pcs`, `vcn`, `ech`, `cs` | tls | `tlsSettings.alpn`, `.pinnedPeerCertSha256`, `.verifyPeerCertByName`, `.echConfigList` (passed as is), `.cipherSuites` |
| `pbk`, `sid`, `spx`, `pqv` | reality | `realitySettings.password` (std base64 normalized to base64url), `.shortId` (may be empty), `.spiderX` (+ `SpiderY`), `.mldsa65Verify` |
| `headerType`, `host`, `path` | tcp (`headerType=http`) | `tcpSettings.header` with `request.path` / `headers.Host` |
| `host`, `path`, `heartbeatPeriod` | ws | `wsSettings.host`, `.path` (`?ed=N` → early data), `.heartbeatPeriod` |
| `host`, `path`, `mode`, `extra` | xhttp | `xhttpSettings.host`, `.path`, `.mode`, `.extra` (JSON object) |
| `fm` | all in scope | `streamSettings.finalmask` (JSON object) |
| `allowInsecure` | tls | not passed (Xray removed it); handled by the `allowInsecure` policy |

- A ws link without `path`, and REALITY without `sni`/`fp`/`sid`, are accepted.
  REALITY only requires `pbk`.
- A parameter that does not apply to the chosen transport or security is
  dropped silently. Examples: `path=/` on tcp without `headerType=http`,
  `alpn`/`pcs` on REALITY, `pbk`/`sid` on TLS, grpc/kcp parameters.
- The generator flags `support-x25519mlkem768` (3x-ui) and `x_padding_bytes`
  are known and ignored.
- An **unknown** parameter is ignored too. The link is still accepted, with
  warning `param_ignored` (logged at `debug`). sfc logs
  `subscription_line_param_ignored`.
- Skipped: non-`vless://` lines, a missing address or port, a label with
  invalid percent-encoding (`invalid_uri` «The line label is not valid
  percent-encoding.», only that line), and anything `conf.Build` rejects
  (`unsupported_value` «Xray rejected the outbound configuration.» with
  Xray's text in `details`).
- To check a subscription offline: `scd generate --input sub.txt --output m.json --log log.json`.
  `skipped` lists reasons, `entries[].warnings` lists warnings.

## Fetching subscriptions

`subscriptions[].fetchUserAgent` (scd and sfc) sets the `User-Agent` for
downloads from `http(s)` URLs. The default is `scd/<version>`.

> **Warning:** some panels choose the response format by User-Agent. With
> Remnawave default rules, `scd/…`, curl and similar clients get base64 share
> links, which is what scd and sfc need. A UA like `Happ/…` or `INCY/…`
> appends `?serverDescription=…` to every label, and that text ends up in the
> city and the tag. `clash*`, `mihomo`, `stash` and `sfa…`/`singbox` get
> YAML or JSON instead of share links, and the source then yields no entries.

`subscriptions[].fetchHeaders` (scd and sfc) adds request headers. An example
is `x-hwid` for a Remnawave HWID device limit. `User-Agent` (use
`fetchUserAgent`), `Host`, `Content-Length`, `Transfer-Encoding` and
`Connection` are not allowed. **Values are secrets**: `print-config` (unless
`--include-secrets`), logs, error texts and `/api/runtime-state` (unless
`includeSecrets: true`) show `<redacted>`. Use `${NAME}` to take a value from the environment:

```yaml
fetchHeaders:
  x-hwid: "${SCD_HWID}"
```

## Balancer selection

`target.balancerSelection` shows on the dashboard which outbound Xray's
balancer is choosing **now**. A separate read-only loop calls
`RoutingService.GetBalancerInfo` and `ObservatoryService.GetOutboundStatus`
in parallel on its own schedule, without the target lock. It does not need a
sync first, but tunnel names are shown only after a sync.

- `balancerTag` is `routing.balancers[].tag` in the Xray config, not a
  `ruleTag`. Xray does not expose it over the API.
- `schedule` usually has 6 fields with seconds, for example
  `"*/15 * * * * *"`, matching the observatory `probeInterval`. Polling more
  often is useless.
- `observatory: false` stops the `GetOutboundStatus` calls. `strategy`
  (`leastPing|leastLoad|random|roundRobin`) is only a label for the UI.
- Requirements: `"ObservatoryService"` in Xray `api.services`, and an
  `observatory` or `burstObservatory` section. **Without that section, Xray
  with `ObservatoryService` does not start.** If the service is not enabled,
  the selection still works, but liveness shows as unavailable
  (`observatoryError`). Set `observatorySubjectSelectorPrefix`, otherwise
  `validate-config` warns that scd tunnels are unlikely to match the selector.

What is shown, per strategy:

| Strategy | Shown |
|---|---|
| `leastPing` | The selected outbound (minimum observatory delay among alive candidates), mode `single`. |
| `leastLoad` | `single` when it picks one outbound; otherwise the `pool` of picked outbounds. The real choice is random per connection. With `burstObservatory`, ranking uses RTT deviation, not the average. |
| `random`, `roundRobin` | `pool (n)` of candidates. There is no single current choice. |
| no alive candidate | `none (fallback/default outbound)`. Traffic goes to `fallbackTag` or the default outbound, and the card is degraded. |
| override set | `override → <tag>`. Degraded if the tag is not one of the target's tunnels. |

- Repaired tunnels are outside the balancer. Their outbounds have no prefix,
  so the selector cannot pick them until they rejoin.
- **Override warning:** scd never calls `OverrideBalancerTarget`, and the
  dashboard is read-only. Xray does not validate an override set by other
  tools. A typo sends all balancer traffic nowhere, and the override survives
  rule changes until Xray restarts. The dashboard shows it, and marks it
  degraded when it points outside the tunnels.
- Logs: `balancer_selection_changed`, `balancer_selection_failed`,
  `balancer_selection_recovered`, `balancer_observatory_unavailable`.

## scd sfc

Example: [`docker/sfc/config.example.yml`](docker/sfc/config.example.yml).

```yaml
subscriptions:
  - id: main
    input: https://panel.example.com/sub/<token>
    enabled: true
    format: auto
    fetchTimeoutMs: 5000
    # fetchUserAgent / fetchHeaders: as in scd
    pathRoute: /s
    outputs:
      - id: a8f3c9d2               # public, globally unique: served at /s/a8f3c9d2
        name: germany-extra
        labelIncludeRegex: ['/Германия/i', '/Extra/i']
        labelExcludeRegex: '/WhiteList/i'
        userAgent: [Clash, Stash]  # prefixes gating the profile headers
        profileTitle: Germany Extra
        profileUpdateInterval: 6   # 1..24
runtime:
  refreshSchedule: "*/10 * * * *"
server:
  listen: 0.0.0.0:8081
logging: {level: info, format: json}
```

- `pathRoute` is required. It starts with `/`, is not `/`, does not end
  with `/`, and is a clean URL path: no empty, `.` or `..` segments and no
  characters that need escaping (spaces, `%`, `?`, non-ASCII). Output `id`s are unique across all subscriptions. Each output
  needs `labelIncludeRegex` or `labelExcludeRegex`. At least one enabled
  subscription with an enabled output is required. Unknown keys are rejected
  in every object, as in scd and gtl.
- Lines are parsed with the same VLESS parser and parameter list as scd. Lines
  are kept in original order and raw form, with no applicability checks. Only
  the label filters select lines.
- `serve` refreshes once before listening, then on `refreshSchedule`. A
  refresh never runs concurrently with itself. Outputs are served only from
  memory.
- `GET /healthz` → `200 ok`. `GET <pathRoute>/<id>` returns `200`
  `text/plain` with the base64 payload. An unknown id returns `404`. An
  output with no successful payload yet returns `502`. If the request
  `User-Agent` starts with one of the output's `userAgent` prefixes, the
  response also has `profile-title: base64:<…>` and `profile-update-interval`.
- If a download fails, the last good decoded source in
  `<config dir>/.sfc-cache/<id>.subscription` is used (file mode `0600`). If
  there is no cache, or an output comes out empty, that output keeps serving
  its last good payload and the refresh is reported as failed.
- Subscription URLs appear in logs and in the `refresh` report as
  `<scheme>://<host>/<redacted>`, as in scd.

## scd gtl

```yaml
server:
  listen: 0.0.0.0:8080
load:
  path: /load          # required; starts with "/", not "/", no trailing "/", no query/hash
  maxSizeKb: 10240
logging: {level: info, format: json}
```

- `GET /healthz` → `200 ok`.
- `GET <load.path>/<sizeKb>` returns `200` with exactly `sizeKb × 1024` bytes,
  `content-type: application/octet-stream`, and the headers
  `x-gtl-status-code`, `x-gtl-requested-size-kb`, `x-gtl-requested-size-bytes`
  and `x-gtl-actual-size-bytes`.
- A bad size, or a size above `maxSizeKb`, returns `400`. Other paths return
  `404`. Methods other than `GET` return `405`. Unknown config keys are errors.
- The payload is deterministic pseudo-random bytes, streamed in 64 KiB chunks.
  Each chunk comes from xorshift32 (`s ^= s<<13; s ^= s>>17; s ^= s<<5`, low
  byte), seeded with `0x9e3779b9 ^ uint32(offset)` at the start of the chunk.
  Streaming stops when the client disconnects.

## Cron schedules

All schedules use one parser (`internal/common/cron`, `github.com/robfig/cron/v3`
as is). The same parser validates configs and drives the scheduler.

- Classic Linux crontab: 5 fields (`min hour dom month dow`), or 6 with
  leading seconds. Lists, ranges, steps, and month/day names are allowed. When
  both day-of-month and day-of-week are restricted, either one matches.
- Macros: `@yearly`, `@annually`, `@monthly`, `@weekly`, `@daily`,
  `@midnight`, `@hourly`.
- The library also accepts `?`, `@every <duration>`, and `TZ=…`/`CRON_TZ=…`
  prefixes. The default time zone is the process time zone (UTC in the image).
- Not supported (validation error): `L`, `W`, `#`, `H`, `@minutely`,
  `@secondly`, `@weekdays`, `@weekends`, day-of-week `7` (use `0`/`sun`), an
  empty schedule, and expressions that never fire (`0 0 31 2 *`).
- A loop runs its first tick at start, then on each slot without drift. A slot
  that arrives during a running tick is skipped (`*_tick_skipped_overrun`).
  Slots missed while the host was suspended or the clock jumped are not
  replayed: one overrun is logged and the loop resumes at the next slot.

## Logging

- All services use `log/slog` and write to **stderr**. stdout carries only
  command output.
- `logging.format: json` is slog JSON (`time` RFC3339, `level` as a string,
  `msg`, `event`, then fields). `pretty` is `key=value` text with nested
  values as JSON strings.
- Levels: `fatal`, `error`, `warn`, `info`, `debug`, `trace`, `silent`.
- Event names are stable, for example scd `config_loaded`, `sync_started`,
  `subscription_fetched`, `manifest_built`, `apply_finished`, `daemon_tick`,
  `tunnel_repaired`, `tunnel_rejoined_balancer`, `sync_failed`, and sfc
  `refresh_tick`, `subscription_refreshed`. Events new in the Go version are
  listed in [`docs/CONFIG-MIGRATION.md`](docs/CONFIG-MIGRATION.md) («Логи»).

## Known limitations

Of Xray v26.10.10, documented rather than worked around in code:

- TLS outbounds trust only Xray's built-in CA bundle. Private CAs installed in
  the image are not used. `allowInsecure` no longer exists, so use `pcs` or
  `vcn` instead.
- SOCKS UDP ASSOCIATE replies with an ephemeral port. UDP through generated
  tunnels does not work from outside Docker through a published port.
  TCP and scd monitoring are unaffected; use `network_mode: host` if you need it.
- A Lua `routing.script` in the static Xray config overrides all rules,
  including scd's. This setup is not supported.

Of scd:

- A static routing rule **without** `ruleTag` cannot be listed in
  `fixedRouting`, so scd tries `RemoveRule("")` on it. Xray rejects that with
  `app/router: empty tag name!` and routing apply for that target fails. This
  behaviour is kept on purpose. Give every static rule a `ruleTag` and list it
  in `fixedRouting`.
- Ownership, hash and monitor state is memory-only. Overlap protection does
  not coordinate separate processes.

## Migration notes

Full guide: [`docs/CONFIG-MIGRATION.md`](docs/CONFIG-MIGRATION.md).

- One binary and one image. The `ghcr.io/a1ekseev/sfc` and
  `ghcr.io/a1ekseev/gtl` images are no longer published; their last tags are
  the old version. Use `ghcr.io/a1ekseev/scd` with the `command:` values from
  [Docker](#docker).
- Config changes: label filters use RE2 with `labelExcludeRegex` (rewrite
  lookahead filters). New keys are `allowInsecure` (default `deny`),
  `fetchUserAgent` (default UA is now `scd/<version>`), `fetchHeaders` and
  `balancerSelection`. Validation is stricter in places, and errors carry
  field paths.
- The first sync after the upgrade fully re-applies every target once.
  Manifest `jsonOutbound` now uses the Xray v26.10.10 form, so hashes change.
  Tags of the old profiles stay the same.
- Tunnel SOCKS ports may be reassigned. They are internal to scd. Make sure
  `portRange` is big enough, otherwise that target fails with a clear message.
- Cron: see [Cron schedules](#cron-schedules). The formats that changed
  compared with the previous parser are `@minutely`, dow `7`, `L`/`#`/`H`, and
  an empty sfc schedule.
- Status server routing ignores the query string. Sorting is byte-wise.
  Remote `lastError` texts are Go texts.
- `scd sfc serve` no longer runs a second refresh right after the initial one.
  sfc outputs may now include xhttp, ML-KEM and REALITY-`pqv` lines that the old
  version dropped silently.

## Development

```bash
go vet ./... && go vet -tags 'xraylive visual' ./...
go test -race ./...
golangci-lint run                      # v2.15.0, config .golangci.yml
go build -o bin/scd ./cmd/scd
bash scripts/check-xray-version.sh     # .xray-version == go.mod xray-core == compose image
bash scripts/check-no-node.sh          # Go-only repository

# Live tests against a real Xray v26.10.10 binary
SCD_XRAY_BIN=/path/to/xray go test -tags xraylive -count=1 ./internal/scd/livetest/...
# Dashboard visual regression (headless Chromium)
SCD_CHROME=/path/to/chrome go test -tags visual -count=1 ./internal/scd/status/...
```

Without the environment variable, the tagged tests skip. Reference outputs of
the previous implementation are in [`testdata/reference/`](testdata/reference/README.md).
