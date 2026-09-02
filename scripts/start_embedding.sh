#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_PYTHON="/home/zephyr/go/src/hunyuan3d/.venv/bin/python"
PYTHON_BIN="${PYTHON_BIN:-$DEFAULT_PYTHON}"
LOG_FILE="${EMBEDDING_LOG:-$ROOT_DIR/logs/embedding-service.log}"
PROXY_URL="${PROXY_URL:-http://192.168.50.199:10810}"

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  cat <<'EOF'
Usage: scripts/start_embedding.sh

Starts the Python embedding HTTP service using the shared Hunyuan3D Python
environment. The model is loaded from the Hugging Face cache and downloaded
through the configured proxy only when it is missing.

Environment overrides:
  PYTHON_BIN             Python executable
  EMBEDDING_BACKEND      wemm (default) or hash
  WEMM_MODEL             tencent/WeMM-Embedding-2B (default)
  EMBEDDING_HOST         127.0.0.1 (default)
  EMBEDDING_PORT         7001 (default)
  EMBEDDING_DIMENSION    2048 (default; supported WeMM Matryoshka dimension)
  EMBEDDING_BATCH_SIZE   8 (default; lower to 4 or 1 if GPU memory is insufficient)
  WEMM_MAX_IMAGE_PIXELS  98304 (default; set 0 to use the model default)
  WEMM_MIN_IMAGE_PIXELS  65536 (default; set 0 to use the model default)
  WEMM_IMAGE_PROMPT      Represent this image. (default)
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

if pgrep -af '[p]ython.*embedding_service\.py' >/dev/null 2>&1; then
  echo "embedding_service.py is already running; refuse to start a duplicate." >&2
  exit 1
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
export EMBEDDING_DEVICE="${EMBEDDING_DEVICE:-cuda}"
export EMBEDDING_BATCH_SIZE="${EMBEDDING_BATCH_SIZE:-8}"
export WEMM_MAX_IMAGE_PIXELS="${WEMM_MAX_IMAGE_PIXELS:-98304}"
export WEMM_MIN_IMAGE_PIXELS="${WEMM_MIN_IMAGE_PIXELS:-65536}"

echo "embedding log: $LOG_FILE"
echo "python: $PYTHON_BIN"
echo "backend: $EMBEDDING_BACKEND"
echo "model: $WEMM_MODEL"
echo "endpoint: http://${EMBEDDING_HOST:-127.0.0.1}:${EMBEDDING_PORT:-7001}"
echo "inference: batch=$EMBEDDING_BATCH_SIZE image_pixels=${WEMM_MIN_IMAGE_PIXELS}-${WEMM_MAX_IMAGE_PIXELS}"
echo "download mode: http (HF_HUB_DISABLE_XET=$HF_HUB_DISABLE_XET)"
echo "start: $(date -Is)" | tee -a "$LOG_FILE"

set +e
PYTHONUNBUFFERED=1 "$PYTHON_BIN" "$ROOT_DIR/python/embedding_service.py" 2>&1 | tee -a "$LOG_FILE"
status=${PIPESTATUS[0]}
set -e
echo "exit=$status: $(date -Is)" | tee -a "$LOG_FILE"
exit "$status"
