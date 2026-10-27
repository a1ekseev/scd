#!/usr/bin/env bash
# Compose smoke test: builds the single scd image, starts xray + scd + sfc + gtl
# from docker-compose.yml with docker/compose.smoke.yml (tracked
# config.example.yml files; the ignored docker/*/config.yml are never touched,
# D12) and checks each service end to end. Always tears the stack down.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

project=scd-smoke
compose=(docker compose -p "$project" -f docker-compose.yml -f docker/compose.smoke.yml)
timeout_s=60
gtl_sha256=8ab783947bae918d01b74448625dfd0fef119e16e97d6fc9fdf15e310c87cb01
fixed_names=(xray-test scd sfc gtl)
host_ports=(8080 9090 9091 9092)

log() { printf 'compose-smoke: %s\n' "$*" >&2; }
die() {
  log "FAIL: $*"
  exit 1
}

for tool in docker curl jq sha256sum base64; do
  command -v "$tool" >/dev/null || die "required tool '$tool' not found"
done

# docker-compose.yml uses fixed container_name values and host ports, so the
# smoke cannot run next to another stack started from it (e.g. `docker compose up`).
conflicts=()
for name in "${fixed_names[@]}"; do
  owner="$(docker container inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$name" 2>/dev/null)" || continue
  [[ "$owner" == "$project" ]] || conflicts+=("$name (project '${owner:-none}')")
done
if ((${#conflicts[@]} > 0)); then
  die "containers with the fixed names already exist: ${conflicts[*]}.
  Stop the other stack first (e.g. 'docker compose down' in $root), then rerun."
fi

# Leftovers of an interrupted earlier smoke run.
# The example syncs every 5 minutes; the smoke copy syncs every 5 seconds so
# that the Xray restart check below does not wait for a 5-minute slot.
smoke_dir="$(mktemp -d)"
sed -E 's|^(  schedule: ).*|\1"*/5 * * * * *"|' docker/scd/config.example.yml >"$smoke_dir/scd.yml"
grep -q '^  schedule: "\*/5 \* \* \* \* \*"$' "$smoke_dir/scd.yml" || die "runtime.schedule not found in docker/scd/config.example.yml"
export SCD_SMOKE_SCD_CONFIG="$smoke_dir/scd.yml"

"${compose[@]}" down -v --remove-orphans >&2

busy=()
for port in "${host_ports[@]}"; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    busy+=("$port")
  fi
done
((${#busy[@]} == 0)) || die "host ports already in use on 127.0.0.1: ${busy[*]}; free them and rerun."

ok=0
cleanup() {
  if [[ $ok -ne 1 ]]; then
    log "logs of the failed stack:"
    "${compose[@]}" ps -a >&2 || true
    "${compose[@]}" logs --no-color >&2 || true
  fi
  "${compose[@]}" down -v --remove-orphans >&2 || true
  rm -rf "$smoke_dir"
}
trap cleanup EXIT

log "building the image and starting the stack"
"${compose[@]}" up -d --build >&2

# poll DESCRIPTION CMD... — reruns CMD until it succeeds or timeout_s elapses.
poll() {
  local what=$1 deadline=$((SECONDS + timeout_s))
  shift
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      log "last attempt of: $what"
      "$@" >&2 || true
      die "$what: not ready within ${timeout_s}s"
    fi
    sleep 1
  done
  log "ok: $what"
}

gtl_load_sha() {
  [[ "$(curl -fsS http://127.0.0.1:9092/load/1 | sha256sum | cut -d' ' -f1)" == "$gtl_sha256" ]]
}

sfc_test_all_lines() {
  local lines
  lines="$(curl -fsS http://127.0.0.1:9091/s/test-all | base64 -d | grep -c .)"
  [[ "$lines" == 8 ]] || {
    echo "sfc /s/test-all: $lines lines, want 8"
    return 1
  }
}

scd_instance_status() {
  curl -fsS http://127.0.0.1:9090/api/instance-status | jq -e '.version == 1'
}

# The sync really applied the subscription to Xray 26.10.21.
scd_runtime_state() {
  curl -fsS 'http://127.0.0.1:9090/api/runtime-state?subscriptionId=test-fixture&targetAddress=xray:8080' |
    jq -e '.diff.outbounds.missing == [] and (.diff.outbounds.matched | length) > 0'
}

# GetBalancerInfo + GetOutboundStatus answered (ObservatoryService, D22).
scd_balancer_selection() {
  curl -fsS http://127.0.0.1:9090/api/instance-status |
    jq -e '.subscriptions[0].targets[0].balancerSelection.state != null'
}

poll "gtl /load/1 sha256" gtl_load_sha
poll "sfc /s/test-all decodes to 8 lines" sfc_test_all_lines
poll "scd /api/instance-status version 1" scd_instance_status
poll "scd runtime-state: outbounds applied to Xray" scd_runtime_state
if grep -qE '^[[:space:]]*balancerSelection:' docker/scd/config.example.yml; then
  poll "scd balancerSelection state" scd_balancer_selection
fi

# An Xray restart drops every API-added resource; with an unchanged manifest
# the next sync must notice it and apply again.
scd_logged_drift() {
  # Not a pipe: grep -q would exit early and fail the pipe under pipefail.
  grep -qE 'event(=|":")xray_state_drift' <<<"$("${compose[@]}" logs --no-color scd)"
}
log "restarting Xray"
docker restart xray-test >&2
poll "scd logged xray_state_drift after the Xray restart" scd_logged_drift
poll "scd runtime-state: outbounds applied again" scd_runtime_state

ok=1
log "PASS"
