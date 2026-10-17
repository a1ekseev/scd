#!/usr/bin/env bash
# Checks that the repository contains no Node/TypeScript code and that the
# top-level docs, Docker files, CI and scripts do not mention the Node tool
# chain (D20). The browser script of the status dashboard
# (internal/scd/status/web/dashboard.js) is not Node code and is excluded.
# Go sources are not grepped (the dashboard CSS class `node-card` is fine);
# docs/go-migration holds the historical migration notes and is not checked.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

status=0

fail() {
  printf 'check-no-node: %s\n' "$*" >&2
  exit 2
}

tracked="$(git ls-files -- ':!internal/scd/status/web/')" || fail "git ls-files failed"
files="$(grep -E '(\.(ts|js|mjs)$|(^|/)package(-lock)?\.json$|(^|/)tsconfig[^/]*\.json$)' <<<"$tracked" || true)"
if [[ -n "$files" ]]; then
  printf 'check-no-node: Node/TypeScript files are tracked:\n%s\n' "$(head -n 20 <<<"$files" | sed 's/^/  /')" >&2
  count="$(wc -l <<<"$files")"
  if ((count > 20)); then
    printf '  ... (%d files in total)\n' "$count" >&2
  fi
  status=1
fi

# The name of this script is the only allowed match (docs and CI call it).
pattern='\b(npm|node|nodejs|node\.js|typescript|tsc|node_modules|pino|zod)\b'
# git grep exits 1 when nothing matches and 2 or more on a real error.
rc=0
found="$(git grep -nIiE "$pattern" -- \
  README.md AGENTS.md Dockerfile .dockerignore docker-compose.yml docker .github scripts \
  ':!scripts/check-no-node.sh')" || rc=$?
if ((rc > 1)); then
  fail "git grep failed (exit $rc)"
fi
mentions="$(grep -iE "$pattern" <<<"${found//check-no-node/}" || true)"
if [[ -n "$mentions" ]]; then
  printf 'check-no-node: Node tool chain is mentioned:\n%s\n' "$(head -n 20 <<<"$mentions" | sed 's/^/  /')" >&2
  status=1
fi

if [[ $status -ne 0 ]]; then
  exit "$status"
fi
printf 'check-no-node: ok\n'
