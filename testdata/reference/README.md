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

### scd domain (task 0.2)

Fields that Go intentionally does not reproduce:

- new profile tags: tags of profiles added in Go (xhttp, ML-KEM, REALITY+pqv, ...) have no TS counterpart; links that TS skips (`unsupported_param`, `unsupported_value`, `unsupported_combo`) are accepted by Go where Xray v26.10.10 accepts them (D26/D28), so `ts-parse.json` entries with `ok:false` for in-scope links are expected to differ;
- `jsonOutbound` as a whole: Go uses the v26 form (D11, 09 section 5), compare only the TS-stable parts;
- config error texts: Go messages differ (D9); compare only that an error occurs;
- `normalized.country.nameEn`: informational (ICU, Node `v24.22.0`), likewise `localeCompare` ordering;
- `real-shapes-remnawave.error.txt`: TS rejects all 4 lines, Go accepts them;
- `cron-rejected.json`: `tsParse`/`tsNext` are informational; the Go library decides.
