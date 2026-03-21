# sfc AI Notes

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
- Last good payload is kept in memory and survives refresh failures until process restart.

## Output rules
- Filter only successfully parsed supported VLESS lines.
- Filter by parsed `labelIncludeRegex`.
- Keep original raw lines in original order.
- Repack the filtered plain text to base64 before exposing it publicly.

## Failure rules
- If upstream fetch fails, keep previous successful output payloads.
- If a filtered output becomes empty, mark the refresh as failed for that output and keep the previous successful payload.
- If no successful payload exists yet, `GET /s/<id>` must return `502`.

## Public endpoints
- `GET /healthz`
- `GET /s/<output-id>`

## Non-goals in v1
- no admin UI
- no auth
- no persistence
- no country filter
- no shared library refactor with `scd`
