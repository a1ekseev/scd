# sfc

`sfc` stands for `Subscription Filter Control`.
It loads upstream subscription sources, filters supported VLESS lines by label, repacks the result to base64, and serves the filtered payload by public URL.

Official Docker image:
- `ghcr.io/a1ekseev/sfc:latest`
- `ghcr.io/a1ekseev/sfc:<version>`

Example:

```bash
docker pull ghcr.io/a1ekseev/sfc:latest
```

The image is published on the same release tags `vX.Y.Z` as `scd`.

## Requirements
- Node `24.14.0` or newer
- `npm`

## Install and build

```bash
cd sfc
npm install
npm run build
```

## Main commands

Validate YAML config:

```bash
cd sfc
node ./dist/cli.js validate-config --config ./config.yml
```

Print normalized config:

```bash
cd sfc
node ./dist/cli.js print-config --config ./config.yml
```

Run one refresh cycle:

```bash
cd sfc
node ./dist/cli.js refresh --config ./config.yml
```

Run HTTP server:

```bash
cd sfc
node ./dist/cli.js serve --config ./config.yml
```

## YAML config

Local sample:
- `./config.yml`

Docker sample:
- `../docker/sfc/config.yml`

Example:

```yaml
subscriptions:
  - id: main
    input: ../vpn
    enabled: true
    format: auto
    fetchTimeoutMs: 5000
    pathRoute: /s
    outputs:
      - id: a8f3c9d2
        enabled: true
        name: germany-extra
        labelIncludeRegex: '/Германия.*Extra$/i'
        userAgent:
          - Clash
          - Stash
        profileTitle: Germany Extra
        profileUpdateInterval: 6
      - id: b1e7f442
        enabled: true
        name: netherlands-extra
        labelIncludeRegex: '/Нидерланды.*Extra$/i'

runtime:
  refreshSchedule: "*/10 * * * *"

server:
  listen: 0.0.0.0:8081

logging:
  level: info
  format: json
```

Parameters:
- `subscriptions`: list of subscription sources.
- `subscriptions[].id`: internal operator id used in logs and summaries.
- `subscriptions[].input`: local file path, `http(s)` URL or `-` for stdin.
- `subscriptions[].enabled`: enables or disables this source. Default: `true`.
- `subscriptions[].format`: input decoding mode. Allowed values: `auto`, `plain`, `base64`. Default: `auto`.
- `subscriptions[].fetchTimeoutMs`: timeout for remote source download in milliseconds. Default: `5000`.
- `subscriptions[].pathRoute`: required route prefix used for all outputs of this subscription. Example: `/profiles`.
- `subscriptions[].outputs`: list of public filtered outputs for one source.
- `subscriptions[].outputs[].id`: globally unique opaque public id served at `<pathRoute>/<id>`.
- `subscriptions[].outputs[].enabled`: enables or disables this public output. Default: `true`.
- `subscriptions[].outputs[].name`: optional operator-friendly label for logs.
- `subscriptions[].outputs[].labelIncludeRegex`: JavaScript regex literal applied to parsed subscription `label`.
- `subscriptions[].outputs[].userAgent`: optional list of `User-Agent` prefixes checked with `startsWith`.
- `subscriptions[].outputs[].profileTitle`: optional title sent as response header `profile-title` in `base64:<encoded>` format, but only when a configured `userAgent` prefix matches.
- `subscriptions[].outputs[].profileUpdateInterval`: optional integer from `1` to `24` sent as `profile-update-interval`, but only when a configured `userAgent` prefix matches.
- `runtime.refreshSchedule`: cron expression for background refresh.
- `server.listen`: listen address in `host:port` form.
- `logging.level`: `fatal`, `error`, `warn`, `info`, `debug`, `trace`, `silent`. Default: `info`.
- `logging.format`: `json` or `pretty`. Default: `json`.

## Public HTTP API

- `GET /healthz`: returns `200 OK` when the process is running.
- `GET <pathRoute>/<output-id>`: returns the current cached filtered subscription payload as plain base64 text.

Response semantics for `GET <pathRoute>/<output-id>`:
- unknown `output-id` -> `404`
- configured output without a successful cached payload yet -> `502`
- successful output -> `200` with `text/plain; charset=utf-8`
- if request `User-Agent` matches any configured `subscriptions[].outputs[].userAgent` prefix, `sfc` may also send:
  - `profile-title: base64:<utf8-base64(profileTitle)>`
  - `profile-update-interval: <1..24>`

`sfc` always serves the last good cached payload. A refresh failure does not delete the previous successful cache entry.
The background refresh loop does not overlap with itself. If one refresh is still running when the next cron slot arrives, `sfc` skips that slot and logs `refresh_tick_skipped_overrun`.
