# Go golden files and test corpora

Test data of the Go tests. A golden file is the current Go output: after an
intended change, regenerate it with `-update` and review the diff before
committing. Corpora and vectors are frozen inputs with recorded expectations
(regression data): they are edited by hand, never regenerated.

| Path | What | Kind |
|---|---|---|
| `scd/status/` | status fixtures, API, dashboard and runtime-state (below) | golden, `go test ./internal/scd/status/ -run Golden -update` |
| `scd/print-config/{minimal.sources,docker-example}.json` | `scd print-config` of `minimal.sources.yml` and `docker/scd/config.example.yml`, repository root removed from paths | golden, `go test ./internal/scd/cli/ -run PrintConfig -update` |
| `sfc/print-config-docker-example.json` | `scd sfc print-config` of the example | golden, `go test ./internal/sfc/cli/ -run PrintConfig -update` |
| `sfc/refresh-docker-example.json`, `sfc/real-shapes-{provider2,remnawave}.json` | sfc refresh reports of the example and of `docker/sfc/config.real-shape.go.yml` over `docker/fixtures/real-shapes/` | golden, `go test ./internal/sfc/refresh/ -run 'DockerExample\|RealShapes' -update` |
| `gtl/print-config-docker-example.json` | `scd gtl print-config` of the example | golden, `go test ./internal/gtl/cli/ -run PrintConfig -update` |
| `wgp/print-config-fake.json`, `wgp/print-config-docker-example.json` | `scd wgp print-config` of a fake config and of the example | golden: `go test ./internal/wgp/config/ -run PrintGolden -update`; the example one is checked by `internal/wgp/cli` (edited by hand) |
| `sfc/outputs/<id>.{b64,plain.txt}` | the payloads of the sfc example outputs | vector |
| `sfc/server-cases.json` | sfc HTTP cases `[{name, path, userAgent?, status, contentType, body, headers}]` | vector |
| `gtl/routes.json` | gtl HTTP cases `[{path, method, status, headers, body?\|bodyBytes?}]` | vector |
| `common/cron-next.json`, `common/cron-rejected.json` | cron expressions with their next three runs (UTC), and the rejected ones | vector |
| `common/label-filter.json` | labels with the decisions of three production label filters | vector |
| `scd/corpus/` | VLESS line corpus with its parse/normalize expectations (`corpus/README.md`), D26 matrix corpus (`d26-links.txt`, `d26-expect.json`, `d26-keys.md`) | vector |
| `scd/fixtures/vpn-sample.txt` | a sample subscription | vector |
| `scd/generate/*.manifest.json`, `scd/generate/docker-fixture.summary.json` | `scd generate` manifests and summary: expected tags and summaries | vector |

## scd status (`scd/status/`)

| Path | What |
|---|---|
| `fixtures/<name>.json` | status state fixtures (contract below), input |
| `<name>/{api-status,api-instance-status,api-instances}.json`, `<name>/dashboard.html` | `/api/status`, `/api/instance-status`, `/api/instances` and `/` of each fixture, byte for byte |
| `runtime-state/cases.json` | runtime-state cases (input): a subscription line or remote input, target overrides, builder options, balancer state, the fake Xray items |
| `runtime-state/<case>.json` | `/api/runtime-state` of each case; the temporary input path is `<input>`, a `rawBase64` value is `<present>` |
| `old-remote-snapshot.json` | an `/api/instance-status` of an older instance (no sources, no balancer selection), frozen: the dashboard and the aggregator must accept it. Never regenerated |

```bash
go test ./internal/scd/status/ -run Golden -update
```

The visual test (`-tags visual`, `SCD_CHROME`) compares screenshots of every
fixture with local PNGs in `internal/scd/status/testdata/visual/` (gitignored,
created with `-update`).

### Fixture contract (`scd/status/fixtures/<name>.json`)

```json
{
  "now": "2026-08-15T10:00:00.000Z",
  "config": {
    "instanceId": "local-main",
    "subscriptions": [ {"id": "sub-1", "enabled": true, "address": "127.0.0.1:8080", "observatorySubjectSelectorPrefix": "x-observe-",
                        "balancers": [ {"tag": "x-balancer", "monitor": {"enabled": true, "schedule": "*/1 * * * *"}} ]} ],
    "remoteInstances": [ {"id": "edge-eu", "apiUrl": "https://edge.example.test/api/instance-status", "schedule": "* * * * *"} ],
    "runtimeState": {"enabled": true, "includeRaw": false, "includeSecrets": false}
  },
  "targets": [ {
    "subscriptionId": "sub-1", "targetAddress": "127.0.0.1:8080",
    "balancers": [ {"tag": "x-balancer", "monitor": { "state": "healthy", "remotePingState": "ok", "consecutiveFailures": 0 }} ],
    "tunnels": [ {
      "baseTunnelId": "vless-at-x-tcp-tls-1", "displayName": "🇦🇹 Вена", "countryIso2": "AT",
      "listen": "127.0.0.1", "port": 20000,
      "outboundTagInitial": "x-observe-vless-at-x-tcp-tls-1", "outboundTagCurrent": "x-observe-vless-at-x-tcp-tls-1",
      "outboundWithoutPrefixTag": "vless-at-x-tcp-tls-1",
      "monitor": { "state": "healthy", "lastLatencyMs": 42, "lastStatusCode": 204, "consecutiveFailures": 0 }
    } ]
  } ],
  "remotes": [ { "id": "edge-eu", "apiUrl": "https://edge.example.test/api/instance-status",
                 "lastAttemptAt": "…", "lastSuccessAt": "…", "lastError": "Remote status API returned HTTP 503.", "snapshot": { } } ]
}
```

| Field | Meaning |
|---|---|
| `now` | request time (`capturedAt`, local `lastSuccessAt`) |
| `config.instanceId` | `statusServer.instanceId` (required: the hostname never reaches a golden file) |
| `config.subscriptions[]` | `id`, `enabled`, `address` (= `target.address`), optional `observatorySubjectSelectorPrefix`; everything else takes config defaults (the input is never read). Only enabled subscriptions appear on the dashboard |
| `config.subscriptions[].balancers` | `target.balancers` in the Go config form (`[{tag, monitor?, selection?}]`); the status views show the first |
| `config.subscriptions[].sources` | `[{id, type, enabled, maxActive?, wgpDir?}]`; absent = one source `vless`; `wgpDir` makes the source input `<tmp>/<wgpDir>/<id>` |
| `config.remoteInstances[]` | `statusServer.remoteInstances[]`: `id`, `apiUrl`, `schedule` |
| `config.runtimeState` | `statusServer.runtimeState` |
| `wgp` | `{"<dir>": status.json}` written to `<tmp>/<dir>/status.json` with mode 0600; a JSON string is written verbatim (a broken file) |
| `targets[]` | memory state of one target; must match a configured subscription id + address; absent target = no state (before the first sync) |
| `targets[].balancers[]` | `{tag, monitor?, selection?}`: the state of balancer `tag`; `monitor` is `state.BalancerMonitor`, assigned as is |
| `targets[].balancers[].selection` | `state.SelectionState`: the keys of the JSON view where they overlap, plus `previousSelectedTag`, `consecutiveFailures`, `observatoryCheckedAt` and `observatory` (`[{tag, alive, delayMs?, lastError?, lastSeenAt?, lastTryAt?, healthPing?}]`, `null` = unavailable) |
| `targets[].sources` | `[{sourceId, warnings, candidateCount?, noReplacement?}]` |
| `targets[].tunnels[]` | tunnels in topology order (`sourceId` absent = `vless`); `inboundTag` = `in-<baseTunnelId>`, `routeTag` = `route-<baseTunnelId>`; the three tags give `balanced`; `monitor` is `state.TunnelMonitor` |
| `remotes[]` | the aggregation state of a configured remote (`id` and `apiUrl` must match): optional `lastAttemptAt`, `lastSuccessAt`, `lastError`, `snapshot` (parsed and passed through, unknown fields included). A remote with only `id`/`apiUrl` has no data |

Fixtures: `empty`, `healthy-single`, `degraded-repaired`, `balancer-only`,
`disabled-subscription`, `latency-sort`, `multi-sub-multi-target`
(config order `sub-b, sub-a, sub-c`, `sub-c` without state), `remote-instances`
(`edge-eu` with an error and a snapshot with unknown fields, `edge-us` without
data), `remote-ping-states`, `hostile-strings` (`<script>`, quotes, `&`);
`selection-*` (balancer selection: single, override, pool, none, error,
observatory unavailable, repaired); `sources-two`, `sources-wireguard`;
`wgp-ok`, `wgp-needs-login`.
