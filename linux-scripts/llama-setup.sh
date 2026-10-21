#!/usr/bin/env bash
#
# llama.cpp (CPU build) + one GGUF model + llama-server as a systemd service,
# for Ubuntu/systemd, x86_64. Follows https://github.com/ggml-org/llama.cpp
# docs/build.md (CPU build, static) and tools/server.
#
# Run ON THE SERVER as root:
#   sudo bash llama-setup.sh
#
# Override defaults with env vars, e.g.:
#   sudo HOST=0.0.0.0 API_KEY=changeme CTX=32768 bash llama-setup.sh
#   sudo MODEL_REPO=unsloth/Qwen3.6-35B-A3B-GGUF \
#        MODEL_FILE=Qwen3.6-35B-A3B-UD-Q4_K_M.gguf bash llama-setup.sh
#
# Safe to re-run: the source is updated and rebuilt, an already downloaded
# model is not downloaded again, and the service is restarted.

set -euo pipefail

LLAMA_REF="${LLAMA_REF:-master}"     # git branch or tag to build
MODEL_REPO="${MODEL_REPO:-unsloth/gemma-4-E2B-it-GGUF}"
MODEL_FILE="${MODEL_FILE:-gemma-4-E2B-it-UD-Q4_K_XL.gguf}"
HOST="${HOST:-127.0.0.1}"            # use 0.0.0.0 to reach it from the LAN (set API_KEY too)
PORT="${PORT:-8081}"              # 8080 is taken by the garage UI
API_KEY="${API_KEY:-}"               # empty = no auth
CTX="${CTX:-16384}"                  # context size in tokens
THREADS="${THREADS:-$(lscpu -p=CORE | grep -v '^#' | sort -u | wc -l)}"  # physical cores

SRC_DIR=/opt/llama.cpp
STATE_DIR=/var/lib/llama
MODEL_DIR=$STATE_DIR/models
ENV_FILE=/etc/llama-server.env
UNIT=/etc/systemd/system/llama-server.service

log() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (sudo bash $0)"
[[ $(uname -m) == x86_64 ]] || die "this script targets x86_64; found $(uname -m)"
command -v systemctl >/dev/null || die "systemd is required"
command -v apt-get >/dev/null || die "apt-get is required (Ubuntu/Debian)"

# --- 1. Build dependencies ---------------------------------------------------
log "Installing build dependencies"
DEBIAN_FRONTEND=noninteractive apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  build-essential cmake git curl ccache libssl-dev ca-certificates >/dev/null

# --- 2. Source ---------------------------------------------------------------
if [[ -d $SRC_DIR/.git ]]; then
  log "Updating llama.cpp source ($LLAMA_REF)"
  git -C "$SRC_DIR" fetch --tags --quiet origin
else
  log "Cloning llama.cpp"
  git clone --quiet https://github.com/ggml-org/llama.cpp "$SRC_DIR"
fi
git -C "$SRC_DIR" checkout --quiet "$LLAMA_REF"
git -C "$SRC_DIR" symbolic-ref -q HEAD >/dev/null && git -C "$SRC_DIR" pull --quiet --ff-only
git -C "$SRC_DIR" log -1 --format='    at %h %s'

# --- 3. Build ----------------------------------------------------------------
# GGML_NATIVE (on by default) tunes for this CPU (AVX2 etc.). Static so the
# installed binaries don't depend on the build tree.
log "Building (this takes a while on a small CPU)"
cmake -S "$SRC_DIR" -B "$SRC_DIR/build" -DCMAKE_BUILD_TYPE=Release \
  -DBUILD_SHARED_LIBS=OFF -DLLAMA_BUILD_TESTS=OFF >/dev/null
cmake --build "$SRC_DIR/build" --config Release -j "$(nproc)" \
  --target llama-server llama-cli llama-bench >/dev/null
for b in llama-server llama-cli llama-bench; do
  install -m 0755 "$SRC_DIR/build/bin/$b" /usr/local/bin/$b
done
/usr/local/bin/llama-server --version 2>&1 | head -2

# --- 4. Service user ---------------------------------------------------------
if ! id llama >/dev/null 2>&1; then
  log "Creating system user 'llama'"
  useradd --system --home-dir "$STATE_DIR" --no-create-home --shell /usr/sbin/nologin llama
fi
install -d -m 0755 -o llama -g llama "$STATE_DIR" "$MODEL_DIR"

# --- 5. Model ----------------------------------------------------------------
MODEL_PATH=$MODEL_DIR/$MODEL_FILE
if [[ -s $MODEL_PATH ]]; then
  log "Model already present: $MODEL_PATH"
else
  log "Downloading $MODEL_REPO/$MODEL_FILE"
  # -C - resumes a partial download if the script was interrupted
  sudo -u llama curl -fL --proto '=https' --tlsv1.2 -C - -o "$MODEL_PATH.part" \
    "https://huggingface.co/$MODEL_REPO/resolve/main/$MODEL_FILE"
  mv "$MODEL_PATH.part" "$MODEL_PATH"
fi

# --- 6. Config + systemd unit ------------------------------------------------
# llama-server reads LLAMA_ARG_* / LLAMA_API_KEY from the environment.
log "Writing $ENV_FILE and $UNIT"
umask 077
cat > "$ENV_FILE" <<EOF
LLAMA_ARG_MODEL=$MODEL_PATH
LLAMA_ARG_HOST=$HOST
LLAMA_ARG_PORT=$PORT
LLAMA_ARG_CTX_SIZE=$CTX
LLAMA_ARG_THREADS=$THREADS
EOF
[[ -n $API_KEY ]] && echo "LLAMA_API_KEY=$API_KEY" >> "$ENV_FILE"
chmod 0600 "$ENV_FILE"
umask 022

cat > "$UNIT" <<EOF
[Unit]
Description=llama.cpp server
After=network-online.target
Wants=network-online.target

[Service]
User=llama
Group=llama
EnvironmentFile=$ENV_FILE
ExecStart=/usr/local/bin/llama-server
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$STATE_DIR

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --quiet llama-server
systemctl restart llama-server

# --- 7. Wait for the model to load -------------------------------------------
log "Waiting for llama-server to load the model"
for _ in $(seq 1 120); do
  curl -fs "http://127.0.0.1:$PORT/health" >/dev/null 2>&1 && break
  systemctl is-active --quiet llama-server || { journalctl -u llama-server -n 30 --no-pager; die "llama-server failed to start"; }
  sleep 2
done
curl -fs "http://127.0.0.1:$PORT/health" >/dev/null || die "llama-server not healthy after 4 min (journalctl -u llama-server)"

log "Done"
cat <<EOF
    Model:   $MODEL_PATH
    Web UI:  http://$HOST:$PORT/
    API:     http://$HOST:$PORT/v1   (OpenAI-compatible)
    Logs:    journalctl -u llama-server -f
    Bench:   llama-bench -m $MODEL_PATH -t $THREADS
EOF
if [[ $HOST == 127.0.0.1 ]]; then echo "    Local only: query it on the server, or re-run with HOST=0.0.0.0 API_KEY=... for LAN access"; fi
