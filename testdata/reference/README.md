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
