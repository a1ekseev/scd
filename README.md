# scd

`scd` is one Go binary with three services:

| Command | Service | What it does |
|---|---|---|
| `scd …` | **scd** — Subscription Control Daemon | Turns VLESS subscriptions into Xray outbounds (plus SOCKS inbounds and routing rules per tunnel), applies them to Xray over the gRPC API, monitors and repairs tunnels, and serves a read-only status dashboard and JSON API. |
| `scd sfc …` | **sfc** — Subscription Filter Control | Downloads upstream subscriptions, keeps VLESS lines whose label matches per-output filters, repacks them to base64 and serves them by public URL. Does not talk to Xray. |
| `scd gtl …` | **gtl** — Gen Test Load | HTTP server that streams a random payload of the requested size, for download/load tests. |

There is one image, `ghcr.io/a1ekseev/scd`, for all three. The target Xray version is **v26.10.21**.

Upgrading from the pre-Go version: config keys changed. See
[Migration notes](#migration-notes).

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

Requires Go 1.27 (`go.mod`: `go 1.27.0`, `toolchain go1.27.3`).

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
| `scd sync --config f` | Run one sync and print the JSON report. Exit code 1 when `failed > 0`; orphans are checked first, so an unreachable Xray fails the sync even when no manifest changed. On SIGINT/SIGTERM the Xray apply in flight finishes, the remaining subscriptions and targets are skipped, `sync_canceled` (warn) is logged, the partial report is printed and the exit code is 130 (SIGINT) or 143 (SIGTERM). |
| `scd daemon --config f` | Run sync, monitors, balancer selection, the status server and remote status polling on cron schedules. Needs `runtime.mode: daemon`. |
| `scd generate --source-id <id> [--type vless\|wireguard] [--endpoint-domain-strategy <s>] --input <file\|url\|-> --output m.json --log log.json` | Low-level: build the outbound manifest and parse log from one source without touching Xray. `--source-id` (required) is the tag prefix, as `sources[].id` in the config; `--source-id vless` gives the tags of earlier versions. `--type wireguard` reads a candidate file (a private file, see [WireGuard sources](#wireguard-sources)), needs `--endpoint-domain-strategy` and builds every candidate; keys are written as `<redacted>`. |
| `scd sfc serve\|refresh\|validate-config\|print-config --config f` | sfc: serve with background refresh / one refresh with JSON report / validate / print (`input`, `fetchHeaders` and output ids redacted unless `--include-secrets`; an output id is the secret part of its public URL, so the `refresh` report and logs name outputs by `name` instead). An interrupted `refresh` prints its partial report and exits 130/143 like `scd sync`. |
| `scd gtl serve\|validate-config\|print-config --config f` | gtl: serve / validate (`Config is valid`) / print. |

None of the services hot-reload the config. Restart after a change.
`SIGINT`/`SIGTERM` stop all loops right away. Operations already in flight
finish first: Xray apply/repair/rejoin, the current sfc refresh, open HTTP
requests (up to 10 s), and pending remote pings. A second signal exits at once.

## Docker

The image is `ghcr.io/a1ekseev/scd:latest` or `:<X.Y.Z>`, for `linux/amd64` and
`linux/arm64`. It is published from release tags `vX.Y.Z`. The image is distroless
(`gcr.io/distroless/static-debian13:nonroot`: CA certificates and tzdata, no
shell). It runs as the non-root user `65532:65532` with
`CMD ["scd", "daemon", "--config", "/app/config/config.yml"]`. Choose the
service with `command:`:

| Service | `command:` |
|---|---|
| scd | default CMD |
| sfc | `["scd", "sfc", "serve", "--config", "/app/config/config.yml"]` |
| gtl | `["scd", "gtl", "serve", "--config", "/app/config/config.yml"]` |

The config only needs to be readable. sfc writes its source cache to
`/app/config/.sfc-cache`: a new named volume gets the right owner from the
image, but a bind mount or a volume created by an older root image must be
owned by `65532:65532` (`chown -R 65532:65532 <dir>`). Otherwise sfc keeps
serving but logs `subscription_source_cache_write_failed` and has no fallback
cache. Ports below 1024 are not available to the non-root user.

The root `docker-compose.yml` runs Xray `ghcr.io/xtls/xray-core:26.10.21` and
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
    enabled: true
    sources:                     # each source is loaded, filtered and applied on its own
      - id: vless                # tag prefix: vless-…, in-vless-…, route-vless-…
        type: vless
        input: https://panel.example.com/sub/<token>   # file path, http(s) URL or "-" (stdin)
        enabled: true
        format: auto             # auto | plain | base64
        fetchTimeoutMs: 5000
        allowInsecure: deny      # deny | allow (links with allowInsecure=1)
        # fetchUserAgent: scd/1.0  # default scd/<version>; read the warning below
        # fetchHeaders:
        #   x-hwid: "${SCD_HWID}"  # values are secrets
        maxInputBytes: 67108864  # 1 KiB..64 MiB
        portRange: {start: 20000, end: 20099}
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
| `subscriptions[]` | Subscriptions: a list of sources and one Xray target. At least one must be enabled. `target.address` is unique, so a subscription is one Xray; a source `id` is unique within its subscription. An enabled subscription without an enabled source gets a warning: sync removes every non-fixed resource of its target. | — |
| `.id` / `.enabled` | Subscription id used in logs (`subscriptionId`), reports and the dashboard; on/off. | required / `true` |
| `.sources[]` | At least one source. Each is loaded, filtered and applied separately; its tunnels join the same target. | required |
| `.sources[].id` | Source id: `[a-z0-9-]`, 1..16 characters, no `-` at either end. It prefixes every generated tag and is `sourceId` in logs and reports. `target.address` is unique; a source `id` is unique within its subscription (one Xray), and there no `<id>-` may start another id or overlap `observatorySubjectSelectorPrefix`. | required |
| `.sources[].type` | `vless` or `wireguard` ([WireGuard sources](#wireguard-sources)). | required |
| `.sources[].input` | File path (relative to the config file), `http(s)` URL, or `-` for stdin. A `wireguard` source takes an absolute file path only. | required |
| `.sources[].enabled` / `.format` / `.fetchTimeoutMs` | On/off (a disabled source's resources are removed as orphans); decoding `auto\|plain\|base64`; download timeout (does not apply to Xray calls). `format`, `fetchTimeoutMs`, `allowInsecure`, `fetchUserAgent` and `fetchHeaders` are `vless` only. | `true` / `auto` / `5000` |
| `.sources[].allowInsecure` | Policy for links with `allowInsecure=1` and neither `pcs` nor `vcn`. `deny` skips the link. `allow` accepts it, but Xray v26.10.21 verifies the certificate anyway; logs warning `allow_insecure_ignored`. | `deny` |
| `.sources[].fetchUserAgent`, `.fetchHeaders` | Request `User-Agent` and extra request headers, see [Fetching subscriptions](#fetching-subscriptions). | `scd/<version>`, none |
| `.sources[].maxInputBytes` | Size limit of the input (file, `http(s)` and stdin), `1024..67108864`. | 64 MiB |
| `.sources[].portRange.start/end` | Inclusive port range (`1..65535`) of the source's SOCKS inbounds. Required when inbounds or routing are enabled. The ranges of one subscription must not overlap. | — |
| `.sources[].filters.countryAllowlist` | ISO2 codes. Entries without a recognized country are dropped. For `wireguard`, the exit country. | no filter |
| `.sources[].filters.entryCountryAllowlist` | `wireguard` only: ISO2 codes of the entry country. | no filter |
| `.sources[].maxActive`, `.selection`, `.wireguard` | `wireguard` only, required, see [WireGuard sources](#wireguard-sources). | — |
| `.sources[].monitor.request.timeoutMs`, `.monitor.retry.{attempts,delayMs}` | Any type: override `target.monitor` for the source's tunnels. See [Monitoring and repair](#monitoring-and-repair). | the target's |
| `.sources[].filters.labelIncludeRegex`, `.labelExcludeRegex` | Label filters, see [Label filters](#label-filters). | no filter |
| `.target.address` | Xray gRPC API `host:port`. | required |
| `.target.timeoutMs` | Timeout of each Xray API call. | `5000` |
| `.target.fixedOutbounds` / `fixedInbounds` / `fixedRouting` | Outbound/inbound tags and routing `ruleTag`s that scd never deletes. A fixed tag must not lie in a source namespace (`<id>-…`, `in-<id>-…`, `route-<id>-…`). | `[]` |
| `.target.visionUdp443Override` | Rewrite REALITY `xtls-rprx-vision` to `xtls-rprx-vision-udp443` on this target. | `false` |
| `.target.observatorySubjectSelectorPrefix` | Prefix added to every applied outbound tag, so that the static `observatory.subjectSelector` / `balancers[].selector` match them. | none |
| `.target.inboundSocks.listen` | IP address or `localhost` for the generated SOCKS inbounds; the ports come from `sources[].portRange`. Required when inbounds or routing are enabled or the monitor is on. | — |
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
is reported instead of silently ignored. The subscription form without
`sources` is gone: `input`, `format`, `filters`, `fetchTimeoutMs`,
`fetchUserAgent`, `fetchHeaders` and `allowInsecure` directly in a
subscription, and `target.inboundSocks.portRange`, are unknown keys.
Validation errors list every problem with its field path, for example:
`subscriptions.0.target.inboundSocks.listen: must be an IP address or "localhost" (Xray cannot listen on a domain)`.

### WireGuard sources

A source with `type: wireguard` reads a local candidate file with one
`wireguard://` line per candidate and puts at most `maxActive` of them into
Xray as WireGuard outbounds.

```yaml
      - id: wg
        type: wireguard
        input: /app/wgp/wg     # absolute path, mode 0600 (no group or other access)
        portRange: {start: 20100, end: 20109}   # at least maxActive ports
        maxActive: 2
        selection:
          strategy: score          # score | load | label
          switchMarginPercent: 20  # 0..100
          switchConfirmChecks: 2   # 1..10
          deadPairCooldownMinutes: 10   # >= 4
          replaceAfterFailures: 2  # 1..10
        monitor:                   # optional, any type
          request: {timeoutMs: 10000}
          retry: {attempts: 2, delayMs: 2000}
        filters:
          countryAllowlist: [NL]       # exit country
          entryCountryAllowlist: [NL]  # entry country, checked first
        wireguard:
          endpointDomainStrategy: UseIPv4   # AsIs | UseIP | UseIPv4 | UseIPv6 | UseIPv4v6 | UseIPv6v4
```

- **Input.** An absolute path to a regular file that group and others cannot
  access; `http(s)`, `-` and relative paths are errors. Otherwise the source
  fails with `reasonCode: wireguard_input_refused` in `subscription_fetch_failed`
  and its tunnels stay as they are. `wireguard` sources need
  `resources.inbounds` and `resources.routing`.
- **Line format.** `wireguard://<privateKey>@<host>:<port>?<params>#<label>`.
  `provider` comes first and selects the required parameters; every line needs
  `publickey`, `address`, `mtu` (576..1500) and `allowedips`; optional are
  `dns` (IPs only), `reserved` (3 numbers), `keepalive`, `presharedkey` and
  the metadata `server`, `entry`, `exit`, `load`, `score`, `expires`. A line
  with an unknown provider or parameter, a repeated or empty parameter or a
  broken rule is skipped as `wireguard_invalid`; the detail names the field,
  never the value. Lines of other schemes are `unsupported_scheme`, and
  `wireguard://` and `masque://` lines in a `vless` source are `unsupported_scheme` too, with `raw` cut to `<scheme>://<redacted>`.
- **Tags.** `<id>-<cc>-<city>-wg-<sha8>`; `cc` and `city` come from the label
  (the endpoint host without a city), `sha8` from the public tunnel fields
  only (the interface public key instead of the private key, a hash of the
  preshared key). The label and the metadata are not part of `sha8`.
- **Selection.** Candidates are filtered by entry country (`entry`, else the
  label flag), then exit country (`exit`, else the label flag), then label;
  a candidate without a country fails a non-empty country filter. They are
  ranked by `selection.strategy`: `score` (score, load, label, tag), `load`
  (load, score, label, tag) or `label` (label, tag); a candidate without the
  sort key comes after those with it. The first `maxActive` are active, never
  two with the same private key; slot `i` of the ranking gets port
  `portRange.start + i`. The others stay out of Xray.
- **Apply.** The source is applied in full (remove the source's resources,
  then add, kind by kind, like a VLESS source) on the first sync after a
  start, when `type`, `portRange`, `inboundSocks.listen`, the observatory
  prefix, `endpointDomainStrategy` or `maxActive` change, after a failed
  replacement, and on `xray_state_drift`. Otherwise each sync changes single
  slots only (see "Monitoring and repair"). Xray never holds more than `maxActive`
  outbounds of the source, at any RPC of a sync, a rollback, a repair, a
  rejoin or a replacement: before each `AddOutbound` of the source scd lists
  Xray's outbounds and does not add when the source already has `maxActive`
  of them (both tags count, with and without the observatory prefix), so
  this holds even after a lost RPC answer. Such a refusal, and a failed full
  apply, make the next sync apply the source in full; after a failed full
  apply the source's tunnels are not repaired or replaced until then.
- **Outbound.** `noKernelTun: true` always (Xray never touches the host's
  routes), `streamSettings.sockopt.domainStrategy` from
  `wireguard.endpointDomainStrategy`, `reserved` as an array.
- **Secrets.** The private and preshared keys never appear in logs, sync
  reports, `generate` output, `print-config`, the status API or the dashboard:
  JSON shows `<redacted>`, the manifest `raw` has `redacted` in their place,
  skipped lines have no `raw`. `/api/runtime-state` never returns `rawBase64`
  or keys of a WireGuard outbound, even with `includeRaw` and
  `includeSecrets`; its `expected` part is the active set of the last sync.
- **Status.** A WireGuard tunnel has `protocol: "wireguard"` in `/api/status`
  and `/api/instance-status`; `targets[].sources[]` of a `wireguard` source
  adds `maxActive`, `activeCount` and `candidateCount`.

### Sync

- Every source owns four tag namespaces on its target:
  `<observatorySubjectSelectorPrefix><id>-…` and `<id>-…` (outbounds, in and
  out of the balancer), `in-<id>-…` (SOCKS inbounds) and `route-<id>-…`
  (routing rules). Tags are `<id>-<cc>-<city>-<profile>-<sha8>`; a source with
  `id: vless` keeps the tags of earlier scd versions.
- Every sync first removes **orphans** on each target: non-fixed resources
  outside the namespace of every enabled source (tags of a removed or
  disabled source, tags from before a source id change), in the order
  routing rules → inbounds → outbounds, for the enabled resource kinds.
  Routing rules without a `ruleTag` are never touched. If an orphan cannot be
  removed (or Xray cannot be reached), scd logs `orphan_remove_failed` and
  applies nothing to that target on this sync; removed orphans are logged as
  `orphans_removed`. Because of this check, `scd sync` exits with an error
  when Xray is unreachable, even if no manifest changed.
- Then each enabled source is applied on its own, in id order: outbound →
  inbound → routing. In each kind scd removes only that source's resources,
  then adds the new ones. Fixed resources and other sources' resources are
  never touched. If an add fails, the source's outbounds are rolled back
  best-effort. A source's hashes are stored only when all its kinds succeed.
- When a source's manifest hash is unchanged, sync still lists the target's
  outbounds, inbounds and rules (read-only). If a resource of that source is
  missing, for example after an Xray restart, it logs warning
  `xray_state_drift` with per-kind counts and applies the source again. A
  repaired outbound (without the observatory prefix) is not a loss.
- Topology: one outbound per filtered entry. With inbounds and routing
  enabled, each outbound also gets one SOCKS inbound (`in-…`) and one routing
  rule (`route-…`). Ports are allocated sequentially from the source's
  `portRange` in tag order. If the range is too small, the first entries in
  tag order get the ports and the rest are skipped with reason code
  `port_range_exhausted` (in the sync report, plus warning
  `manifest_entry_warning` per line); the source itself is applied.
- Hashes are memory-only, so the first sync after a restart always applies
  every source.
- Empty input, or input with no parsed lines, fails closed for that source:
  it is marked failed, its resources in Xray stay as they are and it gets no
  mutating API calls. A failed source does not touch the other sources.
- Filters run once per source after parsing, in this order: `countryAllowlist`,
  `labelIncludeRegex`, `labelExcludeRegex`. The report counts `filtered`
  separately from `skipped` (parse/validation).
- Sync, repair, rejoin and candidate replacement share one per-target mutex. A cron slot that
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
- Repair and rejoin, like sync, remove before they add: first the tunnel's
  rule and outbound, then the new outbound and rule. Xray never holds two
  copies of a tunnel at once (some providers limit simultaneous connections
  per key); for that moment the tunnel has no outbound. If the old outbound
  cannot be removed, scd adds nothing, restores the old rule (best effort)
  and logs `tunnel_repair_failed` or `tunnel_rejoin_failed`; an outbound
  that is already gone is not an error. If a rejoin add
  fails, scd restores the unprefixed outbound and rule (best effort) and
  logs `tunnel_rejoin_failed`.
- A tunnel that is already out of the balancer is not repaired again: further
  failed checks only update its monitor state (`degraded`, `lastError` = the
  probe error, the failure counter) and log `tunnel_repair_skipped` at debug.
  Xray is not touched until the tunnel passes a check (rejoin) or a sync
  replaces the topology (manifest change or `xray_state_drift`), after which a
  failure repairs it once again. A failed repair is retried at the next failed
  check. Without `observatorySubjectSelectorPrefix` there is no balancer to
  leave, so every final failure still recreates the outbound.
- `sources[].monitor.request.timeoutMs` and `sources[].monitor.retry.{attempts,
  delayMs}` override the target's values for the tunnels of that source (any
  type); unset keys keep the target's. A WireGuard failure shows only as a
  timeout or reset and the first handshake can be slow, so for `wireguard`
  sources use `timeoutMs` ≥ 10000 and `attempts` ≥ 2.
- **Candidate replacement** (`wireguard`). Once `replaceAfterFailures` is
  reached, a failed tunnel is replaced instead of repaired; it is repaired
  only when no replacement exists. A slot keeps its port; the
  replacement is the first ranked candidate that is not active, does not
  share a private key with another active one and whose key + endpoint pair
  is not in quarantine. It removes the old tunnel first (rule, outbound,
  inbound; both outbound tags, with and without the observatory prefix,
  are removed), then adds the new one (outbound with the observatory prefix,
  inbound, rule), under the same per-target mutex as sync, repair and
  rejoin. A slot is replaced:
  - on a sync, when its candidate is gone from the file or changed
    (`reasonCode: gone|changed`);
  - after `replaceAfterFailures` failed checks in a row (a repair does not
    reset this count, only a successful check does): the pair goes into
    quarantine for `deadPairCooldownMinutes` (`reasonCode: failed`); while a
    tunnel without a replacement keeps failing, every failed check puts its
    pair into quarantine again, so the pair stays out as long as it fails;
  - on a sync, when a healthy active candidate is beaten by at least
    `switchMarginPercent` % on `switchConfirmChecks` syncs in a row
    (`reasonCode: better`). The metric is `score` (`load` for `strategy: load`),
    the other one if either candidate lacks it; without a metric, and always
    with `strategy: label`, nothing switches. A full apply starts these
    series over.
  An empty slot (fewer tunnels than `maxActive`) is filled on a sync by the
  first allowed candidate, on the lowest free port: outbound, inbound, rule
  are added, the source's other tunnels are not touched (`reasonCode: vacant`,
  no `fromTag`; a failed step removes what it added).
  Each replacement logs `candidate_replaced` (`fromTag`, `toTag`, `reasonCode`).
  If the old outbound cannot be removed, nothing is added. A failed removal
  of the old rule does not stop a replacement; it is logged at warn as
  `candidate_replace_failed` with `stage: remove_rule`. Any failed step
  logs `candidate_replace_failed`, leaves the slot empty and makes the next
  sync apply the source in full.
- **No replacement.** A failed tunnel with no candidate outside quarantine
  stays in repair (out of the balancer, still checked, rejoined when it
  recovers); a candidate gone from the file with none to replace it is
  removed (`candidate_removed`) and its slot stays empty. Both show the
  source warning `no_replacement` in `targets[].sources[]` and on the
  dashboard; `candidate_no_replacement` is logged once when a tunnel enters
  that state. Quarantine and the hysteresis and failure counts are in memory
  only.
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
| `/api/runtime-state?subscriptionId=…&targetAddress=…` | Diagnostic view from the loaded config and live Xray API data. `expected` is the union of the enabled sources; with several sources `expected.sources[]` gives `{sourceId, source, error?}` per source. |

Routing looks only at the path, so `/api/status?x=1` returns JSON. The JSON
schemas are a contract and grow only by adding fields; `version` stays `1`. A
panic in a handler returns 500 and logs `http_handler_panic`. The dashboard is
read-only and has no links to the JSON endpoints.

A target whose subscription has several sources, a `wireguard` source or a
source with a warning, gets `targets[].sources[]` =
`{sourceId, type, enabled, maxActive?, activeCount?, candidateCount?, warnings?[]{code, message}}`
in source id order, and its tunnels get `sourceId`; a WireGuard tunnel also
has `protocol: "wireguard"` (VLESS tunnels have no `protocol`). The dashboard then shows a
Sources card with the warnings (for example `port_range_exhausted`) and the
source next to each tunnel endpoint. A subscription with one source and no
warning has neither key, as before.

`/api/runtime-state` hides secrets unless `includeSecrets: true`. Hidden values
include subscription and monitor/push URLs (shown as `<scheme>://<host>/<redacted>`),
`fetchHeaders` values, UUIDs and keys, outbound `path` and the xhttp `host`,
which are `<redacted>`. `includeRaw: true` adds `rawBase64` protobuf blobs
only together with `includeSecrets: true`, because the blobs contain everything
unredacted. WireGuard outbounds never get `rawBase64`, and their `parsed`
never shows the private or preshared key.

## Label filters

Filters use Go `regexp` (RE2: linear time, no lookaround). They apply the same
way in scd (`subscriptions[].sources[].filters`) and sfc (`outputs[]`):

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
combination and parameter that Xray v26.10.21 accepts is accepted and reaches
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

`fetchUserAgent` (scd `subscriptions[].sources[]`, sfc `subscriptions[]`) sets the `User-Agent` for
downloads from `http(s)` URLs. The default is `scd/<version>`.

> **Warning:** some panels choose the response format by User-Agent. With
> Remnawave default rules, `scd/…`, curl and similar clients get base64 share
> links, which is what scd and sfc need. A UA like `Happ/…` or `INCY/…`
> appends `?serverDescription=…` to every label, and that text ends up in the
> city and the tag. `clash*`, `mihomo`, `stash` and `sfa…`/`singbox` get
> YAML or JSON instead of share links, and the source then yields no entries.

`fetchHeaders` (scd `subscriptions[].sources[]`, sfc `subscriptions[]`) adds request headers. An example
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
  `content-type: application/octet-stream` and `content-length`; no other
  custom headers.
- A bad size, or a size above `maxSizeKb`, returns `400`. Other paths return
  `404`. Methods other than `GET` return `405`. Unknown config keys are errors.
- The payload is an AES-128-CTR keystream, streamed in 64 KiB chunks: the key
  is random per process (memory only, new on every restart), the IV random per
  request. Every body is different and incompressible, so nothing in the path
  (e.g. a TLS-terminating CDN) sees repeated content. Streaming stops when the
  client disconnects.

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
  `subscription_fetched`, `orphans_removed`, `orphan_remove_failed`,
  `manifest_built`, `apply_finished`, `daemon_tick`,
  `tunnel_repaired`, `tunnel_rejoined_balancer`, `sync_failed`, and sfc
  `refresh_tick`, `subscription_refreshed`.

## Known limitations

Of Xray v26.10.21, documented rather than worked around in code:

- TLS outbounds trust only Xray's built-in CA bundle. Private CAs installed in
  the image are not used. `allowInsecure` no longer exists, so use `pcs` or
  `vcn` instead.
- SOCKS UDP ASSOCIATE replies with an ephemeral port. UDP through generated
  tunnels does not work from outside Docker through a published port.
  TCP and scd monitoring are unaffected; use `network_mode: host` if you need it.
- A Lua `routing.script` in the static Xray config overrides all rules,
  including scd's. This setup is not supported.

Of scd:

- Ownership, hash and monitor state is memory-only. Overlap protection does
  not coordinate separate processes.

## Migration notes

- One binary and one image. The `ghcr.io/a1ekseev/sfc` and
  `ghcr.io/a1ekseev/gtl` images are no longer published; their last tags are
  the old version. Use `ghcr.io/a1ekseev/scd` with the `command:` values from
  [Docker](#docker).
- Subscriptions now have `sources` (breaking, no compatibility layer): move
  `input`, `format`, `fetchTimeoutMs`, `fetchUserAgent`, `fetchHeaders`,
  `allowInsecure` and `filters` into `sources[0]` with `id: vless` and
  `type: vless`, and `target.inboundSocks.portRange` into
  `sources[0].portRange`. With `id: vless` the tags and ports stay the same;
  with another id the old resources are removed as orphans by the first sync.
  The source input is now limited to 64 MiB for files and stdin too
  (`maxInputBytes`). `scd generate` needs `--source-id`. In logs and sync
  reports `sourceId` is now the source id (the subscription id stays in
  `subscriptionId`).
- Config changes: label filters use RE2 with `labelExcludeRegex` (rewrite
  lookahead filters). New keys are `allowInsecure` (default `deny`),
  `fetchUserAgent` (default UA is now `scd/<version>`), `fetchHeaders` and
  `balancerSelection`. Validation is stricter in places, and errors carry
  field paths.
- The first sync after the upgrade fully re-applies every target once.
  Manifest `jsonOutbound` now uses the Xray v26.10.21 form, so hashes change.
  Tags of the old profiles stay the same.
- Tunnel SOCKS ports may be reassigned. They are internal to scd. Make sure
  `portRange` is big enough, otherwise the entries beyond it are skipped
  (`port_range_exhausted`).
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

# Live tests against a real Xray v26.10.21 binary
SCD_XRAY_BIN=/path/to/xray go test -tags xraylive -count=1 ./internal/scd/livetest/...
# Dashboard visual regression (headless Chromium)
SCD_CHROME=/path/to/chrome go test -tags visual -count=1 ./internal/scd/status/...
```

Dashboard blocks without a reference of the previous implementation are
compared with local screenshots in `internal/scd/status/testdata/visual/`
(gitignored, never committed); add `-update` to the visual test command to
create them. A test without its screenshot is skipped.

Without the environment variable, the tagged tests skip. Reference outputs of
the previous implementation are in [`testdata/reference/`](testdata/reference/README.md).
