#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_PYTHON="/home/zephyr/go/src/hunyuan3d/.venv/bin/python"
PYTHON_BIN="${PYTHON_BIN:-$DEFAULT_PYTHON}"
LOG_FILE="${EMBEDDING_LOG:-$ROOT_DIR/logs/embedding-service.log}"
PROXY_URL="${PROXY_URL:-http://192.168.50.199:10810}"

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  cat <<'EOF'
Usage: scripts/start_embedding.sh [--no-server]

Starts the Python embedding HTTP service using the shared Hunyuan3D Python
environment, then (by default) launches the Go search-server in the
background with EMBEDDING_ENDPOINT pointed at it. The model is loaded from
the Hugging Face cache and downloaded through the configured proxy only
when it is missing. The search-server always runs without the proxy so
cloud-drive requests stay direct.

Options:
  --no-server     Start only the embedding service, skip the Go server

Environment overrides:
  PYTHON_BIN             Python executable
  SEARCH_SERVER          0 disables the Go server (same as --no-server)
  SEARCH_SERVER_LOG      logs/search-server.log (default)
  EMBEDDING_BACKEND      wemm (default) or hash
  WEMM_MODEL             tencent/WeMM-Embedding-2B (default)
  EMBEDDING_HOST         127.0.0.1 (default)
  EMBEDDING_PORT         7001 (default)
  EMBEDDING_DIMENSION    2048 (default; supported WeMM Matryoshka dimension)
  EMBEDDING_BATCH_SIZE   8 (default; lower to 4 or 1 if GPU memory is insufficient)
  VIDEO_SEARCH_PUBLIC_URL  external origin for absolute frame URLs (optional)
  WEMM_IMAGE_PROMPT      Represent this image. (default)
  WEMM_COMPRESSED_MIN_IMAGE_PIXELS  65536 (compressed profile default)
  WEMM_COMPRESSED_MAX_IMAGE_PIXELS  98304 (compressed profile default)
  PROXY_URL              http://192.168.50.199:10810 (default); none/direct disables proxy
  EMBEDDING_LOG          logs/embedding-service.log (default)
EOF
  exit 0
fi

if [[ ! -x "$PYTHON_BIN" ]]; then
  echo "python executable not found: $PYTHON_BIN" >&2
  echo "set PYTHON_BIN to the existing Hunyuan3D Python environment" >&2
  exit 1
fi

# ------------------------------------------------------------- Go server (first)
# The Go server is started before the embedding duplicate check on purpose:
# when the embedding service is already running, this script is still the
# quickest way to (re)launch just the Go server.
START_SEARCH_SERVER="${SEARCH_SERVER:-1}"
if [[ "${1:-}" == "--no-server" ]]; then
  START_SEARCH_SERVER=0
fi

start_search_server() {
  local binary="$ROOT_DIR/.build/search-server"
  if pgrep -f '[.]build/search-server' >/dev/null 2>&1; then
    echo "search-server: already running; skip"
    return 0
  fi
  if [[ ! -x "$binary" && -x /usr/local/go/bin/go ]]; then
    echo "search-server binary missing; building..."
    mkdir -p "$ROOT_DIR/.build/tmp" "$ROOT_DIR/.build/gocache"
    if TMPDIR="$ROOT_DIR/.build/tmp" GOCACHE="$ROOT_DIR/.build/gocache" \
      /usr/local/go/bin/go build -o "$binary.new" "$ROOT_DIR/cmd/search-server"; then
      mv "$binary.new" "$binary"
    fi
  fi
  if [[ ! -x "$binary" ]]; then
    echo "search-server binary missing: $binary" >&2
    echo "build it first: go build -o .build/search-server ./cmd/search-server" >&2
    return 1
  fi
  local server_log="${SEARCH_SERVER_LOG:-$ROOT_DIR/logs/search-server.log}"
  mkdir -p "$(dirname "$server_log")"
  # Load .env here (not inside the subshell) so the endpoint message below
  # reflects the same EMBEDDING_PORT the server actually gets.
  if [[ -f "$ROOT_DIR/.env" ]]; then
    set -a
    # shellcheck disable=SC1091
    source "$ROOT_DIR/.env"
    set +a
  fi
  local endpoint="http://127.0.0.1:${EMBEDDING_PORT:-7001}"
  (
    cd "$ROOT_DIR"
    unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
    export EMBEDDING_ENDPOINT="$endpoint"
    nohup "$binary" >>"$server_log" 2>&1 &
    echo $! >"$ROOT_DIR/.build/search-server.pid"
  )
  local pid
  pid="$(cat "$ROOT_DIR/.build/search-server.pid")"
  sleep 1
  if kill -0 "$pid" 2>/dev/null; then
    echo "search-server: pid=$pid log=$server_log endpoint=$endpoint"
  else
    echo "search-server exited immediately; check $server_log" >&2
    return 1
  fi
}

if [[ "$START_SEARCH_SERVER" == "1" ]]; then
  start_search_server
fi

if pgrep -af '[p]ython.*embedding_service\.py' >/dev/null 2>&1; then
  echo "embedding_service.py is already running; nothing left to do."
  exit 0
fi

mkdir -p "$(dirname "$LOG_FILE")"
case "${PROXY_URL,,}" in
  ""|none|direct|off)
    unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
    echo "network: direct (proxy disabled)"
    ;;
  *)
    export HTTP_PROXY="$PROXY_URL"
    export HTTPS_PROXY="$PROXY_URL"
    export ALL_PROXY="$PROXY_URL"
    export http_proxy="$PROXY_URL"
    export https_proxy="$PROXY_URL"
    export all_proxy="$PROXY_URL"
    echo "network: proxy=$PROXY_URL"
    ;;
esac
export HF_HUB_DISABLE_XET="${HF_HUB_DISABLE_XET:-1}"
export HF_HUB_ENABLE_HF_TRANSFER=0
export HF_HUB_DOWNLOAD_TIMEOUT="${HF_HUB_DOWNLOAD_TIMEOUT:-60}"
export EMBEDDING_BACKEND="${EMBEDDING_BACKEND:-wemm}"
export WEMM_MODEL="${WEMM_MODEL:-tencent/WeMM-Embedding-2B}"
export EMBEDDING_DIMENSION="${EMBEDDING_DIMENSION:-2048}"
export WEMM_IMAGE_PROMPT="${WEMM_IMAGE_PROMPT:-Represent this image.}"
export WEMM_COMPRESSED_MIN_IMAGE_PIXELS="${WEMM_COMPRESSED_MIN_IMAGE_PIXELS:-65536}"
export WEMM_COMPRESSED_MAX_IMAGE_PIXELS="${WEMM_COMPRESSED_MAX_IMAGE_PIXELS:-98304}"
export EMBEDDING_DEVICE="${EMBEDDING_DEVICE:-cuda}"
export EMBEDDING_BATCH_SIZE="${EMBEDDING_BATCH_SIZE:-8}"

echo "embedding log: $LOG_FILE"
echo "python: $PYTHON_BIN"
echo "backend: $EMBEDDING_BACKEND"
echo "model: $WEMM_MODEL"
echo "endpoint: http://${EMBEDDING_HOST:-127.0.0.1}:${EMBEDDING_PORT:-7001}"
echo "inference: batch=$EMBEDDING_BATCH_SIZE image_profiles=original,compressed compressed_pixels=${WEMM_COMPRESSED_MIN_IMAGE_PIXELS}-${WEMM_COMPRESSED_MAX_IMAGE_PIXELS}"
echo "download mode: http (HF_HUB_DISABLE_XET=$HF_HUB_DISABLE_XET)"
echo "start: $(date -Is)" | tee -a "$LOG_FILE"

set +e
PYTHONUNBUFFERED=1 "$PYTHON_BIN" "$ROOT_DIR/python/embedding_service.py" 2>&1 | tee -a "$LOG_FILE"
status=${PIPESTATUS[0]}
set -e
echo "exit=$status: $(date -Is)" | tee -a "$LOG_FILE"
exit "$status"
