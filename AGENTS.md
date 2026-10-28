# scd: Agent Context

## Repository Shape
- One Go module, `github.com/a1ekseev/scd`, builds one binary, `scd`, which runs three services:
  - `scd`: Subscription Control Daemon. Manages Xray through the gRPC API and serves the status dashboard. Its commands are top level: `scd sync|daemon|generate|validate-config|print-config`.
  - `scd sfc`: Subscription Filter Control. Filters subscriptions and serves public base64 payloads: `scd sfc serve|refresh|validate-config|print-config`.
  - `scd gtl`: Gen Test Load. An HTTP random load payload generator: `scd gtl serve|validate-config|print-config`.
- Layout:
  - `cmd/scd/`: entry point. `main.version` is set by ldflags.
  - `internal/cli/`: root cobra command and `--version` (root only). Mounts the three services.
  - `internal/common/`: shared code only. `buildinfo cliutil configload cron cronloop httpserve inputsrc jsonout labelfilter listenaddr logging regexlit schema securefile sigctx subscription vless`.
  - `internal/scd/`, `internal/sfc/`, `internal/gtl/`: the services (`config/`, `cli/`, …).
  - `internal/archtest/`: import-boundary test.
  - `testdata/reference/`: frozen reference outputs of the previous implementation, used by Go tests (D20).
  - `docker/`: example and real-shape configs, fixtures, test Xray config.
  - `scripts/`: CI checks and compose smoke.
- User docs: `README.md`.

## Common Rules
- Go `1.27` (`go 1.27.0`, `toolchain go1.27.3`). Use the latest dependency versions, except xray-core.
- `internal/scd`, `internal/sfc` and `internal/gtl` never import each other, and `internal/common` imports no service. `internal/archtest` enforces this. Shared code goes only in `internal/common`.
- xray-core is pinned by commit: `v1.260327.1-0.20261021115930-1b1647cd9ddf` = Xray v26.10.21. Never `@latest`. Update the go.mod pin, the compose image `ghcr.io/xtls/xray-core:<ver>` and `.xray-version` **together**; `scripts/check-xray-version.sh` checks this.
- Behaviour follows Go conventions (D9): Go error texts, `log/slog` to stderr, `regexp` (RE2), `net/url`, stdlib base64, `strings.Compare` ordering. Kept as a contract: log `event` names and fields, the status JSON API (additive only, `version: 1`), the dashboard look (visual regression), and Xray API behaviour (RPC set and order, fixed resources, rollback, error texts).
- Lint: `golangci-lint` v2.15.0 with `.golangci.yml` (gofumpt and goimports formatters, revive). Every package and exported identifier needs a doc comment. Do not add `//nolint` without a reason.
- Tests use stdlib `testing`, table-driven where it fits, with real in-process servers (`httptest`, bufconn fake Xray `internal/scd/xrayapi/xrayfake`) instead of mocks.
- `**/config.yml` is ignored by git. Local `docker/<svc>/config.yml` files can hold the owner's real configs: never create, modify or assume them. Tracked configs are `docker/<svc>/config.example.yml`, `config.real-shape.yml` (old form) and `config.real-shape.go.yml` (Go form).
- Never commit real subscription URLs, hosts, UUIDs, keys, paths or sfc output ids. Use the sanitized fixtures in `docker/fixtures/` (`real-shapes/` included).
- The tree is Go-only (`scripts/check-no-node.sh`); the only `.js` file is the browser script `internal/scd/status/web/dashboard.js` of the dashboard.

## Checks
```bash
go vet ./... && go vet -tags 'xraylive visual' ./...
go test -race ./...
golangci-lint fmt && golangci-lint run
go build -o bin/scd ./cmd/scd            # always -o bin/scd
./bin/scd validate-config --config docker/scd/config.example.yml
./bin/scd sfc validate-config --config docker/sfc/config.example.yml
./bin/scd gtl validate-config --config docker/gtl/config.example.yml
bash scripts/check-xray-version.sh
bash scripts/check-no-node.sh
SCD_XRAY_BIN=/path/to/xray-26.10.21 go test -tags xraylive -count=1 ./internal/scd/livetest/...
SCD_CHROME=/path/to/headless_shell go test -tags visual -count=1 ./internal/scd/status/...
bash scripts/compose-smoke.sh            # Docker smoke on the example configs
```
The live and visual tests skip without their environment variables.

## CI Contract
- `.github/workflows/docker.yml`. On pull requests, pushes to `master` and release tags, job `verify` runs: `go mod verify`, the Xray pin check, the Go-only tree check, `go vet` (also with `-tags 'xraylive visual'`), golangci-lint, govulncheck (pinned version), `go test -race`, live Xray tests, dashboard visual regression, build, and example config validation. Job `smoke` (after `verify`) runs the compose smoke.
- Release tags `vX.Y.Z` publish one multi-arch image (amd64 and arm64) with tags `latest` and `X.Y.Z`, only after `verify` and `smoke` pass.

## Docker Image
- `ghcr.io/a1ekseev/scd` only. The `sfc` and `gtl` images are no longer published.
- Distroless static `:nonroot` image, user `65532:65532`; the sfc cache dir `/app/config/.sfc-cache` is created owned by it. `.dockerignore` is an allow-list (`go.mod`, `go.sum`, `cmd/`, `internal/`). Default `CMD ["scd","daemon","--config","/app/config/config.yml"]`. Compose selects sfc and gtl with `command: ["scd","sfc","serve",…]` and `["scd","gtl","serve",…]`.

## scd Notes
- A subscription is `{id, enabled, sources[], target}`; the old subscription keys and `target.inboundSocks.portRange` are unknown keys. A source `id` prefixes its tags (`id: vless` gives the old tags) and owns four namespaces on its target: `<obsPrefix><id>-`, `<id>-` (outbounds), `in-<id>-`, `route-<id>-` (`apply.Namespace`). `target.address` is unique, so a subscription is one Xray: within it source ids are unique and their `<id>-` prefixes and port ranges do not overlap. `normalize.Policy.TagPrefix` is required (empty panics).
- Pipeline:
  0. `syncer`, per enabled subscription: orphans first (`apply.RemoveOrphans`, under the target mutex): non-fixed resources outside every enabled source's namespace, rule → inbound → outbound, enabled kinds only; rules with an empty `ruleTag` are skipped (`RemoveRule("")` is never sent). An error stops that target for this sync (`orphan_remove_failed`).
  1. `syncer`: for each enabled source, in id order, load input (`common/inputsrc`: file, http(s) or stdin; `maxInputBytes`; `fetchUserAgent`; redacted `fetchHeaders`).
  2. `common/subscription` + `common/vless`: split lines and parse.
  3. `normalize`: scope (D26), allowInsecure policy, flow, ML-KEM key checks, tags.
  4. `manifest`: build, then filter (country, then label); `outbound` builds the v26 JSON, and `conf.Build` validates it.
  5. `topology`: tunnels of one source, `in-`/`route-` tags, sequential ports from the source `portRange`; lines beyond it are skipped (`port_range_exhausted`).
  6. `resources`: SOCKS inbounds and routing rules.
  7. `apply`, per source: outbound → inbound → routing; remove own, add new; fixed and other sources' resources never touched; outbound-only rollback. One source failing does not touch the others.
  8. `state`: memory-only hashes and topology per source (`Target.Sources`), tunnels of all sources (source, then tag) with monitor state.
- Logs and sync reports: `subscriptionId` is the subscription, `sourceId` the source id.
- `xrayapi` is the only package that talks gRPC to Xray. Mutations: Handler List/Add/Remove Outbound/Inbound and Routing ListRule/AddRule/RemoveRule. Read-only: `GetBalancerInfo` and `GetOutboundStatus`.
- Sync, repair and rejoin share the per-target mutex in `state`. Monitors and balancer selection read without it.
- `daemon` runs `common/cronloop` loops: sync; per subscription the tunnel monitor, balancer monitor and balancer selection; per remote instance a status poll. It also runs the status HTTP server. On SIGINT/SIGTERM, loops stop at once, in-flight Xray mutations finish (`context.WithoutCancel`) but no new source is loaded and no new target or source is applied, then pending remote pings, then the HTTP server. There is no hot reload.
- `monitor`: tunnel probes through the generated SOCKS inbounds (`socks`). Repair recreates the outbound without the observatory prefix; rejoin restores it. `balancerMonitor` is observe-only, with an optional remote push. Both run only after a sync reached the target.
- `balancer`: read-only balancer selection view (D22). Never calls `OverrideBalancerTarget`.
- `status`: dashboard (`html/template` + `web/` assets, embedded), `/api/status`, `/api/instance-status`, `/api/instances`, `/api/runtime-state` (secrets redacted and `rawBase64` omitted unless `includeSecrets`). `aggregate` holds the remote snapshots.
- `country`: flag emoji → ISO2. Labels are not interpreted beyond country and city (D33).

## sfc Notes
- No Xray. Per subscription: fetch (with fetchUserAgent and fetchHeaders) → parse with the shared `common/vless` (same parameter list as scd, no applicability checks) → per-output label filter (`common/labelfilter`) → keep raw lines in original order → base64.
- Outputs are served only from memory at `<pathRoute>/<output id>`; output ids are globally unique. `GET /healthz` is also served. Profile headers are sent only on a `userAgent` prefix match.
- Failure rules:
  - A failed download falls back to the disk source cache `<config dir>/.sfc-cache/<id>.subscription`.
  - With no cache, the previous payloads are kept.
  - An empty filtered output fails that output and keeps its previous payload.
  - No payload yet → `502`.
- `serve`: one initial refresh before listening, then `refreshSchedule`. Overruns are skipped.

## gtl Notes
- `GET /healthz`, `GET <load.path>/<sizeKb>` → always `200` for a valid size, `application/octet-stream`, `content-length = sizeKb*1024`, no custom headers. Bad size or a size above `maxSizeKb` → `400`; other paths → `404`; non-GET → `405`.
- Payload (`internal/gtl/payload`): AES-128-CTR keystream, streamed in 64 KiB chunks from one reused buffer. The key is random per process (memory only, never logged), the IV random per request, so no two bodies repeat and middleboxes see no fingerprintable content. Nothing decrypts it.
- `load.path` is required: starts with `/`, is not `/`, has no trailing `/`, query or hash. Unknown config keys are errors.
