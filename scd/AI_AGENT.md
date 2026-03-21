# scd: AI Agent Context

## Current Shape
- Runtime baseline: Node `24.14.0+`
- Build model: TypeScript compiles to `dist/`
- Package manager: `npm`
- Main runtime modes:
  - `sync`
  - `daemon`
- Low-level debug commands:
  - `generate`

`generate-and-load` and `load` no longer exist. `sync` is the single operational apply path.

## Main Architecture
- `src/config/load-config.ts`
  - loads `config.yml`
  - interpolates `${ENV_VAR}`
  - validates through `zod`
  - resolves relative paths against config location
- `src/runtime/sync-once.ts`
  - orchestration entry for one-shot sync
  - loads subscriptions
  - builds resource plans per subscription
  - applies them to each `subscription.targets[]`
  - keeps memory-only ownership state per `subscription + target + resource kind`
  - resets topology/monitor state only when a valid target manifest changed
  - keeps memory-only overlap guard for one process
- `src/runtime/run-daemon.ts`
  - long-running scheduler loop
  - uses `cron-parser`
  - loads config once on startup
  - calls `syncWithConfig()` on each tick
  - runs optional monitoring and speedtest cron loops per target
  - runs optional balancer monitor cron loops per target
  - can start the built-in read-only status server
  - does not hot-reload YAML; restart required for config changes
- `src/apply/resource-applicator.ts`
  - generic runtime/apply contract for resource kinds
  - currently implemented for `outbound`, `inbound`, and `routing`
- `src/apply/apply-outbounds.ts`
  - `list current`
  - full replace per target
  - deletes every current non-fixed outbound on the target
  - preserves `fixedOutbounds`
  - best-effort rollback on failure
- `src/apply/apply-inbounds.ts`
  - manages generated SOCKS inbounds on target
  - preserves `fixedInbounds`
- `src/apply/apply-routing.ts`
  - manages generated routing rules on target
  - preserves `fixedRouting`
- `src/topology/build-tunnel-topology.ts`
  - builds stable target topology from filtered outbound manifest
  - allocates `in-` tags, `route-` tags and sequential ports
- `src/runtime/monitoring.ts`
  - HTTP health-check through generated SOCKS inbounds
  - per-target repair flow
  - simple telemetry speedtest
  - speedtest supports ordered fallback URLs
- `src/runtime/status-server.ts`
  - HTML + JSON read-only view over in-memory monitoring state
- `src/api/xray-handler-client.ts`
  - simple request-per-RPC HTTP/2 gRPC client
  - no connection reuse by design
  - has per-request timeout

## Config Contract

Main config file:
- `./config.yml`

Compose config file:
- `../docker/config.yml`

Current schema:
- `subscriptions[]`
  - `id`
  - `input`
  - `enabled`
  - `format`
  - `fetchTimeoutMs`
  - `filters?`
    - `countryAllowlist?`
    - `labelIncludeRegex?`
  - `targets[]`
    - `address`
    - `timeoutMs`
    - `fixedOutbounds[]`
    - `fixedInbounds[]`
    - `fixedRouting[]`
    - `observatorySubjectSelectorPrefix?`
    - `inboundSocks?`
    - `monitor`
    - `balancerMonitor`
    - `speedtest`
- `runtime.mode`
- `runtime.schedule`
- `logging.level`
- `logging.format`
- `resources.outbounds.enabled`
- `resources.inbounds.enabled`
- `resources.routing.enabled`
- `statusServer.enabled`
- `statusServer.listen`

Defaults are applied by the schema. Relative paths are normalized during config loading.

## Supported Behavior
- Input sources:
  - local file
  - `http(s)` URL
  - stdin
- Input decoding:
  - plain
  - base64
  - auto-detect
- VLESS profiles:
  - `tcp + tls`
  - `ws + tls`
  - `tcp + reality + xtls-rprx-vision`
- Resource kinds:
  - `outbound`
  - `inbound`
  - `routing`
- Monitoring:
  - HTTP checks through generated SOCKS inbounds
  - tunnel-local repair that recreates `routing + outbound`
  - optional target-level balancer monitor via external SOCKS5
  - simple HTTP download speedtest telemetry
  - speedtest URLs can be configured as ordered fallback list
  - speedtest concurrency is limited per target via `speedtest.maxParallel`
  - monitor probes run with bounded concurrency per target via `monitor.maxParallel`
  - balancer monitor is observe-only and does not trigger repair or apply
  - built-in read-only status page
  - grouped HTML status view by `subscription -> target`
  - target-specific runtime JSON dump via `/api/runtime-state`
  - runtime JSON is redacted by default unless `statusServer.runtimeState.includeRaw/includeSecrets` explicitly enable more detail

Strict rules still apply:
- non-`vless://` => skipped
- unknown param => skipped
- unsupported combo/value => skipped

## CLI Surface

Built CLI:

```bash
node ./dist/cli.js validate-config --config ./config.yml
node ./dist/cli.js print-config --config ./config.yml
node ./dist/cli.js sync --config ./config.yml
node ./dist/cli.js daemon --config ./config.yml
node ./dist/cli.js generate --input ../vpn --output ./xray.manifest.json --log ./parse.log.json
```

## Logging
- Logger: `pino`
- Formats:
  - `json`
  - `pretty`
- Logs go to `stderr`
- Machine-readable reports go to `stdout`

Important event names:
- `config_loaded`
- `sync_started`
- `subscription_fetched`
- `manifest_built`
- `apply_finished`
- `daemon_tick`
- `sync_failed`

## Runtime State
- Managed outbound ownership state is memory-only
- It is keyed by `subscriptionId + target.address + resource kind`
- Topology, monitor and speedtest state are also memory-only
- After restart, memory state is empty again
- `SyncReport.unchanged` counts skipped resource plans, not individual outbound entries
- Overlap protection is memory-only and does not coordinate separate processes
- If `observatorySubjectSelectorPrefix` is set on a target, it is prepended to every applied outbound tag on that target
- During monitor-triggered repair, recreated outbound is applied without `observatorySubjectSelectorPrefix`
- Empty input or `parsed === 0` is fail-closed for that source; its targets are marked failed and receive no Xray API calls
- Subscription filters run once after parsing and before target-specific prefixing; they do not contribute to `skipped`

## Docker
- `Dockerfile` is in project root
- Compose service is in `../docker-compose.yml`
- Xray service stays separate
- Control-plane service mounts:
  - `../docker/config.yml`
  - root `../vpn`

The compose stack was verified with:
- `xray`: `ghcr.io/xtls/xray-core:26.2.6`
- `scd`: local Docker build

## Validation / Verification Status
- `npm run typecheck` passes
- `npm run build` passes
- `npm test` passes
- `sync --config ./config.yml` works against live Docker Xray
- `docker compose up -d --build` starts both `xray` and `scd`
- `scd` daemon performs an initial sync successfully

## Important Design Constraints
- Keep the API client simple; do not add session pooling unless there is a proven need.
- Keep service logic readable over clever abstractions.
- `inbound` and `routing` are topology-driven and must stay aligned with the same tunnel mapping as outbound.
- Repair and full sync must use the same per-target mutex.
- `observatory` remains static config; only outbound tagging is prepared here via target-specific prefixing.
- `xtls-sdk` stays reference-only:
  - [`remnawave/xtls-sdk`](https://github.com/remnawave/xtls-sdk)
