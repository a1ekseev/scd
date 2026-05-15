# Gen Test Load

`gtl` is a small HTTP service for deterministic download/load tests.

It always returns `200` for valid load requests, so clients download the full generated body instead of short-circuiting on special HTTP statuses.

## HTTP API

```text
GET /healthz
GET <load.path>/:sizeKb
```

Example:

```bash
curl -i http://127.0.0.1:9092/load/32
```

If `load.path` is `/load`, `/load/32` returns a `200` response with exactly `32 * 1024` bytes. Payload is streamed in 64 KiB chunks and uses deterministic ASCII `a` bytes.

Diagnostic headers:

- `x-gtl-status-code`
- `x-gtl-requested-size-kb`
- `x-gtl-requested-size-bytes`
- `x-gtl-actual-size-bytes`

## Config

```yaml
server:
  listen: 0.0.0.0:8080

load:
  path: /load
  maxSizeKb: 10240

logging:
  level: info
  format: json
```

`load.path` is required. There is no fallback or alias route.

## CLI

```bash
npm run build
node ./dist/cli.js validate-config --config ./config.yml
node ./dist/cli.js print-config --config ./config.yml
node ./dist/cli.js serve --config ./config.yml
```

## Docker

The public image is published as:

```bash
docker pull ghcr.io/a1ekseev/gtl:latest
```

Release tags are shared with the repository release tags, for example `v1.0.2` publishes `ghcr.io/a1ekseev/gtl:1.0.2`.
