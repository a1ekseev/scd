# tools/golden

> Temporary tooling. This directory is deleted in task 7.3 of the Go migration;
> `testdata/reference/` stays.

Node scripts that import the built TypeScript services (`scd/dist`, `sfc/dist`,
`gtl/dist`) and write reference outputs into `testdata/reference/`. Go tests read
those references after the TS code is removed.

Every script imports `lib/freeze-time.mjs` first (frozen `Date`, default
`2026-08-15T10:00:00.000Z`, override with `GOLDEN_NOW`) and writes through
`lib/io.mjs` (`writeJSON`, `writeText`, `repoRoot`).

## Regenerate references

1. Build the TS services (`npm ci && npm run build` in `scd`, `sfc`, `gtl`), then run
   all scripts in a Node 24 container from the repository root:

   ```bash
   docker run --rm --network host -e TZ=UTC -e HTTPS_PROXY -e NO_PROXY \
     -v /root/.ccr/ca-bundle.crt:/ca.crt:ro -e NODE_EXTRA_CA_CERTS=/ca.crt \
     -v "$PWD":/repo -w /repo node:24-alpine node tools/golden/run-all.mjs
   ```

   Scripts that do not exist yet are reported as `skipped`.

2. Screenshots (`screenshots.mjs`) need a Chromium headless shell and run separately
   on the host, not in the container:

   ```bash
   node tools/golden/screenshots.mjs
   ```
