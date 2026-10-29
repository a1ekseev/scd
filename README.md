# scd

`scd` is one Go binary with four services:

| Command | Service | What it does |
|---|---|---|
| `scd …` | **scd** — Subscription Control Daemon | Turns VLESS subscriptions into Xray outbounds (plus SOCKS inbounds and routing rules per tunnel), applies them to Xray over the gRPC API, monitors and repairs tunnels, and serves a read-only status dashboard and JSON API. |
| `scd sfc …` | **sfc** — Subscription Filter Control | Downloads upstream subscriptions, keeps VLESS lines whose label matches per-output filters, repacks them to base64 and serves them by public URL. Does not talk to Xray. |
| `scd gtl …` | **gtl** — Gen Test Load | HTTP server that streams a random payload of the requested size, for download/load tests. |
| `scd wgp …` | **wgp** — WireGuard provider | Keeps VPN provider accounts and keys and publishes WireGuard and MASQUE candidate files for scd sources. Does not talk to Xray or scd. |

There is one image, `ghcr.io/a1ekseev/scd`, for all four. The target Xray version is **v26.10.21**.

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
- [scd wgp](#scd-wgp)
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
| `scd generate --source-id <id> [--type vless\|wireguard\|masque] [--endpoint-domain-strategy <s>] --input <file\|url\|-> --output m.json --log log.json` | Low-level: build the outbound manifest and parse log from one source without touching Xray. `--source-id` (required) is the tag prefix, as `sources[].id` in the config; `--source-id vless` gives the tags of earlier versions. `--type wireguard` reads a candidate file (a private file, see [WireGuard sources](#wireguard-sources)), needs `--endpoint-domain-strategy` and builds every candidate; `--type masque` reads a MASQUE candidate file the same way, without `--endpoint-domain-strategy`. Keys are written as `<redacted>`. |
| `scd sfc serve\|refresh\|validate-config\|print-config --config f` | sfc: serve with background refresh / one refresh with JSON report / validate / print (`input`, `fetchHeaders` and output ids redacted unless `--include-secrets`; an output id is the secret part of its public URL, so the `refresh` report and logs name outputs by `name` instead). An interrupted `refresh` prints its partial report and exits 130/143 like `scd sync`. |
| `scd gtl serve\|validate-config\|print-config --config f` | gtl: serve / validate (`Config is valid`) / print. |
| `scd wgp serve\|validate-config\|print-config --config f`, `scd wgp login\|logout --provider <id> --config f` | wgp: serve / validate (like `scd validate-config`) / print (paths of secret files, never their contents) / interactive login and logout of a provider that supports them; other providers fail with `provider "<id>" (type "<type>") does not support login`. See [scd wgp](#scd-wgp). |

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
| wgp | `["scd", "wgp", "serve", "--config", "/app/config/config.yml"]` |

The config only needs to be readable. sfc writes its source cache to
`/app/config/.sfc-cache`: a new named volume gets the right owner from the
image, but a bind mount or a volume created by an older root image must be
owned by `65532:65532` (`chown -R 65532:65532 <dir>`). Otherwise sfc keeps
serving but logs `subscription_source_cache_write_failed` and has no fallback
cache. wgp writes its state to `/app/state` (mode `0700`) and its candidate
outputs to `/app/out`; the image creates both owned by `65532:65532`, the same
rule applies to bind mounts. Ports below 1024 are not available to the
non-root user.

The root `docker-compose.yml` runs Xray `ghcr.io/xtls/xray-core:26.10.21` and
all four services on one locally built image. It mounts the git-ignored
`docker/<svc>/config.yml` files. To start from the examples, run
`cp docker/<svc>/config.example.yml docker/<svc>/config.yml`.

wgp and scd share only the named volume `wgp-out`: wgp mounts it read-write
at `/app/out`, scd read-only at `/app/wgp` (the `input` of its `wireguard` and
`masque` sources). Both run as `65532`, so the `0600` files wgp writes there
are readable by scd and by nobody else. wgp is on its own network
`wgp-egress` (internet only, nothing shared with scd or Xray) and runs with a
read-only root, no capabilities and `no-new-privileges`. Its status page is
published on `127.0.0.1:9093`. Before the first start, on the host:

```bash
mkdir -p secrets docker/wgp/state docker/wgp/static
head -c 32 /dev/urandom | base64 > secrets/wgp_state_key
chmod 0400 secrets/wgp_state_key && chmod 0700 docker/wgp/state
chmod 0600 docker/wgp/static/*.conf
sudo chown 65532:65532 secrets/wgp_state_key docker/wgp/state docker/wgp/static/*.conf
```

The state key is the compose secret `wgp_state_key`. Outside swarm, compose
bind-mounts a secret file with its host owner and mode, so it must be owned
by `65532` with mode `0400` or `0600`; the same goes for the state directory
(`0700`) and the static `.conf` files (no access for others). Docker Desktop
on macOS shows bind-mounted files as owned by the reading user, so there the
`chown` is not needed. `secrets/`, `docker/wgp/state/` and
`docker/wgp/static/` (the `.conf` files hold private keys) are git-ignored.

```bash
docker compose up -d --build
docker compose logs -f scd
docker compose down
bash scripts/compose-smoke.sh   # self-contained smoke test on the example configs
```

wgp is in the compose profile `wgp`: a plain `docker compose up -d` starts
Xray, scd, sfc and gtl only and needs no wgp config, state or key (scd then
sees an empty `wgp-out` volume). Start wgp with
`docker compose --profile wgp up -d` or `COMPOSE_PROFILES=wgp`; the smoke
enables the profile itself.

Published ports: the scd dashboard at <http://127.0.0.1:9090/>, sfc at
<http://127.0.0.1:9091/s/test-all>, gtl at <http://127.0.0.1:9092/load/32>,
and the wgp status page at <http://127.0.0.1:9093/>.
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
| `.sources[].type` | `vless`, `wireguard` ([WireGuard sources](#wireguard-sources)) or `masque` ([MASQUE sources](#masque-sources)). | required |
| `.sources[].input` | File path (relative to the config file), `http(s)` URL, or `-` for stdin. A `wireguard` or `masque` source takes an absolute file path only. | required |
| `.sources[].enabled` / `.format` / `.fetchTimeoutMs` | On/off (a disabled source's resources are removed as orphans); decoding `auto\|plain\|base64`; download timeout (does not apply to Xray calls). `format`, `fetchTimeoutMs`, `allowInsecure`, `fetchUserAgent` and `fetchHeaders` are `vless` only. | `true` / `auto` / `5000` |
| `.sources[].allowInsecure` | Policy for links with `allowInsecure=1` and neither `pcs` nor `vcn`. `deny` skips the link. `allow` accepts it, but Xray v26.10.21 verifies the certificate anyway; logs warning `allow_insecure_ignored`. | `deny` |
| `.sources[].fetchUserAgent`, `.fetchHeaders` | Request `User-Agent` and extra request headers, see [Fetching subscriptions](#fetching-subscriptions). | `scd/<version>`, none |
| `.sources[].maxInputBytes` | Size limit of the input (file, `http(s)` and stdin), `1024..67108864`. | 64 MiB |
| `.sources[].portRange.start/end` | Inclusive port range (`1..65535`) of the source's SOCKS inbounds. Required when inbounds or routing are enabled. The ranges of one subscription must not overlap. | — |
| `.sources[].filters.countryAllowlist` | ISO2 codes. Entries without a recognized country are dropped. For `wireguard` and `masque`, the exit country. | no filter |
| `.sources[].filters.entryCountryAllowlist` | `wireguard` and `masque` only: ISO2 codes of the entry country. | no filter |
| `.sources[].maxActive`, `.selection` | `wireguard` and `masque` only, required, see [WireGuard sources](#wireguard-sources). | — |
| `.sources[].wireguard` | `wireguard` only, required. | — |
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

### MASQUE sources

A source with `type: masque` reads a local candidate file with one
`masque://` line per candidate and puts at most `maxActive` of them into
Xray as MASQUE outbounds (CONNECT-IP over HTTP/3 or HTTP/2). Everything not
listed here works as for [WireGuard sources](#wireguard-sources): input rules
(`masque_input_refused`), filters, ranking, apply, the `maxActive` limit,
replacement, quarantine and hysteresis, secrets and status
(`protocol: "masque"`). A `masque` source has no `wireguard` section.

```yaml
      - id: mq
        type: masque
        input: /app/wgp/warpmq # absolute path, mode 0600
        portRange: {start: 20110, end: 20119}
        maxActive: 1
        selection:
          strategy: label          # score | load | label
          switchMarginPercent: 20
          switchConfirmChecks: 2
          deadPairCooldownMinutes: 10
          replaceAfterFailures: 2
        monitor:
          request: {timeoutMs: 10000}
          retry: {attempts: 2, delayMs: 2000}
```

- **Line format.**
  `masque://<privateKey>@<ip>:<port>?provider=…&peerkey=…&address=…&sni=…&transport=h3|h2&dns=…&priority=<n>[&entry=…&exit=…&expires=…]#<label>`.
  The device key is an ECDSA P-256 PKCS #8 key and `peerkey` the PKIX P-256
  key of the endpoint, both base64url without padding; `address` holds at
  most one IPv4 and one IPv6; `sni` is a hostname or the endpoint IP (then no
  SNI is sent); `dns` are IPs. A broken line is skipped as `masque_invalid`
  (the detail names the field, never the value).
- **Transports.** One device key has one line per transport (`h3`, `h2`),
  on the same endpoint, that differ only in `transport` and `priority`
  (0 first). One tunnel per device key is active. A new tunnel for a key (the
  first sync, a replacement) takes the line with the lowest `priority` whose
  key + endpoint + transport triple is not in quarantine. When the monitor
  replaces a failed `h3` tunnel (UDP blocked, for example), its triple goes
  into quarantine and the `h2` line of the key takes the slot. A healthy
  tunnel is never switched to another transport of its key: `h2` stays after
  the `h3` quarantine expires, until it fails or changes. Between keys,
  `selection.strategy` ranks as usual. A full apply (see WireGuard sources)
  keeps the active transport of a key; only a restart picks again by
  `priority`.
- **Tags.** `<id>-<cc>-<city>-mq-<sha8>`; `sha8` covers the device public
  key, `peerkey`, endpoint, `address`, `sni`, `transport` and `dns`, not the
  label and the metadata (`priority` included).
- **Outbound.** `protocol` and `streamSettings.network` `masque` with the
  device key (std base64 SEC 1), the endpoint key (PEM PKIX, pinned by Xray)
  and the addresses in `masqueSettings`; always `security: tls` with
  `tlsSettings.serverName` = `sni` (Xray checks `security` only when the
  outbound is added), `alpn: ["h2"]` for `h2` only. No `fingerprint`,
  headers or `pinnedPeerCertSha256` (the latter would turn off the key
  pinning).
- **Monitor.** A failure shows only as a reset or a timeout, up to 10 s
  while the tunnel is set up: use `timeoutMs` ≥ 10000 and `attempts` ≥ 2.
- **Secrets.** The device key never appears in any output; the
  runtime-state `parsed` object of a MASQUE outbound has no private key and
  there is no `rawBase64`, even with `includeRaw` and `includeSecrets`.

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
- **Candidate replacement** (`wireguard`, `masque`). Once `replaceAfterFailures` is
  reached, a failed tunnel is replaced instead of repaired; it is repaired
  only when no replacement exists. A slot keeps its port; the
  replacement is the first ranked candidate that is not active, does not
  share a private key with another active one and whose key + endpoint pair
  (key + endpoint + transport for `masque`) is not in quarantine. It removes the old tunnel first (rule, outbound,
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
- **Empty file.** An empty candidate file (no lines at all; wgp writes one
  when a provider has no live candidate) is not a source failure: every
  tunnel of the source is removed as above, the slots stay empty with
  `no_replacement`, and candidates coming back fill them again. A file with
  lines none of which is accepted still fails the source
  (`manifest_validation_failed`) and its tunnels stay.
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
| `/api/status` | `{tunnels, wgp?}` |
| `/api/instance-status` | Local snapshot `{version: 1, instanceId, capturedAt, subscriptions, tunnels, …}`, which remote collectors poll. |
| `/api/instances` | Local snapshot plus the last successful snapshot of each remote instance (`lastError` on failure). |
| `/api/runtime-state?subscriptionId=…&targetAddress=…` | Diagnostic view from the loaded config and live Xray API data. `expected` is the union of the enabled sources; with several sources `expected.sources[]` gives `{sourceId, source, error?}` per source. |

Routing looks only at the path, so `/api/status?x=1` returns JSON. The JSON
schemas are a contract and grow only by adding fields; `version` stays `1`. A
panic in a handler returns 500 and logs `http_handler_panic`. The dashboard is
read-only and has no links to the JSON endpoints.

A target whose subscription has several sources, a `wireguard` or `masque` source or a
source with a warning, gets `targets[].sources[]` =
`{sourceId, type, enabled, maxActive?, activeCount?, candidateCount?, warnings?[]{code, message}}`
in source id order, and its tunnels get `sourceId`; a WireGuard or MASQUE
tunnel also has `protocol: "wireguard"` or `"masque"` (VLESS tunnels have no `protocol`). The dashboard then shows a
Sources card with the warnings (for example `port_range_exhausted`) and a
colored badge with the source id in the title of each tunnel card (the color
follows the id). A subscription with one source and no
warning has neither key, as before.

Each enabled `wireguard` or `masque` source has its input directory checked
for `status.json` (written by wgp) on every request to `/api/status`,
`/api/instance-status` and the dashboard: a private regular file (no access
for group or others) of at most 64 KiB. Each directory with the file adds a
block to `wgp[]` of `/api/status` and `/api/instance-status`:
`{sources[]{subscriptionId, sourceId}, updatedAt?, error?, providers?[]{id, output, type, state, error?, lastTickAt?, lastSuccessAt?, issued, limit?, expiresAt?}}`.
Without the file there is no block and no key. A file that cannot be read
(wrong mode, too large, invalid JSON, a `version` other than `1`) gives a block
with only `error`, a text without the host path; the response is still 200 and
complete. The dashboard shows a wgp section above the subscriptions with a card
per provider (state, issued of limit, expiry, last success, error) and a card
per unreadable file. `needs_login`, `error`, an unknown state,
an expiry within 2 hours of the snapshot time (below the renewal window, so
only a missed renewal; with `renewBeforeMinutes` under 120 a healthy
certificate shows as degraded too) and an unreadable file are shown
as degraded. A central scd gets the blocks of remote instances through
`remoteInstances` and never reads their files. `/api/runtime-state` has no wgp
data.

`/api/runtime-state` hides secrets unless `includeSecrets: true`. Hidden values
include subscription and monitor/push URLs (shown as `<scheme>://<host>/<redacted>`),
`fetchHeaders` values, UUIDs and keys, outbound `path` and the xhttp `host`,
which are `<redacted>`. `includeRaw: true` adds `rawBase64` protobuf blobs
only together with `includeSecrets: true`, because the blobs contain everything
unredacted. WireGuard and MASQUE outbounds never get `rawBase64`, and their
`parsed` never shows the private or preshared key.

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

## scd wgp

wgp keeps provider accounts, keys and certificates and publishes
**candidates**: one file per output with one `wireguard://` or `masque://`
line per candidate (see [WireGuard sources](#wireguard-sources) and
[MASQUE sources](#masque-sources)), plus `status.json` in the same
directory. An scd source of type `wireguard` or `masque` reads the file; scd
and wgp share only that directory (read-only for scd). wgp holds no tunnels
and never talks to Xray.

```yaml
server:
  listen: 0.0.0.0:8080            # required
logging: {level: info, format: json}
state:
  dir: /app/state                 # required, absolute
  keyFile: /run/secrets/wgp_state_key  # required, absolute; a Docker secret
deviceNamePrefix: wgp-a-          # required, [a-z0-9-]+, one per host
keys:
  revokeOldAfterMinutes: 30       # optional, >= 1; required by provider types that revoke
api:                              # optional
  timeoutMs: 15000
  retry: {attempts: 3, minDelayMs: 3000, maxDelayMs: 8000}
identification:                   # identification.<type> for the provider types in use
  <type>: {…}
providers:
  - id: a                         # [a-z0-9]+, unique
    type: <type>                  # a registered provider type
    enabled: true                 # default true
    schedule: "*/15 * * * *"      # required cron
    # …keys of the provider type
outputs:
  - id: a                         # [a-z0-9]+, unique
    path: /app/out/a          # absolute; every output in one directory
    provider: a                   # exactly one output per enabled provider
    candidates: 4                 # required or forbidden, by provider type
```

- The config is strict: unknown keys are errors and the important keys have
  no defaults. `${NAME}` is not expanded; the file holds no secrets.
- The core keys are checked by wgp, the rest of each provider entry and its
  `identification.<type>` by the provider type. An unknown `type` is an error.
- Outputs and providers are 1:1: an output names one provider, a provider
  feeds at most one output, and an enabled provider must have one. An output
  path must not be named `status.json`.
- `print-config` prints `state.keyFile` as a path; wgp never prints the key.
- Provider API calls verify the server certificate and present a fixed TLS
  ClientHello and header set per provider. WARP API requests use a TLS 1.2
  ClientHello (ECDHE AES-256-GCM suites c02c and c030, fixed extension order,
  no GREASE), HTTP/1.1 and JSON with sorted keys; the app strings
  (`identification.warp`) are those of the 1.1.1.1 Android app.
- **State.** `state.dir` holds one encrypted record per provider,
  `<provider id>.state` (AES-256-GCM, bound to the provider id: a record
  renamed to another id does not open). `state.keyFile` holds the key: the
  standard base64 of 32 random bytes, for example
  `head -c 32 /dev/urandom | base64 > secrets/wgp_state_key`, mode `0600` or
  stricter. Logs show the key only as `keySha8` (event `state_opened`).
- `serve`, `login` and `logout` refuse to start unless `state.dir` is a
  directory (not a symlink) owned by the process uid with no group or other
  access (`0700`), every file in it is a regular file owned by that uid with
  no group or other access (`0600`), and the key file has no group or other
  access. They also open every record first: a wrong key or a damaged record
  stops the command with an error naming the file. wgp never replaces an
  unreadable record with a new one; restore the file or the key, or remove
  the record to start that provider over (its sessions and devices are then
  lost).
- `serve`, `login` and `logout` lock `state.dir` (`flock` on `wgp.lock`,
  which holds the pid of the holder), so `login` runs only while `serve` is
  stopped. A second command fails at once with `state dir … is in use by
  another wgp process (pid N in its own pid namespace)`. The kernel drops the
  lock when the holder exits, even when it is killed, so a leftover
  `wgp.lock` never blocks a restart and must not be removed by hand.
- `login`/`logout` need `--provider <id>` of a configured provider.
- **Schedule.** `serve` runs every enabled provider on its `schedule`: the
  first tick at start, then on each slot; a slot that arrives while the
  previous tick of that provider still runs is skipped
  (`provider_tick_skipped_overrun`). On SIGINT/SIGTERM the loops stop at
  once, an active tick is cancelled and waited for, then the status page
  drains (up to 10 s). There is no hot reload.
- **Outputs.** Every tick writes the provider's lines to its output `path`,
  one line per candidate, in the order the provider returns them. The write
  is atomic (temporary file, fsync, rename, mode `0600`), and a file that
  already holds the same bytes is not rewritten, so its mtime changes only
  when a candidate changes. Each line is checked with the parser scd uses
  for the provider's kind (`wireguard://` or `masque://`). A failed tick or a
  bad line fails the output, and the previous file stays (event
  `provider_tick_failed`; `output_published` when a file changed). A tick
  that returns no lines and no error means "no live candidate": the output
  becomes an empty file, and scd removes the source's tunnels.
- **`status.json`.** After every tick wgp rewrites `status.json` in the
  output directory (atomic, `0600`) with the status of every provider that
  has ticked: `id`, `output`, `type`, `state` (`ok`, `needs_login` or
  `error`), `error`, `lastTickAt`, `lastSuccessAt`, `issued`, `limit`,
  `expiresAt`. A provider or output error goes into `state: error` and
  `error`; it never stops the service. The file holds no secrets.
- **Status page.** `server.listen` serves `GET /healthz` (`ok`),
  `GET /api/status` (the `status.json` document from memory) and `GET /` (the
  same as an HTML page, refreshed every 30 s). Other paths are `404`, other
  methods `405`. The page is for the owner on the host: publish the port
  only on `127.0.0.1`. scd never uses it.
- `providers` needs at least one entry.

### Provider `static`

Publishes ready WireGuard configs: every `*.conf` file in `dir`, in file name
order, becomes one `wireguard://` line with `provider=static`. It has no
login, no state and no `identification`; its output takes no `candidates`
(the number of active tunnels is the scd source's `maxActive`).

```yaml
providers:
  - id: static
    type: static
    dir: /app/static              # required, absolute; mount it read-only
    schedule: "*/5 * * * *"       # required; files are read again on each tick
    maxFileBytes: 65536           # default 64 KiB
    defaults:                     # for fields a file leaves out
      mtu: 1420                   # default 1420, 576..1500
      allowedIPs: [0.0.0.0/0, "::/0"]  # default 0.0.0.0/0, ::/0
      dns: []                     # default none: no dns in the line
outputs:
  - {id: wg, path: /app/out/wg, provider: static}
```

- The files use the `wg-quick` format, keys case-insensitive:
  `[Interface]` with `PrivateKey`, `Address`, `DNS` (IP addresses only),
  `MTU`, and exactly one `[Peer]` with `PublicKey`, `PresharedKey`,
  `Endpoint`, `AllowedIPs`, `PersistentKeepalive`. `Address`, `DNS` and
  `AllowedIPs` may be comma lists or repeated lines.
- Comments `# Label = 🇳🇱 Amsterdam` (otherwise the file name without
  `.conf`) and `# Reserved = 1,2,3` set the label and the 3 reserved bytes.
- `ListenPort`, `Table`, `FwMark`, `SaveConfig`, `PreUp`, `PostUp`,
  `PreDown` and `PostDown` are ignored with a warning (`provider_warning`) and
  never run. Any other key, a second `[Interface]` or `[Peer]`, or a missing
  `[Peer]` rejects the file.
- A file must be a regular file of at most `maxFileBytes` with no access for
  others (`0600` or `0640`). The values get the same checks as in scd
  ([WireGuard sources](#wireguard-sources)).
- A rejected file is logged (`provider_error`) and shown in `status.json`
  (`state: error`, `error` names the file and the field, never a value); the
  other files are still published. With no accepted file the tick fails and
  the previous output stays.

### Provider `warp`

Registers Cloudflare WARP devices and publishes, with `provider=warp`, one
`wireguard://` line per device (`tunnel: wireguard`) or one `masque://` line
per device and transport (`tunnel: masque`, see [WARP MASQUE](#warp-masque)). It has no login (`login --provider` fails with
`does not support login`); the devices register themselves. Its output takes
`candidates`: the number of devices. All devices leave through the Cloudflare
colo nearest to the host, so more devices give spares, not other countries.

```yaml
identification:
  warp:                           # required with a warp provider; all strings required
    userAgent: "1.1.1.1/6.38.9-5641 (Android 16.0.0)"
    clientVersion: a-6.38.9-5641  # the CF-Client-Version header
    apiVersion: v0a5641           # the first path segment of the API
    model: PC                     # model, locale, os_version of POST /reg
    locale: en_US
    osVersion: "16.0.0"
providers:
  - id: warp
    type: warp
    tunnel: wireguard             # required: wireguard | masque
    schedule: "0 */6 * * *"       # required; every tick checks every device
    keys:
      rotateOnStart: true         # required: a new key for every device at start
    deviceLostAfterChecks: 2      # default 2, >= 1
    reRegisterMinIntervalMinutes: 1440  # required, >= 1
    label: "WARP"                 # required; put a flag here for scd country filters
    wireguard:                    # optional; only with tunnel: wireguard
      dns: [1.1.1.1, 1.0.0.1, "2606:4700:4700::1111", "2606:4700:4700::1001"]  # default these
      mtu: 1280                   # default 1280, 576..1500
      allowedIPs: [0.0.0.0/0, "::/0"]  # default 0.0.0.0/0, ::/0
      port: 2408                  # default the API port; 2408 | 500 | 1701 | 4500
outputs:
  - {id: warp, path: /app/out/warp, provider: warp, candidates: 1}
```

- **Registration.** A device slot without a device registers once
  (`POST /reg` with a new X25519 key, `tunnel_type: wireguard`). The device
  id, its token and the private key go only into the encrypted state.
- **Check.** Every tick sends `GET /reg/{id}` for every device. `200` with
  `enabled: true` is healthy and resets the loss counter. `404` with code
  `1006`, `401` with code `1012` and `enabled: false` count as one loss.
  Other answers (`5xx`, `429`, timeouts, network errors) neither count nor
  reset the counter. Until the device is lost its line stays published;
  every counted answer is shown in `status.json`.
- **Loss.** After `deviceLostAfterChecks` losses in a row (the counter is in
  the state, so a restart keeps it) the device is dropped and its line is
  withdrawn (`device_lost`; each counted answer logs `device_check_lost`
  with the counter). The slot registers a new device, but not
  earlier than `reRegisterMinIntervalMinutes` after its last `POST /reg`
  attempt, failed ones included (the time is saved in the state before the
  request; a restart does not reset it). Only a slot that never tried goes at
  once. Until then the slot reports `re-registration not before <time>`,
  or, after a failed attempt of this process, the text of that attempt:
  `register: status 429 (next attempt not before <time>)`.
- **Rotation.** With `keys.rotateOnStart` every existing device gets a new
  key once per process (`PATCH /reg/{id}`), after a healthy check; a failed
  rotation is tried again on the next tick. A device registered in this
  process is not rotated. The old key keeps working for a few minutes, so
  scd has time to switch.
- **Line.** `reserved` is the base64-decoded `config.client_id` of the last
  registration or key change (it changes with the key) and must be exactly 3
  bytes; the addresses, the peer key and the endpoint come from the same
  answer (a check answer for the current key refreshes them). With
  `wireguard.port` the endpoint host is kept and the port replaced. DNS, MTU
  and allowed IPs come from the config.
- `status.json`: `issued` is the number of published lines; a failing device
  makes `state: error` with `error` naming the device by its number
  (`device 1: check: device lost: status 404, code 1006`), never its id,
  token or key. With no device left (all lost, or none registered yet) the
  tick returns no lines and no error, so the output becomes an empty file and
  scd drops the tunnels; `state: error` still says why. No line while a
  device exists (its answer cannot make a line) fails the tick and the
  previous output stays.
- More devices than `candidates` in the state (the key was lowered) are
  forgotten, not deleted at Cloudflare.

#### WARP MASQUE

A device works with one tunnel type only, so WARP over MASQUE is its own
`warp` provider with its own devices and its own output, read by an scd
source of type `masque` ([MASQUE sources](#masque-sources)).

```yaml
providers:
  - id: warpmq
    type: warp
    tunnel: masque
    schedule: "0 */6 * * *"
    keys:
      rotateOnStart: true         # a new P-256 key for every device at start
    reRegisterMinIntervalMinutes: 1440
    label: "WARP MASQUE"
    masque:                       # required with tunnel: masque
      transports: [h3, h2]        # required, h3 | h2 without repeats; the order is the priority
      sni: consumer-masque.cloudflareclient.com  # required: a hostname, or the endpoint IP (no SNI sent)
      port: 443                   # default 443: the endpoint port, UDP for h3, TCP for h2
      dns: [1.1.1.1, 1.0.0.1, "2606:4700:4700::1111", "2606:4700:4700::1001"]  # default these; IPs only
outputs:
  - {id: warpmq, path: /app/out/warpmq, provider: warpmq, candidates: 1}
```

- `wireguard` with `tunnel: masque` and `masque` with `tunnel: wireguard`
  are `validate-config` errors.
- Registration, check, loss, re-registration limit and `status.json` are
  the same as above. Right after `POST /reg` (still X25519, `tunnel_type:
  wireguard`) the device gets a new ECDSA P-256 key with `PATCH /reg/{id}`
  (`key_type: secp256r1`, `tunnel_type: masque`); a failed PATCH leaves the
  device without lines and is tried again on the next tick, without a new
  registration. `keys.rotateOnStart` repeats that PATCH with a new key once
  per process. A replaced MASQUE key stops working at once, so a restart with
  rotation can break the tunnel until the next scd sync.
- The P-256 private key is kept only in the encrypted state, like the token.
- **Lines.** From the PATCH answer (or a check answer for the current key):
  the endpoint key (`config.peers[0].public_key`, PEM, must be a PKIX P-256
  key) as `peerkey`, `endpoint.v4` with its port replaced by `masque.port`
  as the endpoint (h3 and h2 use the same address), and the device
  addresses. One line per `masque.transports` entry, `priority` = its index
  (`0` first); the lines of one device differ only in `transport` and
  `priority`, and scd activates one of them. An IP `sni` must be the
  endpoint IP of the answer, or the device has no lines (`status.json`
  names the field).

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
go vet ./... && go vet -tags 'xraylive visual warplive' ./...
go test -race ./...
golangci-lint run                      # v2.15.0, config .golangci.yml
go build -o bin/scd ./cmd/scd
bash scripts/check-xray-version.sh     # .xray-version == go.mod xray-core == compose image
bash scripts/check-no-node.sh          # Go-only repository

# Live tests against a real Xray v26.10.21 binary
SCD_XRAY_BIN=/path/to/xray go test -tags xraylive -count=1 ./internal/scd/livetest/...
# Dashboard visual regression (headless Chromium)
SCD_CHROME=/path/to/chrome go test -tags visual -count=1 ./internal/scd/status/...
# Live WARP check against the real Cloudflare API (not run in CI)
SCD_WARP_LIVE=1 go test -tags warplive -count=1 ./internal/wgp/provider/warp/...
```

Dashboard blocks without a reference of the previous implementation are
compared with local screenshots in `internal/scd/status/testdata/visual/`
(gitignored, never committed); add `-update` to the visual test command to
create them. A test without its screenshot is skipped.

The live tests need no network: besides the Xray API checks, a second test
Xray on `127.0.0.1` (a WireGuard peer, an RFC 9484 MASQUE server with a
self-signed certificate) carries HTTP requests through the tunnels scd builds,
across sync, repair, rejoin and candidate replacement.

The live WARP check registers a new temporary device, switches it to MASQUE,
checks it with `GET /reg/{id}` and deletes it at the end; keys and tokens stay
in memory.

Without the environment variable, the tagged tests skip. Reference outputs of
the previous implementation are in [`testdata/reference/`](testdata/reference/README.md).
