# Reference outputs from the TypeScript services

Captured from the TS implementation before it is removed, so the Go rewrite can be
checked against them. Regenerate with `tools/golden/` (see its README; removed in
task 7.3).

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
| `gtl/payload-vectors.json` | one array: `{sizeBytes, sha256, first16hex}` for 0, 1, 16, 1024, 65536, 65537, 98311, 10485760 (`first16hex` = first 16 bytes as hex; via `writeRepeatedPayload`), then `{offset, fill8}` for offsets 0, 65536, 2^32, 2^32+65536 (8 bytes of `fillDeterministicPayload`; 2^32 equals 0 because XOR wraps to int32) | 0.3 |
| `gtl/routes.json` | `[{path, method, status, headers, body?|bodyBytes?}]` from `buildResponse(new URL(path,'http://127.0.0.1').pathname, config)` with `maxSizeKb: 64`; `body` for text responses, `bodyBytes` (payload length, not stored) for 200 loads; last row `HEAD /load/1` = 405 captured from a real server (`date`/`connection`/`keep-alive` dropped) | 0.3 |
| `gtl/print-config-docker-example.json` | `loadConfig` output of `docker/gtl/config.example.yml` | 0.3 |
| `gtl/config-full.yml`, `gtl/config-keys.json` | as for sfc | 0.3 |

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
