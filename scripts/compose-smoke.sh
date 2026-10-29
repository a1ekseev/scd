#!/usr/bin/env bash
# Compose smoke test: builds the single scd image, starts xray + scd + sfc + gtl
# + wgp from docker-compose.yml with docker/compose.smoke.yml (tracked
# config.example.yml files; the ignored docker/*/config.yml are never touched,
# D12) and checks each service end to end. wgp publishes the static fixtures
# and scd applies them as WireGuard tunnels through the read-only volume.
# Always tears the stack down.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

project=scd-smoke
compose=(docker compose -p "$project" --profile wgp -f docker-compose.yml -f docker/compose.smoke.yml)
timeout_s=60
fixed_names=(xray-test scd sfc gtl wgp)
host_ports=(8080 9090 9091 9092 9093)

log() { printf 'compose-smoke: %s\n' "$*" >&2; }
die() {
  log "FAIL: $*"
  exit 1
}

for tool in docker curl jq sha256sum base64 tar; do
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

# The example syncs every 5 minutes; the smoke copy syncs every 5 seconds so
# that the Xray restart check below does not wait for a 5-minute slot.
smoke_dir="$(mktemp -d)"
sed -E 's|^(  schedule: ).*|\1"*/5 * * * * *"|' docker/scd/config.example.yml >"$smoke_dir/scd.yml"
grep -q '^  schedule: "\*/5 \* \* \* \* \*"$' "$smoke_dir/scd.yml" || die "runtime.schedule not found in docker/scd/config.example.yml"
export SCD_SMOKE_SCD_CONFIG="$smoke_dir/scd.yml"

# The wireguard source "wg" of the example reads /app/wgp/wg, the output
# of wgp static: enable it in the copy.
awk '/^      - id: /{wg = ($3 == "wg")} wg && /^        enabled: false$/{sub(/false/, "true"); wg = 0} {print}' \
  "$smoke_dir/scd.yml" >"$smoke_dir/scd.yml.tmp" && mv "$smoke_dir/scd.yml.tmp" "$smoke_dir/scd.yml"
grep -A3 -- '^      - id: wg$' "$smoke_dir/scd.yml" | grep -q '^        enabled: true$' ||
  die "wireguard source wg not found in docker/scd/config.example.yml"

# wgp inputs: the static fixtures (git keeps them 0644, static wants no
# access for others) and a fresh state key. Docker Desktop shows bind-mounted
# files as owned by the reading user; on Linux they keep the host owner, so
# they are given to 65532 as docker/README.md says (sudo on CI runners).
static_count=0
mkdir -m 0755 "$smoke_dir/static"
for f in docker/fixtures/static/*.conf; do
  install -m 0600 "$f" "$smoke_dir/static/"
  static_count=$((static_count + 1))
done
((static_count > 0)) || die "no docker/fixtures/static/*.conf"
(umask 077 && head -c 32 /dev/urandom | base64 >"$smoke_dir/wgp_state_key")
if [[ "$(uname -s)" == Linux ]]; then
  as_root=()
  if ((EUID != 0)); then
    command -v sudo >/dev/null || die "not root and no sudo: cannot chown the wgp inputs to 65532"
    as_root=(sudo -n)
  fi
  "${as_root[@]}" chown 65532:65532 "$smoke_dir/wgp_state_key" "$smoke_dir"/static/*.conf ||
    die "cannot chown the wgp inputs to 65532 (needs root or passwordless sudo)"
fi
export SCD_SMOKE_WGP_STATIC="$smoke_dir/static" SCD_SMOKE_WGP_KEY="$smoke_dir/wgp_state_key"

# Leftovers of an interrupted earlier smoke run.
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

# The body is random per request, so check the length only.
gtl_load_len() {
  [[ "$(curl -fsS http://127.0.0.1:9092/load/1 | wc -c | tr -d ' ')" == 1024 ]]
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

# The WireGuard candidates are real Xray outbounds (runtime, not only expected).
scd_runtime_state_wireguard() {
  curl -fsS 'http://127.0.0.1:9090/api/runtime-state?subscriptionId=test-fixture&targetAddress=xray:8080' |
    jq -e '[.runtime.outbounds[] | select(.parsed.protocol == "wireguard" and .classification == "managed-initial")] |
      length == 2 and all(.parsed.endpoint | startswith("192.0.2."))'
}

# GetBalancerInfo + GetOutboundStatus answered (ObservatoryService, D22).
scd_balancer_selection() {
  curl -fsS http://127.0.0.1:9090/api/instance-status |
    jq -e '.subscriptions[0].targets[0].balancerSelection.state != null'
}

# wgp: static published every fixture, status.json is written, and both are
# 0600 files of 65532 on the output volume (listed from a docker cp tar).
wgp_status_ok() {
  curl -fsS http://127.0.0.1:9093/api/status |
    jq -e --argjson n "$static_count" '.version == 1 and (.providers | length) == 1 and
      .providers[0].type == "static" and .providers[0].state == "ok" and .providers[0].issued == $n'
}
wgp_files() {
  local listing
  listing="$(docker cp wgp:/app/out - | tar -tvf -)"
  for f in wg status.json; do
    grep -qE "^-rw-------[[:space:]].*65532[[:space:]/]+65532[[:space:]].*out/$f\$" <<<"$listing" || {
      echo "$listing"
      echo "out/$f: not a 0600 file of 65532:65532"
      return 1
    }
  done
  [[ "$(docker cp wgp:/app/out/wg - | tar -xOf - | grep -c '^wireguard://.*[?&]provider=static[&#]')" == "$static_count" ]]
}
wgp_logged_ignored_keys() {
  grep -qE 'event(=|":")provider_warning' <<<"$("${compose[@]}" logs --no-color wgp)"
}

# scd read status.json and wg through its :ro mount and applied the
# candidates as WireGuard tunnels (they need not come up: fake keys).
scd_wgp_block() {
  curl -fsS http://127.0.0.1:9090/api/instance-status |
    jq -e '(.wgp | length) == 1 and .wgp[0].providers[0].type == "static" and .wgp[0].providers[0].state == "ok"'
}
scd_wireguard_tunnels() {
  curl -fsS http://127.0.0.1:9090/api/instance-status |
    jq -e '[.tunnels[] | select(.protocol == "wireguard" and .sourceId == "wg")] | length == 2'
}

poll "gtl /load/1 is 1024 bytes" gtl_load_len
poll "sfc /s/test-all decodes to 8 lines" sfc_test_all_lines
poll "scd /api/instance-status version 1" scd_instance_status
poll "scd runtime-state: outbounds applied to Xray" scd_runtime_state
if grep -qE '^[[:space:]]*balancerSelection:' docker/scd/config.example.yml; then
  poll "scd balancerSelection state" scd_balancer_selection
fi
poll "wgp /api/status: static ok, $static_count candidates" wgp_status_ok
poll "wgp wrote wg ($static_count lines) and status.json, 0600 65532:65532" wgp_files
poll "wgp logged the ignored .conf keys" wgp_logged_ignored_keys
poll "scd /api/instance-status: wgp block of static" scd_wgp_block
poll "scd /api/instance-status: 2 wireguard tunnels of source wg" scd_wireguard_tunnels
poll "scd runtime-state: wireguard outbounds applied to Xray" scd_runtime_state_wireguard

# The image runs as nonroot (65532), and sfc must still be able to write its
# source cache (a failed write is only a warning, so check that a refresh
# logged no such warning; the write itself is debug).
for svc in scd sfc gtl wgp; do
  user="$(docker inspect --format '{{.Config.User}}' "$svc")"
  [[ "$user" == "65532:65532" ]] || die "$svc runs as user '$user', want 65532:65532"
done
log "ok: scd, sfc, gtl and wgp run as 65532:65532"
sfc_cache_written() {
  local logs
  logs="$("${compose[@]}" logs --no-color sfc)"
  grep -q 'subscription_refreshed' <<<"$logs" && ! grep -q 'subscription_source_cache_write_failed' <<<"$logs"
}
poll "sfc wrote its source cache as nonroot" sfc_cache_written

# An Xray restart drops every API-added resource; with an unchanged manifest
# the next sync (or the monitor first) must notice it and apply again.
scd_logged_drift() {
  # Not a pipe: grep -q would exit early and fail the pipe under pipefail.
  grep -qE 'event(=|":")xray_state_(drift|lost)' <<<"$("${compose[@]}" logs --no-color scd)"
}
log "restarting Xray"
docker restart xray-test >&2
poll "scd logged xray_state_drift or xray_state_lost after the Xray restart" scd_logged_drift
poll "scd runtime-state: outbounds applied again" scd_runtime_state

ok=1
log "PASS"
