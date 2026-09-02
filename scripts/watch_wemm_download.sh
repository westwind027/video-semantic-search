#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_PYTHON="/home/zephyr/go/src/hunyuan3d/.venv/bin/python"
PYTHON_BIN="${PYTHON_BIN:-$DEFAULT_PYTHON}"
LOG_FILE="${WEMM_DOWNLOAD_LOG:-$ROOT_DIR/logs/wemm-download.log}"
INTERVAL_SECONDS="${WEMM_WATCH_INTERVAL:-10}"
TOTAL_BYTES="${WEMM_MODEL_BYTES:-5441695216}"
ONCE=0

usage() {
  cat <<'EOF'
Usage: scripts/watch_wemm_download.sh [options]

Options:
  --log PATH       download log (default: logs/wemm-download.log)
  --interval SEC   polling interval (default: 10)
  --once           print one sample and exit
  -h, --help       show this help

The downloader should be started with scripts/download_wemm.sh so that the
HTTP/Xet progress output is available in the log. The watcher also reports the
embedding process and its established TCP connections to the proxy.
EOF
}

while (($# > 0)); do
  case "$1" in
    --log)
      [[ $# -ge 2 ]] || { echo "--log requires a path" >&2; exit 2; }
      LOG_FILE="$2"
      shift 2
      ;;
    --interval)
      [[ $# -ge 2 ]] || { echo "--interval requires seconds" >&2; exit 2; }
      INTERVAL_SECONDS="$2"
      shift 2
      ;;
    --once)
      ONCE=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if ! [[ "$INTERVAL_SECONDS" =~ ^[1-9][0-9]*$ ]]; then
  echo "interval must be a positive integer" >&2
  exit 2
fi

if ! [[ "$TOTAL_BYTES" =~ ^[1-9][0-9]*$ ]]; then
  echo "WEMM_MODEL_BYTES must be a positive integer" >&2
  exit 2
fi

if [[ ! -x "$PYTHON_BIN" ]]; then
  PYTHON_BIN="python3"
fi

human_bytes() {
  awk -v value="${1:-0}" 'BEGIN {
    split("B KB MB GB TB", units, " ");
    unit_index = 1;
    while (value >= 1024 && unit_index < 5) { value /= 1024; unit_index++ }
    if (unit_index == 1) printf "%.0f%s", value, units[unit_index];
    else printf "%.2f%s", value, units[unit_index];
  }'
}

human_duration() {
  awk -v seconds="${1:-0}" 'BEGIN {
    if (seconds < 0) { print "--"; exit }
    seconds = int(seconds + 0.5);
    days = int(seconds / 86400); seconds %= 86400;
    hours = int(seconds / 3600); seconds %= 3600;
    minutes = int(seconds / 60); seconds %= 60;
    if (days > 0) printf "%dd %02dh %02dm", days, hours, minutes;
    else if (hours > 0) printf "%02dh %02dm %02ds", hours, minutes, seconds;
    else printf "%02dm %02ds", minutes, seconds;
  }'
}

parse_log() {
  if [[ ! -f "$LOG_FILE" ]]; then
    printf 'none\t0\t0\n'
    return
  fi

  "$PYTHON_BIN" - "$LOG_FILE" "$TOTAL_BYTES" <<'PY'
import re
import sys
from pathlib import Path

log_path = Path(sys.argv[1])
try:
    text = log_path.read_bytes()[-8 * 1024 * 1024:].decode("utf-8", "replace")
except OSError:
    print("none\t0\t0")
    raise SystemExit

text = re.sub(r"\x1b\[[0-9;?]*[ -/]*[@-~]", "", text)
lines = re.split(r"[\r\n]+", text)
number = r"([0-9]+(?:[.,][0-9]+)?)\s*([kKmMgGtT](?:i?[bB])?|[bB])"
binary_factor = {"B": 1, "K": 1024, "M": 1024**2, "G": 1024**3, "T": 1024**4}
decimal_factor = {"B": 1, "K": 1000, "M": 1000**2, "G": 1000**3, "T": 1000**4}

def to_bytes(value, unit, binary):
    normalized = unit.upper()
    if normalized == "B":
        prefix = "B"
    else:
        prefix = normalized[0]
    if normalized.endswith("IB"):
        binary = True
    factors = binary_factor if binary else decimal_factor
    return float(value.replace(",", "")) * factors[prefix]

for line in reversed(lines):
    if "embedding service listening on" in line:
        print(f"ready\t{int(sys.argv[2])}\t0")
        raise SystemExit
    if "model.safetensors:" not in line:
        continue
    phase = "reconstructing" if "reconstructing file:" in line else "downloading"
    binary_units = "reconstructing file:" in line or "downloading bytes:" in line
    current = re.search(r"\|\s*" + number, line)
    if not current:
        continue
    progress = to_bytes(current.group(1), current.group(2), binary_units)
    rates = re.findall(number + r"/s", line)
    rate = to_bytes(rates[-1][0], rates[-1][1], binary_units) if rates else 0
    print(f"{phase}\t{int(progress)}\t{int(rate)}")
    raise SystemExit

print("none\t0\t0")
PY
}

find_embedding_pid() {
  ps -eo pid=,comm=,args= | awk '$2 ~ /^python([0-9.]*)?$/ && $0 ~ /embedding_service\.py/ {print $1; exit}'
}

tcp_stats() {
  local pid="$1"
  if [[ -z "$pid" ]]; then
    printf '0\t0\n'
    return
  fi

  ss -tinp 2>/dev/null | awk -v wanted="pid=${pid}," '
    /ESTAB/ {
      matched = index($0, wanted) > 0
      if (matched) connections++
      next
    }
    matched && /bytes_received:/ {
      if (match($0, /bytes_received:[0-9]+/))
        received += substr($0, RSTART + 15, RLENGTH - 15)
      matched = 0
    }
    END { printf "%d\t%d\n", connections + 0, received + 0 }
  '
}

printf 'watching %s (every %ss)\n' "$LOG_FILE" "$INTERVAL_SECONDS"
last_time=""
last_received=""
last_progress=""

while :; do
  now="$(date +%s)"
  IFS=$'\t' read -r phase progress reported_rate <<<"$(parse_log)"
  pid="$(find_embedding_pid || true)"
  IFS=$'\t' read -r connections received <<<"$(tcp_stats "$pid")"

  percent="$(awk -v current="$progress" -v total="$TOTAL_BYTES" 'BEGIN { printf "%.2f", current * 100 / total }')"
  remaining="$(awk -v current="$progress" -v total="$TOTAL_BYTES" -v speed="$reported_rate" 'BEGIN {
    if (speed > 0) printf "%.0f", (total - current) / speed;
    else print "-1"
  }')"

  network_rate=0
  if [[ -n "$last_time" && "$now" -gt "$last_time" && -n "$last_received" ]]; then
    network_rate=$(( (received - last_received) / (now - last_time) ))
    ((network_rate < 0)) && network_rate=0
  fi
  if [[ "$network_rate" -eq 0 && -n "$last_time" && "$now" -gt "$last_time" && -n "$last_progress" ]]; then
    progress_delta=$((progress - last_progress))
    if ((progress_delta > 0)); then
      network_rate=$((progress_delta / (now - last_time)))
    fi
  fi

  if [[ -n "$pid" ]]; then
    state="running pid=$pid"
  else
    state="not-running"
  fi

  if [[ "$phase" == "none" ]]; then
    progress_text="unavailable"
    speed_text="network $(human_bytes "$network_rate")/s"
    eta_text="--"
  elif [[ "$phase" == "ready" ]]; then
    progress_text="$(human_bytes "$progress") / $(human_bytes "$TOTAL_BYTES") (100.00%, ready)"
    speed_text="download complete"
    eta_text="00m 00s"
  else
    progress_text="$(human_bytes "$progress") / $(human_bytes "$TOTAL_BYTES") (${percent}%, ${phase})"
    speed_text="progress $(human_bytes "$reported_rate")/s; network $(human_bytes "$network_rate")/s"
    eta_text="$(human_duration "$remaining")"
  fi

  printf '%s | %s | progress=%s | %s | eta=%s | proxy_connections=%s\n' \
    "$(date '+%F %T')" "$state" "$progress_text" "$speed_text" "$eta_text" "$connections"

  last_time="$now"
  last_received="$received"
  last_progress="$progress"
  ((ONCE == 1)) && break
  sleep "$INTERVAL_SECONDS"
done
