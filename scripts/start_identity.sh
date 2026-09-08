#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_PYTHON="/home/zephyr/go/src/hunyuan3d/.venv/bin/python"
PYTHON_BIN="${PYTHON_BIN:-$DEFAULT_PYTHON}"
IDENTITY_LOG="${IDENTITY_LOG:-$ROOT_DIR/logs/identity-service.log}"

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  cat <<'EOF'
Usage: scripts/start_identity.sh

Starts the standalone InsightFace identity service. It shares the existing
Python environment; it does not install CUDA or Torch packages.

Environment overrides:
  PYTHON_BIN          Python executable
  IDENTITY_BACKEND    insightface (default) or hash (offline API test)
  FACE_MODEL          buffalo_l (default)
  IDENTITY_DEVICE     cuda (default) or cpu
  IDENTITY_HOST       127.0.0.1 (default)
  IDENTITY_PORT       7003 (default)
  IDENTITY_LOG        logs/identity-service.log (default)
EOF
  exit 0
fi

if [[ ! -x "$PYTHON_BIN" ]]; then
  echo "python executable not found: $PYTHON_BIN" >&2
  exit 1
fi
if [[ -f "$ROOT_DIR/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT_DIR/.env"
  set +a
fi

# The shared Python environment installs CUDA runtime wheels (including
# cuDNN) under site-packages instead of the system linker path. Without this
# path ONNX Runtime silently falls back to CPUExecutionProvider even when the
# machine has a working NVIDIA GPU.
if nvidia_root="$("$PYTHON_BIN" -c 'import nvidia; print(next(iter(nvidia.__path__)))' 2>/dev/null)"; then
  identity_cuda_libs="$(find "$nvidia_root" -type d -name lib -print 2>/dev/null | paste -sd: -)"
  if [[ -n "$identity_cuda_libs" ]]; then
    export LD_LIBRARY_PATH="${identity_cuda_libs}${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  fi
fi

if pgrep -af '[p]ython.*identity_service\.py' >/dev/null 2>&1; then
  echo "identity_service.py is already running; nothing left to do."
  exit 0
fi
mkdir -p "$(dirname "$IDENTITY_LOG")"
echo "identity log: $IDENTITY_LOG"
echo "start: $(date -Is)" | tee -a "$IDENTITY_LOG"
PYTHONUNBUFFERED=1 "$PYTHON_BIN" "$ROOT_DIR/python/identity_service.py" 2>&1 | tee -a "$IDENTITY_LOG"
