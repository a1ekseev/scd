# sfc: Agent Context

## Purpose
- `sfc` is a standalone sibling project to `scd`.
- It does not talk to Xray.
- It fetches subscription sources, keeps supported VLESS lines, filters by parsed `label`, repacks the result to base64, and serves it over HTTP.

## Runtime model
- One source can produce many public outputs.
- Each output has a globally unique opaque `id`.
- Server startup performs an initial refresh before listening.
- Background refresh is driven by one global cron schedule.
- Output serving is read-only and never fetches upstream on demand.
- Server lookup uses cached outputs by `pathRoute + "/" + output.id`.
- Last good output payload is kept in memory.
- Last successful decoded source subscription is persisted in `.sfc-cache` next to the active config file and is used as refresh fallback when upstream input fails.

## Output rules
- Filter only successfully parsed supported VLESS lines.
- Filter by parsed `labelIncludeRegex`.
- Keep original raw lines in original order.
- Repack the filtered plain text to base64 before exposing it publicly.

## Failure rules
- If upstream fetch fails, try disk source cache first; if it exists, rebuild outputs from it.
- If upstream fetch fails and disk source cache is missing, keep previous successful output payloads.
- If a filtered output becomes empty, mark the refresh as failed for that output and keep the previous successful payload.
- If no successful payload exists yet, `GET <pathRoute>/<output-id>` must return `502`.

## Public endpoints
- `GET /healthz`
- `GET <pathRoute>/<output-id>`

## Non-goals in v1
- no admin UI
- no auth
- no country filter
- no shared library refactor with `scd`

## Config Notes
- `subscriptions[].pathRoute` is required and has no default.
- `subscriptions[].outputs[].id` must be globally unique.
- `subscriptions[].outputs[].userAgent` gates optional `profile-title` and `profile-update-interval` headers only; mismatch does not block payload delivery.

## CLI Surface
- `node ./dist/cli.js serve --config ./config.yml`
- `node ./dist/cli.js refresh --config ./config.yml`
- `node ./dist/cli.js validate-config --config ./config.yml`
- `node ./dist/cli.js print-config --config ./config.yml`

## Docker Image
- `ghcr.io/a1ekseev/sfc`

## Verification
- Run `npm run typecheck`, `npm test`, and `npm run build` from `sfc/`.
- Validate local config with `node ./dist/cli.js validate-config --config ./config.yml`.
- Validate Docker config with `node ./dist/cli.js validate-config --config ../docker/sfc/config.yml`.
- Docker smoke uses root `docker compose up -d --build sfc`.
