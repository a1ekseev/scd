#!/usr/bin/env bash
# Checks that the Xray version pinned in .xray-version matches both the
# xray-core Go dependency in go.mod (by commit) and the Xray image tag in
# docker-compose.yml (D4: they are updated only together).
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pin_file="$root/.xray-version"
gomod="$root/go.mod"
compose="$root/docker-compose.yml"

fail() {
  printf 'check-xray-version: %s\n' "$*" >&2
  exit 1
}

[[ -f "$pin_file" ]] || fail "missing $pin_file"
read -r version commit extra <"$pin_file" || true
[[ -n "${version:-}" && -n "${commit:-}" && -z "${extra:-}" ]] ||
  fail ".xray-version must contain exactly one line '<version> <commit12>', got: $(head -n1 "$pin_file")"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "bad version '$version' in .xray-version"
[[ "$commit" =~ ^[0-9a-f]{12}$ ]] || fail "bad commit '$commit' in .xray-version (want 12 hex chars)"

status=0

gomod_line="$(grep -E '^[[:space:]]*(require[[:space:]]+)?github\.com/xtls/xray-core[[:space:]]' "$gomod" || true)"
if [[ -z "$gomod_line" ]]; then
  printf 'check-xray-version: go.mod has no github.com/xtls/xray-core requirement\n' >&2
  status=1
elif ! grep -qE "github\.com/xtls/xray-core v1\.260327\.1-0\.20261010092107-${commit}([[:space:]]|$)" <<<"$gomod_line"; then
  printf 'check-xray-version: go.mod xray-core does not match .xray-version (want commit %s for Xray %s):\n  %s\n' \
    "$commit" "$version" "$(sed -E 's/^[[:space:]]+//' <<<"$gomod_line")" >&2
  status=1
fi

image="ghcr.io/xtls/xray-core:${version}"
if ! grep -qE "image:[[:space:]]*[\"']?${image//./\\.}[\"']?[[:space:]]*$" "$compose"; then
  found="$(grep -oE 'ghcr\.io/xtls/xray-core:[^[:space:]"'\'']+' "$compose" || true)"
  printf 'check-xray-version: docker-compose.yml must use image %s, found: %s\n' "$image" "${found:-<none>}" >&2
  status=1
fi

if [[ $status -ne 0 ]]; then
  printf 'check-xray-version: update go.mod, docker-compose.yml and .xray-version together (D4)\n' >&2
  exit "$status"
fi
printf 'check-xray-version: ok (Xray %s, commit %s)\n' "$version" "$commit"
