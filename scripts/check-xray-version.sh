#!/usr/bin/env bash
# Checks that the Xray version pinned in .xray-version matches both the
# xray-core Go dependency in go.mod (by commit) and the Xray image tag in
# docker-compose.yml (D4: they are updated only together), and that uTLS is a
# direct dependency at exactly the version xray-core requires.
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
# Any pseudo-version whose commit suffix is the pinned commit (the prefix
# changes with every Xray bump).
elif ! grep -qE "github\.com/xtls/xray-core v[^[:space:]]*-${commit}([[:space:]]|$)" <<<"$gomod_line"; then
  printf 'check-xray-version: go.mod xray-core does not match .xray-version (want commit %s for Xray %s):\n  %s\n' \
    "$commit" "$version" "$(sed -E 's/^[[:space:]]+//' <<<"$gomod_line")" >&2
  status=1
fi

image="ghcr.io/xtls/xray-core:${version}"
if ! grep -qE "image:[[:space:]]*[\"']?${image//./\\.}[\"']?[[:space:]]*(#.*)?$" "$compose"; then
  found="$(grep -oE 'ghcr\.io/xtls/xray-core:[^[:space:]"'\'']+' "$compose" || true)"
  printf 'check-xray-version: docker-compose.yml must use image %s, found: %s\n' "$image" "${found:-<none>}" >&2
  status=1
fi

utls=github.com/refraction-networking/utls
xray_mod="$(cd "$root" && go list -m -f '{{.Path}}@{{.Version}}' github.com/xtls/xray-core)"
utls_want="$(cd "$root" && go mod graph | awk -v from="$xray_mod" -v to="$utls@" '$1 == from && index($2, to) == 1 { print substr($2, length(to) + 1) }')"
utls_have="$(cd "$root" && go list -m -f '{{.Version}} {{.Indirect}}' "$utls")"
if [[ -z "$utls_want" || "$utls_have" != "$utls_want false" ]]; then
  printf 'check-xray-version: go.mod must require %s %s directly (the version %s requires), found: %s\n' \
    "$utls" "${utls_want:-<none>}" "$xray_mod" "${utls_have:-<none>}" >&2
  status=1
fi

if [[ $status -ne 0 ]]; then
  printf 'check-xray-version: update go.mod, docker-compose.yml and .xray-version together (D4)\n' >&2
  exit "$status"
fi
printf 'check-xray-version: ok (Xray %s, commit %s, uTLS %s)\n' "$version" "$commit" "$utls_want"
