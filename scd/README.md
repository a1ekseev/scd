# scd

`scd` stands for `Subscription Control Daemon`.
`scd` converts subscription-style VLESS input into Xray outbounds and applies them to Xray over the gRPC API.

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
        # observatorySubjectSelectorPrefix: x-observe-

runtime:
  mode: run-once

logging:
  level: info
  format: pretty

resources:
  outbounds:
    enabled: true
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
- `subscriptions[].targets[].observatorySubjectSelectorPrefix`: optional prefix added to every applied outbound tag for this target. Use it when static `observatory.subjectSelector` or future `routing.balancers[].selector` should match dynamic outbound by prefix.
- `runtime.mode`: service mode. Allowed values: `run-once`, `daemon`. Default: `run-once`.
- `runtime.schedule`: cron expression for daemon mode. Required only when `runtime.mode: daemon`. Example: `"*/5 * * * *"`.
- `logging.level`: log verbosity. Allowed values: `fatal`, `error`, `warn`, `info`, `debug`, `trace`, `silent`. Default: `info`.
- `logging.format`: log output format. Allowed values: `json`, `pretty`. Default: `json`.
- `resources.outbounds.enabled`: enables outbound apply. If `false`, the service still fetches subscriptions and builds the manifest, but does not push outbounds to Xray.

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

Control-plane owns all non-fixed outbound on a target.

Current apply model:
- if a subscription manifest did not change during the current process lifetime, that target is skipped without any Xray API calls
- if an input source is empty after `trim()` or produces `parsed === 0`, that source fails closed and no Xray API calls are made for its targets
- if subscription filters are configured, they run once per source in this order: `countryAllowlist`, then `labelIncludeRegex`
- if the manifest changed, `scd` lists current outbounds on that target, deletes every non-fixed outbound, and then applies the full new outbound set
- `fixedOutbounds` are never deleted
- if `observatorySubjectSelectorPrefix` is set on a target, it is prepended to every applied outbound tag on that target
- after cleanup, the full new outbound set is applied to that target
- failure is target-local: one failed target does not stop other targets or subscriptions
- if `add` fails after cleanup started, `scd` performs best-effort rollback: remove newly added outbound and re-add saved previous configs

The no-change optimization is memory-only. After a process restart, daemon memory is empty and the first sync will talk to Xray again.
`unchanged` in `SyncReport` is a resource-level metric: it counts skipped resource plans, not individual outbound items.
Overlap protection is also memory-only and works only inside one running process. Separate `sync` processes are not coordinated with each other.
Filter counters are separate from `skipped`: `skipped` remains parse/validation-only, while `filtered`, `filteredByCountry`, and `filteredByLabelRegex` describe intentional post-parse filtering.

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
- Outbound profiles:
  - `tcp + tls`
  - `ws + tls`
  - `tcp + reality + xtls-rprx-vision`

## What is not supported yet
- non-`vless://` protocols
- unsupported VLESS query params
- inbound/routing/balancer apply logic
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
