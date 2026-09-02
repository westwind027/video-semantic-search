#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_PYTHON="/home/zephyr/go/src/hunyuan3d/.venv/bin/python"
PYTHON_BIN="${PYTHON_BIN:-$DEFAULT_PYTHON}"
LOG_FILE="${WEMM_DOWNLOAD_LOG:-$ROOT_DIR/logs/wemm-download.log}"
PROXY_URL="${PROXY_URL:-http://192.168.50.199:10810}"
MAX_ATTEMPTS="${WEMM_MAX_ATTEMPTS:-3}"
RETRY_DELAY_SECONDS="${WEMM_RETRY_DELAY_SECONDS:-5}"
DOWNLOAD_MODE="${WEMM_DOWNLOAD_MODE:-http}"

case "$DOWNLOAD_MODE" in
  http|xet)
    ;;
  *)
    echo "unsupported WEMM_DOWNLOAD_MODE=$DOWNLOAD_MODE (expected http or xet)" >&2
    exit 1
    ;;
esac

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  cat <<'EOF'
Usage: scripts/download_wemm.sh

Starts the WEMM embedding service with the configured proxy. HTTP Range download
is the default; set WEMM_DOWNLOAD_MODE=xet to use the Xet client explicitly.
Progress and errors are appended to logs/wemm-download.log.
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

export HTTP_PROXY="$PROXY_URL"
export HTTPS_PROXY="$PROXY_URL"
export ALL_PROXY="$PROXY_URL"
export HF_HUB_ENABLE_HF_TRANSFER=0
if [[ "$DOWNLOAD_MODE" == "http" ]]; then
  # Force huggingface_hub's regular HTTP downloader. It writes a .incomplete
  # file and resumes it with a Range request after an interrupted run.
  export HF_HUB_DISABLE_XET=1
  export HF_HUB_DOWNLOAD_TIMEOUT="${HF_HUB_DOWNLOAD_TIMEOUT:-60}"
else
  export HF_HUB_DISABLE_XET=0
  export HF_XET_HIGH_PERFORMANCE="${HF_XET_HIGH_PERFORMANCE:-0}"
  export HF_XET_FIXED_DOWNLOAD_CONCURRENCY="${HF_XET_FIXED_DOWNLOAD_CONCURRENCY:-8}"
  export HF_XET_NUM_CONCURRENT_RANGE_GETS="${HF_XET_NUM_CONCURRENT_RANGE_GETS:-8}"
  export HF_XET_CHUNK_CACHE_SIZE_BYTES="${HF_XET_CHUNK_CACHE_SIZE_BYTES:-10000000000}"
  export HF_XET_CLIENT_RETRY_MAX_ATTEMPTS="${HF_XET_CLIENT_RETRY_MAX_ATTEMPTS:-3}"
  export HF_XET_CLIENT_RETRY_BASE_DELAY="${HF_XET_CLIENT_RETRY_BASE_DELAY:-1000ms}"
  export HF_XET_CLIENT_RETRY_MAX_DURATION="${HF_XET_CLIENT_RETRY_MAX_DURATION:-60s}"
  export HF_XET_CLIENT_CONNECT_TIMEOUT="${HF_XET_CLIENT_CONNECT_TIMEOUT:-30s}"
  export HF_XET_CLIENT_READ_TIMEOUT="${HF_XET_CLIENT_READ_TIMEOUT:-60s}"
fi
export EMBEDDING_BACKEND="${EMBEDDING_BACKEND:-wemm}"
export WEMM_MODEL="${WEMM_MODEL:-tencent/WeMM-Embedding-2B}"
export EMBEDDING_DIMENSION="${EMBEDDING_DIMENSION:-2048}"
export WEMM_IMAGE_PROMPT="${WEMM_IMAGE_PROMPT:-Represent this image.}"
export EMBEDDING_DEVICE="${EMBEDDING_DEVICE:-cuda}"
export EMBEDDING_BATCH_SIZE="${EMBEDDING_BATCH_SIZE:-8}"
export WEMM_MAX_IMAGE_PIXELS="${WEMM_MAX_IMAGE_PIXELS:-98304}"
export WEMM_MIN_IMAGE_PIXELS="${WEMM_MIN_IMAGE_PIXELS:-65536}"

echo "download log: $LOG_FILE"
echo "proxy: $PROXY_URL"
echo "download mode: $DOWNLOAD_MODE"
echo "inference: batch=$EMBEDDING_BATCH_SIZE image_pixels=${WEMM_MIN_IMAGE_PIXELS}-${WEMM_MAX_IMAGE_PIXELS}"
if [[ "$DOWNLOAD_MODE" == "http" ]]; then
  echo "HTTP download timeout: ${HF_HUB_DOWNLOAD_TIMEOUT}s"
else
  echo "Xet high performance: $HF_XET_HIGH_PERFORMANCE"
  echo "Xet fixed download concurrency: $HF_XET_FIXED_DOWNLOAD_CONCURRENCY"
  echo "Xet concurrent range gets: $HF_XET_NUM_CONCURRENT_RANGE_GETS"
  echo "Xet retry: attempts=$HF_XET_CLIENT_RETRY_MAX_ATTEMPTS max_duration=$HF_XET_CLIENT_RETRY_MAX_DURATION read_timeout=$HF_XET_CLIENT_READ_TIMEOUT"
fi
echo "start: $(date -Is)" | tee -a "$LOG_FILE"

status=1
for attempt in $(seq 1 "$MAX_ATTEMPTS"); do
  echo "attempt=$attempt/$MAX_ATTEMPTS: $(date -Is)" | tee -a "$LOG_FILE"
  set +e
  if command -v script >/dev/null 2>&1; then
    command script -qefc "$(printf '%q ' "$PYTHON_BIN" "$ROOT_DIR/python/embedding_service.py")" /dev/null 2>&1 | tee -a "$LOG_FILE"
    status=${PIPESTATUS[0]}
  else
    PYTHONUNBUFFERED=1 "$PYTHON_BIN" "$ROOT_DIR/python/embedding_service.py" 2>&1 | tee -a "$LOG_FILE"
    status=${PIPESTATUS[0]}
  fi
  set -e

  if [[ "$status" -eq 0 || "$status" -eq 130 || "$attempt" -eq "$MAX_ATTEMPTS" ]]; then
    break
  fi
  echo "attempt=$attempt failed with exit=$status; retrying in ${RETRY_DELAY_SECONDS}s" | tee -a "$LOG_FILE"
  sleep "$RETRY_DELAY_SECONDS"
done

echo "exit=$status: $(date -Is)" | tee -a "$LOG_FILE"
exit "$status"
