# blanc: Agent Context

## Repository Shape
- This repository contains independent sibling services:
  - `scd`: Subscription Control Daemon, Xray API manager and status dashboard.
  - `sfc`: Subscription Filter Control, subscription filter and public base64 payload server.
  - `gtl`: Gen Test Load, deterministic HTTP load payload generator.
- Service-specific agent notes:
  - `scd/AGENTS.md`
  - `sfc/AGENTS.md`
  - `gtl/AGENTS.md`

## Common Rules
- Each service is a standalone Node `24.14.0+` / TypeScript package with its own `package.json`, tests, and Dockerfile.
- Do not introduce cross-service shared libraries unless explicitly requested.
- Run service-local checks from the service directory:
  - `npm run typecheck`
  - `npm test`
  - `npm run build`
  - `node ./dist/cli.js validate-config --config ./config.yml`
- Root `docker-compose.yml` is used for local runtime smoke checks.
- `**/config.yml` is ignored by git. Local and `docker/<service>/config.yml` files can exist for runtime testing, but agents must not assume they are tracked artifacts.
- Before Docker smoke checks, verify that the relevant ignored `docker/<service>/config.yml` exists and matches the service config contract.

## CI Contract
- `.github/workflows/docker.yml` verifies `scd`, `sfc`, and `gtl` on pull requests and pushes to `master`.
- Release tags `vX.Y.Z` publish Docker images for all three services.

## Docker Images
- `scd`: `ghcr.io/a1ekseev/scd`
- `sfc`: `ghcr.io/a1ekseev/sfc`
- `gtl`: `ghcr.io/a1ekseev/gtl`

Release tags are shared across services.
