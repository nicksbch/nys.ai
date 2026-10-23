#!/usr/bin/env bash
#
# Deploy everything to the server, in dependency order, from the current
# source. Run FROM YOUR MACHINE, in any directory:
#   ./deploy-all.sh
#
# Steps, in the order they run (* = only when named in ONLY):
#   garage         linux-scripts/garage-setup.sh   object store; everything else needs it
#   relay          relay/deploy.sh                 event log; apps publish to it
#   vigil          vigil/deploy.sh                 monitor; after relay: reads app events from it
#   preprocessor   preprocessor/deploy.sh          unzips archives; before duckpipe, which loads what it writes
#   duckpipe *     duckpipe/deploy.sh              before ducklake, which points it at the lake
#   ducklake       linux-scripts/ducklake-setup.sh PostgreSQL catalog + lake bucket
#   garage-ui      garage-ui/deploy.sh
#   lake-ui        lake-ui/deploy.sh               after ducklake: reads its catalog settings
#   lakeshape      lakeshape/deploy.sh             after ducklake: builds ../analytics into the lake
#   semantic       semantic/deploy.sh              MCP server over the marts; after lakeshape
#   url-to-bucket  url-to-bucket/deploy.sh         service; after relay: takes download requests from it
#   mcp-hub        mcp-hub/deploy.sh               MCP gateway; after vigil and semantic: copies their MCP tokens
#   foreman        foreman/deploy.sh               agents; after mcp-hub: copies its token
#   analyst        analyst/deploy.sh               chat over the semantic layer; after mcp-hub: copies its token
#   home           home/deploy.sh                  homepage linking the UIs above
#   llama *        linux-scripts/llama-setup.sh    pulls and rebuilds llama.cpp (slow)
#
# Pick steps with ONLY or SKIP (they still run in the order above):
#   ONLY="vigil relay" ./deploy-all.sh
#   SKIP="garage ducklake" ./deploy-all.sh
#   ONLY=llama ./deploy-all.sh
#
# Before anything is touched on the server, every Go app in the selected
# steps is vetted, tested and built locally (semantic, in Python, is tested), so a broken build stops the run
# instead of leaving the server half updated. After the last step, a status
# check runs on the server.
#
# Settings: some install scripts rewrite their systemd unit or env file on
# every run (vigil's DISKS/EXPECT, garage-ui's BUCKETS, all of llama's), so
# they must be passed every time. Put them in deploy.env next to this script
# (see deploy.env.example); it is read if present. HOST and SSH_PORT may be
# set there or in the environment.
#
# Needs ssh access to the server and sudo there: expect one password prompt
# per step that installs a service. Safe to re-run.

set -euo pipefail

cd "$(dirname "$0")"
[[ -f deploy.env ]] && { set -a; . ./deploy.env; set +a; }

# The jobs vigil alerts on when they go quiet. Both data sources (price paid,
# data.police.uk) publish monthly, so each step should succeed at least every
# 35 days.
# VIGIL_EXPECT="${VIGIL_EXPECT-url-to-bucket/download=840h preprocessor/unzip=840h duckpipe/ingest=840h lakeshape/run=840h}"

HOST="${HOST:-192.168.1.169}"   # ssh destination; ~/.ssh/config supplies the user
SSH_PORT="${SSH_PORT:-2222}"
ONLY="${ONLY:-}"
SKIP="${SKIP:-}"

ORDER=(garage relay vigil preprocessor duckpipe ducklake garage-ui lake-ui lakeshape semantic url-to-bucket mcp-hub foreman analyst home llama)
OPT_IN=(duckpipe llama)   # skipped unless named in ONLY

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
has() { local x=$1; shift; for y in "$@"; do [[ $x == "$y" ]] && return 0; done; return 1; }

# Settings each step accepts. In deploy.env they carry the step's prefix,
# e.g. VIGIL_EXPECT is passed to vigil as EXPECT. Unprefixed copies in your
# shell are never passed on, so one app's LISTEN can't leak into another's.
# (Plain functions rather than associative arrays: macOS ships bash 3.2.)
prefix() { local p=$(tr 'a-z-' 'A-Z_' <<<"$1"); echo "$p"; }
vars() {
  case $1 in
    garage) echo "VERSION CAPACITY ZONE BUCKET KEY_NAME S3_BIND WEB_BIND" ;;
    vigil) echo "LISTEN DISKS EXPECT UI_USER WEBHOOK EXTRA_ARGS" ;;
    relay) echo "LISTEN EXTRA_ARGS" ;;
    preprocessor) echo "BUCKETS KEY_NAME ENDPOINT LISTEN UI_USER" ;;
    duckpipe) echo "SOURCE_BUCKETS LAKE_BUCKET KEY_NAME ENDPOINT DUCKDB_VERSION LISTEN UI_USER" ;;
    ducklake) echo "LAKE_BUCKET KEY_NAME PG_DB PG_USER S3_ENDPOINT DUCKDB_VERSION LAN_CIDR HOST_IP" ;;
    garage-ui) echo "LISTEN BUCKETS KEY_NAME UI_USER ENDPOINT" ;;
    lake-ui) echo "LISTEN LAKE_BUCKET KEY_NAME PG_ROLE UI_USER ENDPOINT" ;;
    lakeshape) echo "PROJECT LAKE_BUCKET KEY_NAME ENDPOINT DUCKDB_VERSION" ;;
    semantic) echo "LISTEN LAKE_BUCKET KEY_NAME PG_ROLE ENDPOINT" ;;
    url-to-bucket) echo "BUCKETS KEY_NAME WORKERS ENDPOINT LISTEN UI_USER" ;;
    mcp-hub) echo "LISTEN EXTRA_ARGS" ;;
    foreman) echo "LISTEN EXTRA_ARGS" ;;
    analyst) echo "LISTEN EXTRA_ARGS" ;;
    home) echo "LISTEN SERVICES" ;;
    llama) echo "REF MODEL_REPO MODEL_FILE HOST PORT API_KEY CTX THREADS" ;;
  esac
}
# The name a setting has inside the step's script: garage's VERSION and
# llama's REF are GARAGE_VERSION and LLAMA_REF there.
script_name() {
  case $1:$2 in
    garage:VERSION) echo GARAGE_VERSION ;;
    llama:REF) echo LLAMA_REF ;;
    *) echo "$2" ;;
  esac
}
# Steps run from a linux-scripts setup script instead of an app's deploy.sh.
setup_script() {
  case $1 in
    garage) echo garage-setup.sh ;;
    ducklake) echo ducklake-setup.sh ;;
    llama) echo llama-setup.sh ;;
  esac
}

# settings STEP: prints NAME=VALUE for each setting given in deploy.env.
settings() {
  local step=$1 v pv
  for v in $(vars "$step"); do
    pv="$(prefix "$step")_$v"
    if [[ -n ${!pv:-} ]]; then printf '%s=%s\n' "$(script_name "$step" "$v")" "${!pv}"; fi
  done
}

# --- Which steps -------------------------------------------------------------
for s in $ONLY $SKIP; do has "$s" "${ORDER[@]}" || die "unknown step '$s' (steps: ${ORDER[*]})"; done
steps=()
for s in "${ORDER[@]}"; do
  if [[ -n $ONLY ]]; then
    has "$s" $ONLY || continue
  else
    has "$s" "${OPT_IN[@]}" && continue
  fi
  has "$s" $SKIP && continue
  steps+=("$s")
done
[[ ${#steps[@]} -gt 0 ]] || die "no steps selected"
log "Deploying to $HOST: ${steps[*]}"

# --- Preflight: vet, test and build every Go app locally ---------------------
# semantic is the one Python app: its tests run with uv instead.
command -v go >/dev/null || die "go is required"
for s in "${steps[@]}"; do
  [[ -n $(setup_script "$s") ]] && continue
  log "Checking $s"
  if [[ -f $s/pyproject.toml ]]; then
    command -v uv >/dev/null || die "uv is required for $s"
    (cd "$s" && uv run -q --frozen pytest -q) || die "$s's tests fail; nothing was deployed"
    continue
  fi
  (cd "$s" && go vet ./... && go test -count=1 ./... && go build -o /dev/null .) || die "$s does not build or its tests fail; nothing was deployed"
done

# --- Steps -------------------------------------------------------------------
# run_setup STEP: copies a linux-scripts setup script to the server and runs
# it with sudo, passing only that step's settings.
run_setup() {
  local step=$1 script remote vars=() kv
  script=$(setup_script "$step")
  while IFS= read -r kv; do vars+=("$(printf %q "$kv")"); done < <(settings "$step")
  remote=$(ssh -p "$SSH_PORT" "$HOST" 'mktemp -d') || die "could not reach $HOST over ssh"
  scp -q -P "$SSH_PORT" "linux-scripts/$script" "$HOST:$remote/"
  ssh -t -p "$SSH_PORT" "$HOST" "sudo env ${vars[*]-} bash $remote/$script; rc=\$?; rm -rf $remote; exit \$rc"
}

# run_deploy STEP: runs the app's own deploy.sh with only that step's settings.
run_deploy() {
  local step=$1 vars=(-u HOST) kv v
  for v in $(vars "$step"); do vars+=(-u "$v"); done
  while IFS= read -r kv; do vars+=("$kv"); done < <(settings "$step")
  env "${vars[@]}" HOST="$HOST" SSH_PORT="$SSH_PORT" "./$step/deploy.sh"
}

done_steps=()
for s in "${steps[@]}"; do
  log "[$((${#done_steps[@]} + 1))/${#steps[@]}] $s"
  if [[ -n $(setup_script "$s") ]]; then
    run_setup "$s" || die "$s failed (done: ${done_steps[*]:-none}); fix it and re-run with ONLY=\"${steps[*]:${#done_steps[@]}}\""
  else
    run_deploy "$s" || die "$s failed (done: ${done_steps[*]:-none}); fix it and re-run with ONLY=\"${steps[*]:${#done_steps[@]}}\""
  fi
  done_steps+=("$s")
done

# --- Status ------------------------------------------------------------------
log "Status on $HOST"
ssh -p "$SSH_PORT" "$HOST" 'bash -s' <<'EOF'
row() { printf '  %-14s %s\n' "$1" "$2"; }
for u in garage vigil relay preprocessor url-to-bucket mcp-hub foreman analyst duckpipe lakeshape semantic postgresql garage-ui lake-ui home llama-server; do
  state=$(systemctl is-active "$u" 2>/dev/null)
  systemctl cat "$u" >/dev/null 2>&1 || state="not installed"
  row "$u" "$state"
done
# name:port[/path], /healthz by default. garage-ui and lake-ui have no health
# endpoint and ask for a login, so a 401 counts as up.
for h in vigil:8090 relay:8091 url-to-bucket:8093 preprocessor:8099 duckpipe:8100 mcp-hub:8094 foreman:8096 semantic:8097 analyst:8098 home:8000 garage-ui:8080/ lake-ui:8092/ llama-server:8081/health; do
  name=${h%%:*} port=${h#*:} path=/healthz
  [[ $port == */* ]] && path=/${port#*/} port=${port%%/*}
  code=$(curl -s -o /dev/null -m 3 -w '%{http_code}' "http://127.0.0.1:$port$path" 2>/dev/null)
  [[ $code == 2* || $code == 401 ]] && row "$name health" ok || row "$name health" "FAILED ($code)"
done
EOF
log "Done: ${done_steps[*]}"
