# gtl: Agent Context

## Purpose
- `gtl` stands for `Gen Test Load`.
- It is a standalone HTTP service for deterministic download/load testing.
- It always returns `200` for valid load requests so clients download the full generated body.
- It does not depend on `scd`, `sfc`, or Xray.

## Runtime Model
- Public endpoints:
  - `GET /healthz`
  - `GET <load.path>/<sizeKb>`
- `load.path` is required and has no fallback alias.
- `sizeKb` is KiB: `1 KB = 1024 bytes`.
- Valid load responses use:
  - `content-type: application/octet-stream`
  - `content-length: sizeKb * 1024`
  - diagnostic `x-gtl-*` headers.
- Payload is deterministic ASCII `a` bytes streamed in 64 KiB chunks.

## Config Notes
- Required config:
  - `server.listen`
  - `load.path`
- Optional config:
  - `load.maxSizeKb`, default `10240`
  - `logging.level`, default `info`
  - `logging.format`, default `json`
- `load.path` must start with `/`, must not equal `/`, must not end with `/`, and must not contain query or hash.
- Valid load routes always return `200`.
- Invalid route params under the configured load path return `400`; unrelated routes return `404`.

## CLI Surface
- `node ./dist/cli.js serve --config ./config.yml`
- `node ./dist/cli.js validate-config --config ./config.yml`
- `node ./dist/cli.js print-config --config ./config.yml`

## Docker Image
- `ghcr.io/a1ekseev/gtl`

## Main Files
- `src/config/load-config.ts`: YAML loading and zod validation.
- `src/runtime/server.ts`: HTTP routing and streaming payload generation.
- `src/cli.ts`: `serve`, `validate-config`, and `print-config`.

## Verification
- Run `npm run typecheck`, `npm test`, and `npm run build` from `gtl/`.
- Validate local config with `node ./dist/cli.js validate-config --config ./config.yml`.
- Docker smoke uses root `docker compose up -d --build gtl`.
