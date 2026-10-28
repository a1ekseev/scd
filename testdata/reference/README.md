# Reference outputs from the TypeScript services

Captured from the TS implementation before it was removed, so the Go rewrite can be
checked against them. These files are frozen Go test data (D20): the TS services and the
capture scripts (`tools/golden/`) are gone from the tree, so the references cannot be
regenerated. The TS references were captured by commits `ae1fde2`..`975287a`
(tooling and TS sources as of `975287a`: `git show 975287a:tools/golden/README.md`);
later commits only add Go-side fixtures (`scd/status/fixtures/selection-*.json`).
Script names below (`tools/golden/*.mjs`) refer to that commit.

Exception (2026-10-27): the REALITY key, short ids and one SNI in the fixtures came from a
real provider subscription and were replaced in the whole history with synthetic values.
Tunnel tags and hashes derive from the raw link, so `scd/corpus/ts-parse.json`,
`scd/generate/vpn-sample.manifest.json` and `scd/status/runtime-state/vision-udp443.json`
were regenerated with the same scripts and TS sources (`scd-domain.mjs`, `status.mjs`,
`node:24-alpine`); only tags and hashes of the affected lines changed.

Fields depending on ICU (country `nameEn`, `localeCompare` order) are informational:
Go is not required to reproduce them. Captured on the Node version recorded in
`ts-baseline.txt`.

## Index

| Path | What | Task |
|---|---|---|
| `ts-baseline.txt` | TS test counts per service and `node --version` | 0.1 |
| `scd/fixtures/vpn-sample.txt` | copy of `scd/test/fixtures/vpn-sample.txt` | 0.1 |
| `scd/corpus/vless-corpus.txt`, `scd/corpus/README.md` | 84 VLESS lines (one per line) with a one-phrase purpose each | 0.2 |
| `scd/corpus/ts-parse.json` | `[{line, raw, ts}]`: TS `parseSubscriptionLine` + `normalizeVless` per corpus line (`scanLines` over the whole corpus); `ts` is `{ok, label, profile, tag, normalized}`, `{ok:false, reasonCode, reason, details?, label?}` or `{error: "URI malformed"}` | 0.2 |
| `scd/generate/{vpn-sample,docker-fixture}.{manifest,log,summary}.json` | `generateCommand` output (frozen `generatedAt`, `sourceFile` = `<input>`) | 0.2 |
| `scd/generate/real-shapes-provider2.{manifest,log}.json` | provider2 base64 fixture: 50 parsed, all `tcp-tls` | 0.2 |
| `scd/generate/real-shapes-remnawave.error.txt` | TS error for the remnawave fixture (0 parsed, `unsupportedParam=4`); Go intentionally differs (D26/D28) | 0.2 |
| `scd/print-config/{docker-example,minimal}.json` (+ `minimal.yml`) | `loadConfig` output; `input` made repo-relative | 0.2 |
| `scd/config-full.yml`, `scd/config-keys.json` | config with every TS schema key; sorted leaf key paths of its `print-config` (`a.b`, arrays as `a[]`, only leaves) | 0.2 |
| `common/cron-next.json` | `[{expr, from, next[3]}]` from `cron-parser` 5.4.0, `TZ=UTC`, from `2026-08-15T10:00:00.000Z` | 0.2 |
| `common/cron-rejected.json` | expressions Go rejects (D28); `tsParse`/`tsNext` record what TS does with them (informational: TS accepts most) | 0.2 |
| `sfc/refresh-docker-example.json` | `refreshWithConfig` report for a temp copy of `docker/sfc/config.example.yml` (input = absolute path to `docker/fixtures/subscription.b64`, frozen `refreshedAt`): 3 outputs, matchedLines test-all 8, test-de 3, test-nl 3 | 0.3 |
| `sfc/outputs/<output-id>.{b64,plain.txt}` | `lastGoodBase64` / `lastGoodPlain` of each docker-example output | 0.3 |
| `sfc/server-cases.json` | `[{name, path, userAgent?, status, contentType, body, headers}]` from `handleServerRequest(new URL(path,'http://localhost').pathname, state, {'user-agent': ua})`; profile headers only for UA prefix match (`ClashMeta/1.0` on test-de) | 0.3 |
| `sfc/print-config-docker-example.json` | `loadConfig` output of the docker example; `input` repo-relative | 0.3 |
| `sfc/real-shapes.json` | `{provider2, remnawave}`: refresh reports for `docker/sfc/config.real-shape.yml` over `real-shapes/provider2.b64` and `remnawave.txt`. provider2: `en-server` 50, `extra-*` 0 (`Filtered output is empty.`); remnawave: 0/0/0 (TS silently drops `mode`/`extra` lines). Output order as TS reports it (by id) | 0.3 |
| `sfc/config-full.yml`, `sfc/config-keys.json` | config with every TS sfc schema key; sorted leaf key paths (arrays as `a[]`) | 0.3 |
| `gtl/routes.json` | `[{path, method, status, headers, body?|bodyBytes?}]` from `buildResponse(new URL(path,'http://127.0.0.1').pathname, config)` with `maxSizeKb: 64`; `body` for text responses, `bodyBytes` (payload length, not stored) for 200 loads; last row `HEAD /load/1` = 405 captured from a real server (`date`/`connection`/`keep-alive` dropped). Since 2026-10-28 the Go gtl no longer sends the `x-gtl-*` headers and its body is random; the test skips those headers and checks only the length. Its 400 body is a plain `bad request` (the limit is not revealed); the two 400 rows (body and content-length) were updated | 0.3 |
| `gtl/print-config-docker-example.json` | `loadConfig` output of `docker/gtl/config.example.yml` | 0.3 |
| `gtl/config-full.yml`, `gtl/config-keys.json` | as for sfc | 0.3 |
| `scd/status/fixtures/<name>.json` | status state fixtures (contract below; Go reads them in 6.4): `empty`, `healthy-single`, `degraded-repaired`, `balancer-only`, `disabled-subscription`, `latency-sort`, `multi-sub-multi-target`, `remote-instances`, `remote-ping-states`, `hostile-strings` | 0.4 |
| `scd/status/<name>/{index.html,api-status.json,api-instance-status.json,api-instances.json}` | `handleStatusServerRequest` for `/`, `/api/status`, `/api/instance-status`, `/api/instances` per fixture (all 200; JSON re-indented with trailing newline, HTML body + `\n`) | 0.4 |
| `scd/status/<name>/screenshot-{1280,390}.png` | headless Chromium screenshots of `index.html`, exactly `<w>×2400` (reference only) | 0.4 |
| `scd/status/runtime-state/cases.json`, `scd/status/runtime-state/<case>.json` | `buildCurrentRuntimeStateSnapshot` with a fake Xray client (03 §10.5 items 1–6); inputs in `cases.json` | 0.4 |
| `scd/corpus/d26-links.txt`, `scd/corpus/d26-expect.json`, `scd/corpus/d26-keys.md` | D26 matrix corpus: 107 VLESS lines (one per line, no comments) and the expected Go outcome per line; keys from the Xray 26.10.10 binary. **Not** taken from TS (section «Корпус D26») | 0.5 |
| `common/label-filter.json` | `[{label, source, en, extra, extraWhitelist}]`: decisions of the three production TS label regexes (D29) per label; `source` = `remnawave` (4) / `provider2` (50) / `edge` (synthetic) | 0.6 |

### scd domain (task 0.2)

Fields that Go intentionally does not reproduce:

- new profile tags: tags of profiles added in Go (xhttp, ML-KEM, REALITY+pqv, ...) have no TS counterpart; links that TS skips (`unsupported_param`, `unsupported_value`, `unsupported_combo`) are accepted by Go where Xray v26.10.10 accepts them (D26/D28), so `ts-parse.json` entries with `ok:false` for in-scope links are expected to differ;
- `jsonOutbound` as a whole: Go uses the v26 form (D11, 09 section 5), compare only the TS-stable parts;
- config error texts: Go messages differ (D9); compare only that an error occurs;
- `normalized.country.nameEn`: informational (ICU, Node `v24.22.0`), likewise `localeCompare` ordering;
- `real-shapes-remnawave.error.txt`: TS rejects all 4 lines, Go accepts them;
- `cron-rejected.json`: `tsParse`/`tsNext` are informational; the Go library decides.

### sfc and gtl (task 0.3)

Captured by `tools/golden/sfc.mjs` and `tools/golden/gtl.mjs` (Node `v24.22.0`, `TZ=UTC`, frozen time, pino `silent`).

- sfc configs are copied into a temp directory (source cache is written next to the config) with `subscriptions[0].input` replaced by an absolute fixture path; reports contain no paths.
- `real-shapes.json`: the 50/0/0 and 0/0/0 split is a deliberate TS behaviour; Go (D29/D33) differs: remnawave lines with `mode`/`extra` are accepted and provider2 `en-server` keeps 4 lines in the lookahead-free Go form (3.2 `TestRefreshRealShapes`).
- Informational: `Date`-dependent `refreshedAt` (frozen value), `HEAD` row headers (Node-specific), `bodyBytes` (derived from `content-length`).

### scd status server (task 0.4)

Captured by `tools/golden/status.mjs` (Node `v24.22.0` in `node:24-alpine`, `TZ=UTC`, frozen time
`2026-08-15T10:00:00.000Z`) and `tools/golden/screenshots.mjs` (host, Chromium headless shell
`/opt/pw-browsers/chromium_headless_shell-1194/chrome-linux/headless_shell`, Chromium 141.0.7390.37; host fonts
Inter for `sans-serif`, Noto Color Emoji; flags `--headless --no-sandbox --disable-gpu --hide-scrollbars
--force-device-scale-factor=1 --window-size=<w>,2400 --screenshot=<png> file://<index.html>`).

#### Fixture contract (`scd/status/fixtures/<name>.json`)

```json
{
  "now": "2026-08-15T10:00:00.000Z",
  "config": {
    "instanceId": "local-main",
    "subscriptions": [ {"id": "sub-1", "enabled": true, "address": "127.0.0.1:8080", "observatorySubjectSelectorPrefix": "x-observe-"} ],
    "remoteInstances": [ {"id": "edge-eu", "apiUrl": "https://edge.example.test/api/instance-status", "schedule": "* * * * *"} ],
    "runtimeState": {"enabled": true, "includeRaw": false, "includeSecrets": false}
  },
  "targets": [ {
    "subscriptionId": "sub-1", "targetAddress": "127.0.0.1:8080",
    "balancerMonitor": { "state": "healthy", "remotePingState": "ok", "consecutiveFailures": 0 },
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
| `now` | request time; must equal the frozen time (`capturedAt` of local and aggregated snapshots, local `lastSuccessAt`) |
| `config.instanceId` | `statusServer.instanceId` (required: `os.hostname()` never reaches references) |
| `config.subscriptions[]` | `id`, `enabled`, `address` (= `target.address`), optional `observatorySubjectSelectorPrefix` (= `target.observatorySubjectSelectorPrefix`); everything else takes config defaults (`input: ./sub.txt`, never read). Only enabled subscriptions appear on the dashboard |
| `config.remoteInstances[]` | `statusServer.remoteInstances[]`: `id`, `apiUrl`, `schedule` (`timeoutMs` default) |
| `config.runtimeState` | `statusServer.runtimeState` (`enabled`, `includeRaw`, `includeSecrets`); not used by the four captured endpoints |
| `targets[]` | memory state of one target (`SyncMemoryState.targets["<subscriptionId>::<targetAddress>"]`); must match a configured subscription id + address; absent target = no state (before first sync) |
| `targets[].balancerMonitor` | `TargetBalancerMonitorState` (03 §1), assigned as is: required `state` (`idle\|healthy\|degraded`), `remotePingState` (`idle\|pending\|ok\|failed`), `consecutiveFailures`; optional `lastCheckedAt`, `lastSuccessAt`, `lastFailureAt`, `lastStatusCode`, `lastLatencyMs`, `lastSuccessStatusCode`, `lastSuccessLatencyMs`, `lastError`, `remotePingLastRunAt`, `remotePingLastSuccessAt`, `remotePingLastFailureAt`, `remotePingLastStatusCode`, `remotePingLastError`, `remotePingLastReportedStatus` (`up\|down`), `remotePingLastReportedMsg`, `remotePingLastReportedPingMs` |
| `targets[].tunnels[]` | `TunnelMapping` in topology order (`replaceTargetTopology`, then monitor set directly); `[]` = target state without tunnels |
| `….baseTunnelId` | tunnel id (key in `tunnels`); `inboundTag` = `in-<id>`, `routeTag` = `route-<id>` (not visible in the outputs) |
| `….displayName`, `….countryIso2` | card title, card meta (`countryIso2` optional → `-`) |
| `….listen`, `….port` | `endpoint` = `<listen>:<port>` |
| `….outboundTagInitial`, `….outboundTagCurrent`, `….outboundWithoutPrefixTag` | prefixed tag, current tag (after repair = unprefixed), unprefixed tag (`outboundWithoutPrefix.tag`, also used as `baseOutboundTag`); together they give `balanced` (03 §4.4) |
| `….monitor` | `TunnelMonitorState` (03 §1), assigned as is: required `state` (`healthy\|degraded\|repairing\|idle`), `consecutiveFailures`; optional `lastCheckedAt`, `lastSuccessAt`, `lastFailureAt`, `lastStatusCode`, `lastLatencyMs`, `lastSuccessStatusCode`, `lastSuccessLatencyMs`, `lastError` |
| `remotes[]` | aggregation-state entry of a configured remote (`id` and `apiUrl` must match config). Optional `lastAttemptAt`, `snapshot` (an `InstanceStatusSnapshot`; validated by the TS `parseRemoteSnapshot` and passed through untouched, unknown fields included), `lastSuccessAt`, `lastError`. A configured remote missing here, or listed with only `id`/`apiUrl`, has no data (`Waiting for first successful collection`). JSON key order is that of `collectRemoteStatus`: `id, source, apiUrl, lastAttemptAt, snapshot, lastSuccessAt, lastError` |

Fixtures: `empty` (03 §10.4 item 7), `healthy-single` (item 1, healthy balancer), `degraded-repaired` (items 2, 6),
`balancer-only` (item 3), `disabled-subscription` (item 4), `latency-sort` (10.5 item 8: Fast/Slow/Idle/Unknown;
`Unknown` has no `countryIso2`), `multi-sub-multi-target` (10.5 item 7; config order `sub-b, sub-a, sub-c`,
`sub-c` without state), `remote-instances` (10.4 item 8: config order `edge-us, edge-eu`; `edge-eu` has error +
snapshot with unknown fields `futureField`/`futureTunnelField`, `edge-us` has no data), `remote-ping-states`
(Remote Push `idle`, `pending`, `failed down`, `delivered up`, `delivered down`; `ping-failed` = item 1 balancer),
`hostile-strings` (displayName/lastError/remote snapshot/remote `lastError` with `<script>`, quotes, `&`).

Notes:

- Sorting: every sorted group (subscriptions, targets, tunnels, remote ids) is chosen so that ICU `localeCompare`
  equals byte order (`strings.Compare`, D23); `status.mjs` asserts it. Emoji/Cyrillic names occur only in groups
  of one.
- One target per subscription: TS config allows exactly one `target` per subscription, so 10.5 item 7
  (`sub-a` with two targets) is represented by several subscriptions; memory-only targets are not used.
- HTML is a TS byte reference, not a Go golden file (D3: Go may use `html/template`, `&#39;` etc.). `escapeHtml`
  does not escape `'` (seen in `hostile-strings`).
- PNGs are reference only (font rendering depends on the host): the Go visual test (6.5) renders the TS
  `index.html` and the Go HTML in the same browser and compares those.
- Informational: `capturedAt`/timestamps (frozen), order of ICU-sorted lists outside the asserted groups.

#### Balancer selection extension (task 6.6, D22)

`fixtures/selection-*.json` extend the contract with the Go-only balancer selection (10 §6.6); they have **no TS
reference** (the feature does not exist in TS) — their Go golden dashboards are
`testdata/golden/scd/status/selection-*.html` (`go test ./internal/scd/status -run TestDashboardSelectionGolden -update`).
The Go tests that compare with TS (`TestJSONMatchesTSReference`, `TestDashboardTextMatchesTS`, the visual test) skip them.

| Key | Meaning |
|---|---|
| `config.subscriptions[].balancerSelection` | `target.balancerSelection` (`enabled`, `balancerTag`, `schedule`, `observatory`, `strategy?`) |
| `targets[].balancerSelection` | `state.SelectionState`: the keys of the JSON view where they overlap (`balancerTag`, `state` `ok\|error`, `mode`, `selectedOutboundTag` = selected tag, `overrideTarget`, `principleTargets`, `selectedSince`, `lastCheckedAt`, `lastSuccessAt`, `lastFailureAt`, `lastError`, `observatoryError`) plus `previousSelectedTag`, `consecutiveFailures`, `observatoryCheckedAt` and `observatory` (`[{tag, alive, delayMs?, lastError?, lastSeenAt?, lastTryAt?, healthPing?{all, fail, averageMs, deviationMs, minMs, maxMs}}]`, `null` = unavailable) |

Fixtures: `selection-single`, `selection-override`, `selection-pool` (strategy `random`, one dead candidate),
`selection-none`, `selection-error` (stale selection), `selection-observatory-unavailable`, `selection-repaired`
(repaired `nl-1` with a stale prefixed observatory entry, rejoined `at-1` awaiting observation, stale `x-observe-gone`).

#### runtime-state (`scd/status/runtime-state/`)

`cases.json` holds the inputs (subscription line or remote input, target overrides on top of
`fixedOutbounds: [direct, blocked]`, `inboundSocks 127.0.0.1:20000-20010`, prefix `x-observe-`; builder options;
memory balancer state; fake `listOutbounds/listInbounds/listRules` items: `{fixedTag, rawHex}` or
`{tunnel, variant: initial|withoutPrefix}` built with the TS `buildOutboundGrpc`/`buildInboundGrpc`, rules as
`outboundTag`(1)/`inboundTag`(8)/`ruleTag`(19)). Config goes through `loadConfig`, so `config.subscription` and
`config.target` carry all TS defaults. The temp input path is replaced with `<input>` (`config.subscription.input`,
`expected.source`).

Cases: `redacted-default` (10.5 item 1), `vision-udp443` (2), `repaired-fallback` (3), `remote-input` (4),
`redact-monitor-urls` and `redact-monitor-urls-include-secrets` (5), `raw-and-secrets` (6).

This is a reference of the **shape** (keys, classification, diff, redaction). `runtime.*[].parsed` values for
ws/fingerprint fields and `rawBase64` are informational (the TS decoder read wrong fields, D2/D6; Go builds
v26 protobuf). The TS default-config keys in `config.*` follow the TS schema; Go schema changes (D31) apply.

### Корпус D26 (task 0.5)

`scd/corpus/d26-links.txt` + `scd/corpus/d26-expect.json`: one VLESS share link per line and, at the same index, its
expected result in Go. The expectations come from the Xray v26.10.10 VLESS outbound matrix (profile rows,
parameter mapping and Xray error texts, with the D28/O16 and D33 amendments) and the plan tasks 1.7/4.3–4.7. They are
**not** TS outputs (TS skips most of these links). Each line was checked against `infra/conf` and a live API-only
Xray 26.10.10 (`AddOutbound` → `ListOutbounds` → `proto.Equal` + a per-parameter field check); see the task 0.5 report.
Keys: `d26-keys.md`.

Contract of `d26-expect.json` (array, element `i` describes line `i+1`; read by Go tests 1.7, 4.3–4.7, 5.6):

| Field | Meaning |
|---|---|
| `line` | 1-based line number in `d26-links.txt` |
| `matrixRow` | `3#<n>`: row of 11 §3; `4:<param>`: row of 11 §4 (`4:id` = userinfo id; `4:cs`, `4:heartbeatPeriod` = D33 additions) |
| `expect` | `entry` or `skip` |
| `profile` | `entry` only: profile from 11 §3 («Профили и теги») |
| `warnings` | `entry` only: warning codes in order. `flow_cleared` (D16) or `param_ignored` (only for an **unknown** parameter, D28). Parameters that do not apply to the chosen transport/security are dropped silently and give no warning (O16) |
| `params` | `entry` only: link parameters (lowercase keys as in `vless.Link`) that must reach Xray, for `paramcheck` (4.6/5.6). Never contains a cleared `flow`, `allowinsecure`, inapplicable or generator-only keys (`support-x25519mlkem768`, `x_padding_bytes`, O13/O16). Sorted in `paramcheck.Covered` order; the union over all entries is exactly the 22 `Covered` parameters |
| `reasonCode` | `skip` only: `vless.Skip` code |
| `stage` | `skip` only: `parse` (`vless.Parse`), `scd` (`normalize`), `xray` (`conf.Build` refusal, caught in the manifest as `unsupported_value` «Xray rejected the outbound configuration.») |
| `expectContains` | `skip` only: substring of `reason` (`parse`/`scd`) or of `details[0]` (`xray`, the wrapped `conf.Build` error) |
| `note` | free text |

Rules: the corpus is policy-invariant (same result with `Policy{}` and `Policy{AllowInsecure:true}`), so
`allowInsecure=1|true` only appears together with `pcs`/`vcn`; hosts are `*.example.test` or TEST-NET addresses,
except `RS1`/`RS2` (lines 83–84, byte-for-byte line 1 of `docker/fixtures/real-shapes/remnawave.txt` and
`provider2.txt`, hosts `*.example.com`) and the Remnawave stub `0.0.0.0:1` (line 82). Counts: 73 `entry`, 34 `skip`
(2 `parse`, 16 `scd`, 16 `xray`).

### Корпус меток (task 0.6)

`common/label-filter.json` records what the owner's TS regexes decide on each label (D29, 12 §6.3). Evaluated by
`tools/golden/label-filter.mjs` (part of `run-all.mjs`) via `parseRegexLiteral` from `sfc/dist/config/load-config.js`
(`new RegExp(pattern, flags)`), `regex.test(label)` with `lastIndex = 0`:

- `en` = `/^(?!.*Extra)(?!.*WhiteList).*$/i`, `extra` = `/^(?=.*Extra)(?!.*WhiteList).*$/i`, `extraWhitelist` = `/(?=.*Extra)(?=.*WhiteList)/i`.
- `source`: `remnawave` (4 labels) and `provider2` (50) are extracted by the script from `docker/fixtures/real-shapes/{remnawave,provider2}.txt`
  (fragment after the first `#`, `decodeURIComponent`); `edge` = synthetic labels (case variants, both words in either order,
  emoji flags, Cyrillic, empty string, the boundary cases of 12 §6.3, labels with a real `\n`). 91 labels in total, 54 of them real.
- Only the regex decisions are recorded; the meaning of a label (e.g. the `-X` suffix) is not interpreted (D33).
- The three filters are mutually exclusive by construction, so only four true/false combinations exist (en only, extra only,
  extraWhitelist only, none); all four are present.
- Known divergences from Go (RE2 `labelIncludeRegex`/`labelExcludeRegex`, D29): exactly the labels with a real `\n`, because JS `.`
  does not match `\n` (TS `.*` stops at the line break, `.*$` cannot reach the end), while Go looks at the whole label:
  `Extra\nWhiteList` and `WhiteList\nExtra` (TS `extraWhitelist == false`, Go `true`), `Extra\nfoo` (TS `extra == false`, Go `true`),
  `foo\nbar` (TS `en == false`, Go `true`). They are the `allowedDiffs` of `TestLabelFilterRealShapeEquivalence`
  (task 1.3, `internal/common/labelfilter`) with the reason "JS `.` does not match `\n`"; Go behaviour is kept (12 §6.3).
