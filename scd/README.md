# scd

`scd` stands for `Subscription Control Daemon`.
`scd` converts subscription-style VLESS input into Xray resources and applies them to Xray over the gRPC API.

The project now supports both one-shot sync and long-running daemon mode driven by a YAML config.

Daemon mode does not hot-reload YAML. After changing `config.yml`, restart the process or container.

## Requirements
- Node `24.14.0` or newer
- `npm`

## Install and build

```bash
cd scd
npm install
npm run build
```

## Main commands

Validate YAML config:

```bash
cd scd
node ./dist/cli.js validate-config --config ./config.yml
```

Print normalized config:

```bash
cd scd
node ./dist/cli.js print-config --config ./config.yml
```

Run a single sync cycle:

```bash
cd scd
node ./dist/cli.js sync --config ./config.yml
```

Run as a daemon:

```bash
cd scd
node ./dist/cli.js daemon --config ./config.yml
```

Low-level manifest generation:

```bash
cd scd
node ./dist/cli.js generate --input ../vpn --output ./xray.manifest.json --log ./parse.log.json
```

`generate-and-load` and `load` were removed on purpose. `sync` is the only operational apply entrypoint.

## YAML config

Local sample:
- `./config.yml`

Docker/compose sample:
- `../docker/config.yml`

Example:

```yaml
subscriptions:
  - id: local-vpn
    input: ../vpn
    enabled: true
    format: auto
    fetchTimeoutMs: 5000
    filters:
      countryAllowlist:
        - DE
        - NL
      labelIncludeRegex: '/,\s*Extra(?!\s*Whitelist)\s*$/i'
    targets:
      - address: 127.0.0.1:8080
        timeoutMs: 5000
        fixedOutbounds:
          - direct
          - blocked
        fixedInbounds: []
        fixedRouting: []
        # visionUdp443Override: true
        # observatorySubjectSelectorPrefix: x-observe-
        # inboundSocks:
        #   listen: 127.0.0.1
        #   portRange:
        #     start: 20000
        #     end: 20099
        # monitor:
        #   enabled: true
        #   schedule: "*/2 * * * *"
        #   maxParallel: 10
        #   request:
        #     url: https://example.com/
        #     method: GET
        #     expectedStatus: 200
        #     timeoutMs: 5000
        # balancerMonitor:
        #   enabled: true
        #   schedule: "*/2 * * * *"
        #   socks5:
        #     host: 127.0.0.1
        #     port: 1080
        #   request:
        #     url: https://example.com/health
        #     method: GET
        #     expectedStatus: 200
        #     timeoutMs: 5000
        #   successGet:
        #     url: https://example.com/ping.txt
        #     expectedStatus: 200
        #     timeoutMs: 5000
        # speedtest:
        #   enabled: true
        #   schedule: "*/10 * * * *"
        #   urls:
        #     - https://example.com/test-10mb.bin
        #     - https://example.com/test-50mb.bin
        #   method: GET
        #   timeoutMs: 15000
        #   maxParallel: 3

runtime:
  mode: run-once

logging:
  level: info
  format: pretty

resources:
  outbounds:
    enabled: true
  inbounds:
    enabled: false
  routing:
    enabled: false

statusServer:
  enabled: false
  # listen: 127.0.0.1:9090
  runtimeState:
    enabled: true
    includeRaw: false
    includeSecrets: false
```

Parameters:
- `subscriptions`: list of subscription sources. At least one enabled entry is required.
- `subscriptions[].id`: stable source identifier used in logs, reports and `sourceId`.
- `subscriptions[].input`: local file path, `http(s)` URL or `-` for stdin.
- `subscriptions[].enabled`: enables or disables this source. Default: `true`.
- `subscriptions[].format`: input decoding mode. Allowed values: `auto`, `plain`, `base64`. Default: `auto`.
- `subscriptions[].fetchTimeoutMs`: timeout for remote `http(s)` subscription download in milliseconds. Default: `5000`. This does not affect Xray gRPC API calls.
- `subscriptions[].filters`: optional subscription-level manifest filters applied after parsing and before target-specific tagging/apply.
- `subscriptions[].filters.countryAllowlist`: optional ISO2 allowlist, for example `DE`, `NL`, `US`. Entries without a recognized country are excluded when this filter is present.
- `subscriptions[].filters.labelIncludeRegex`: optional JavaScript regex literal applied to `entry.label`, for example `'/,\s*Extra(?!\s*Whitelist)\s*$/i'`.
- `subscriptions[].targets`: list of Xray targets for this subscription. One subscription can be applied to multiple Xray instances.
- `subscriptions[].targets[].address`: Xray gRPC API address in `host:port` form. Example: `127.0.0.1:8080` or `xray:8080` in Docker Compose.
- `subscriptions[].targets[].timeoutMs`: per-request timeout to this target in milliseconds. Default: `5000`.
- `subscriptions[].targets[].fixedOutbounds`: exact outbound tags that must never be deleted by `scd` on this target. The sample configs explicitly include `direct` and `blocked`.
- `subscriptions[].targets[].fixedInbounds`: exact inbound tags that must never be deleted by `scd`.
- `subscriptions[].targets[].fixedRouting`: exact routing `ruleTag` values that must never be deleted by `scd`.
- `subscriptions[].targets[].visionUdp443Override`: if `true`, `scd` rewrites generated VLESS REALITY `flow=xtls-rprx-vision` to `xtls-rprx-vision-udp443` only for this target. Default: `false`.
- `subscriptions[].targets[].observatorySubjectSelectorPrefix`: optional prefix added to every applied outbound tag for this target. Use it when static `observatory.subjectSelector` or future `routing.balancers[].selector` should match dynamic outbound by prefix.
- `subscriptions[].targets[].inboundSocks`: required when `resources.inbounds.enabled` or `resources.routing.enabled` is `true`.
- `subscriptions[].targets[].inboundSocks.listen`: IP or host to bind generated SOCKS inbounds on this target.
- `subscriptions[].targets[].inboundSocks.portRange.start` / `end`: inclusive port range used for stable sequential tunnel allocation.
- `subscriptions[].targets[].monitor`: optional HTTP health-check config for generated tunnels.
- `subscriptions[].targets[].monitor.enabled`: enables monitoring for this target. Default: `false`.
- `subscriptions[].targets[].monitor.schedule`: cron expression for monitor ticks. Required only when monitoring is enabled.
- `subscriptions[].targets[].monitor.maxParallel`: maximum number of monitor probes that may run at the same time for one target. Default: `10`.
- `subscriptions[].targets[].monitor.request`: request definition used through each generated SOCKS tunnel.
- `subscriptions[].targets[].monitor.request.url`: monitored URL.
- `subscriptions[].targets[].monitor.request.method`: `GET`, `HEAD` or `POST`. Default: `GET`.
- `subscriptions[].targets[].monitor.request.expectedStatus`: expected HTTP status code.
- `subscriptions[].targets[].monitor.request.timeoutMs`: request timeout in milliseconds. Default: `5000`.
- `subscriptions[].targets[].balancerMonitor`: optional target-level HTTP check through a configured external SOCKS5 proxy.
- `subscriptions[].targets[].balancerMonitor.enabled`: enables balancer monitoring for this target. Default: `false`.
- `subscriptions[].targets[].balancerMonitor.schedule`: cron expression for balancer monitor ticks.
- `subscriptions[].targets[].balancerMonitor.socks5.host` / `port`: external SOCKS5 endpoint used for balancer checks.
- `subscriptions[].targets[].balancerMonitor.request`: primary request definition executed through that SOCKS5 proxy.
- `subscriptions[].targets[].balancerMonitor.successGet`: optional best-effort `GET` request executed after a successful primary check. It is recorded in UI/JSON but does not affect health-state.
- `subscriptions[].targets[].speedtest`: optional throughput telemetry config.
- `subscriptions[].targets[].speedtest.enabled`: enables simple HTTP download speed tests. Default: `false`.
- `subscriptions[].targets[].speedtest.schedule`: cron expression for speedtest ticks.
- `subscriptions[].targets[].speedtest.urls`: list of URLs for speed tests. `scd` tries them in order and stores the first successful result.
- `subscriptions[].targets[].speedtest.method`: only `GET` is supported in the current implementation.
- `subscriptions[].targets[].speedtest.expectedSizeBytes`: optional expected payload size for reporting/debugging.
- `subscriptions[].targets[].speedtest.timeoutMs`: request timeout in milliseconds. Default: `15000`.
- `subscriptions[].targets[].speedtest.maxParallel`: maximum number of speedtests that may run at the same time for one target. Default: `3`. One tick still processes the full tunnel snapshot; this only limits concurrency inside that tick.
- `runtime.mode`: service mode. Allowed values: `run-once`, `daemon`. Default: `run-once`.
- `runtime.schedule`: cron expression for daemon mode. Required only when `runtime.mode: daemon`. Example: `"*/5 * * * *"`.
- `logging.level`: log verbosity. Allowed values: `fatal`, `error`, `warn`, `info`, `debug`, `trace`, `silent`. Default: `info`.
- `logging.format`: log output format. Allowed values: `json`, `pretty`. Default: `json`.
- `resources.outbounds.enabled`: enables generated outbound apply. Default: `true`.
- `resources.inbounds.enabled`: enables generated SOCKS inbound apply. Default: `false`.
- `resources.routing.enabled`: enables generated routing rule apply. Default: `false`.
- `statusServer.enabled`: enables the built-in read-only status page. Default: `false`.
- `statusServer.listen`: listen address in `host:port` form. Required only when the status server is enabled.
- `statusServer.runtimeState.enabled`: enables the detailed `/api/runtime-state` JSON endpoint. Default: `true`.
- `statusServer.runtimeState.includeRaw`: includes `rawBase64` runtime blobs in the detailed JSON endpoint. Default: `false`.
- `statusServer.runtimeState.includeSecrets`: includes sensitive fields such as subscription URLs and outbound UUID/key material in the detailed JSON endpoint. Default: `false`.

In daemon mode `monitor`, `speedtest`, `balancerMonitor` and the main sync loop do not overlap with themselves. If one tick is still running when the next cron slot arrives, `scd` skips that slot and logs an explicit `*_tick_skipped_overrun` warning.

Relative paths are resolved against the YAML file location.
`${ENV_VAR}` interpolation is supported.
All `subscriptions[].targets[].address` values must be globally unique in the YAML config.

## Regex examples

Examples for `subscriptions[].filters.labelIncludeRegex`:

```yaml
# 1. Keep only "Extra", but exclude "Extra Whitelist"
labelIncludeRegex: '/,\s*Extra(?!\s*Whitelist)\s*$/i'

# 2. Keep labels containing either "Extra" or "Premium", but exclude "Whitelist"
labelIncludeRegex: '/\b(?:Extra|Premium)\b(?!.*\bWhitelist\b)/i'

# 3. Keep labels containing either "Germany" or "Netherlands", but exclude "Trial"
labelIncludeRegex: '/\b(?:Germany|Netherlands)\b(?!.*\bTrial\b)/i'

# 4. Keep labels containing either "DE" or "NL", but exclude "Backup"
labelIncludeRegex: '/\b(?:DE|NL)\b(?!.*\bBackup\b)/i'
```

Recommended usage notes:
- use non-capturing groups like `(?:Extra|Premium)` when you want one of several keywords
- use negative lookaheads like `(?!.*\bWhitelist\b)` to exclude one keyword while keeping broader matches
- test regexes carefully, because filtering happens before apply and can intentionally reduce the manifest to zero entries

## Sync behavior

`scd` owns all non-fixed generated resources on a target.

Current apply model:
- if a subscription manifest did not change during the current process lifetime, that target is skipped without any Xray API calls
- if an input source is empty after `trim()` or produces `parsed === 0`, that source fails closed and no Xray API calls are made for its targets
- if subscription filters are configured, they run once per source in this order: `countryAllowlist`, then `labelIncludeRegex`
- if the manifest changed, `scd` rebuilds target topology and re-applies every enabled generated resource kind for that target
- generated topology is `outbound -> inbound -> routing`, with one SOCKS inbound and one routing rule per filtered outbound entry
- `fixedOutbounds`, `fixedInbounds` and `fixedRouting` are never deleted
- if `observatorySubjectSelectorPrefix` is set on a target, it is prepended only to the initial applied outbound tags
- failure is target-local: one failed target does not stop other targets or subscriptions
- if `add` fails after cleanup started, `scd` performs best-effort rollback: remove newly added outbound and re-add saved previous configs

The no-change optimization is memory-only. After a process restart, daemon memory is empty and the first sync will talk to Xray again.
`unchanged` in `SyncReport` is a resource-level metric: it counts skipped resource plans, not individual outbound items.
Overlap protection is also memory-only and works only inside one running process. Separate `sync` processes are not coordinated with each other.
Filter counters are separate from `skipped`: `skipped` remains parse/validation-only, while `filtered`, `filteredByCountry`, and `filteredByLabelRegex` describe intentional post-parse filtering.

## Monitoring and repair

- Monitoring is optional and runs only in daemon mode for targets where `monitor.enabled: true`.
- Monitor probes run in parallel for all tunnels on one target.
- Every generated tunnel is checked through its generated SOCKS inbound.
- The current implementation performs HTTP checks and expects a configured response status code.
- `balancerMonitor` is separate from tunnel monitoring and runs through a configured external SOCKS5 proxy.
- `balancerMonitor.successGet`, when configured, runs only after a successful primary balancer check and is best-effort only.
- Speedtest is telemetry-only in the current version: it performs an HTTP `GET` through the tunnel and stores the last measured throughput in memory.
- Multiple speedtest URLs are supported as ordered fallback. This is useful when one test file or host is temporarily unavailable.
- Speedtests run with bounded concurrency per target, controlled by `speedtest.maxParallel`.
- If a monitor check fails, `scd` keeps the SOCKS inbound stable, removes that tunnel's generated routing rule and outbound, and recreates them.
- Recreated outbound during repair is applied **without** `observatorySubjectSelectorPrefix`.
- Sync and repair use the same per-target in-memory mutex, so they do not mutate one target concurrently.

## Status page

- The built-in status page is optional and available only in daemon mode.
- HTML endpoint: `/` or `/status`
- JSON endpoint: `/api/status`
- Detailed target runtime JSON endpoint: `/api/runtime-state?subscriptionId=<id>&targetAddress=<addr>`
- Current UI fields:
  - `displayName`
  - `countryIso2`
  - generated SOCKS endpoint
  - current monitor state
  - current balancer monitor state
  - last HTTP status
  - last latency
  - last speedtest throughput
- HTML output is grouped by `subscription -> target`.
- Each target block contains a `Current runtime state (JSON)` link for the detailed runtime dump.
- `/api/status` remains a lightweight in-memory snapshot for the current status page.
- `/api/runtime-state` is different: it builds a read-only target snapshot from the current loaded config plus live Xray API data.

## Docker

Official GHCR image:
- `ghcr.io/a1ekseev/scd:latest`
- `ghcr.io/a1ekseev/scd:<version>`

Pull the published image:

```bash
docker pull ghcr.io/a1ekseev/scd:latest
```

GitHub Actions publication policy:
- `pull_request` and `push` to `master` run `npm ci`, `typecheck`, `test`, and Docker build verification only
- git tags in the form `vX.Y.Z` publish `ghcr.io/a1ekseev/scd:latest` and `ghcr.io/a1ekseev/scd:X.Y.Z`
- published images are multi-arch: `linux/amd64` and `linux/arm64`

The repository includes:
- Xray config dir: `../docker/xray/config/`
- scd config: `../docker/config.yml`
- Compose file: `../docker-compose.yml`

Build and start both services:

```bash
docker compose up -d --build
docker compose ps
```

View `scd` logs:

```bash
docker compose logs -f scd
```

Stop services:

```bash
docker compose down
```

The compose setup was verified with:
- `xray`: `ghcr.io/xtls/xray-core:26.2.6`
- `scd`: local Dockerfile build from `scd/`

## What is supported
- Input:
  - local file
  - `http(s)` URL
  - stdin
  - base64 subscription payloads
- Runtime:
  - `run-once`
  - `daemon`
- Resource apply:
  - outbound
  - inbound (`socks`, `noauth`, `udp: true`)
  - routing (generated inbound tag -> generated outbound tag)
- Monitoring:
  - HTTP health-check through generated SOCKS inbound
  - simple HTTP download speedtest telemetry
  - built-in read-only status page
- Outbound profiles:
  - `tcp + tls`
  - `ws + tls`
  - `tcp + reality + xtls-rprx-vision`

## What is not supported yet
- non-`vless://` protocols
- unsupported VLESS query params
- balancer apply logic
- observatory runtime apply logic
- distributed locking
- inter-process lock coordination

## Development

Typecheck:

```bash
cd scd
npm run typecheck
```

Build:

```bash
cd scd
npm run build
```

Tests:

```bash
cd scd
npm test
```

## Notes
- Logging uses `pino`.
- Daemon/service mode keeps ownership state and overlap protection in memory only.
- Apply/runtime orchestration is already split through a generic resource applicator contract; outbound is the first implementation.
- `observatorySubjectSelectorPrefix` is target-scoped because observatory and future balancer selectors are target-specific.
- `logging.format: pretty` is supported in production runtime, not only during development.
- [`remnawave/xtls-sdk`](https://github.com/remnawave/xtls-sdk) is reference material only, not a runtime dependency.
