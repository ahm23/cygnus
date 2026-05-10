#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  scripts/stress-report.sh [options] -- <cygnus stress-test command...>

Options:
  --label NAME       Label for the output directory. Default: run
  --out-dir DIR      Directory for reports. Default: reports/stress
  --pid PID          Provider cygnus PID for pidstat. Default: first pidof cygnus
  --interval SECS    Monitor sampling interval. Default: 1

Example:
  scripts/stress-report.sh --label fsync-off --pid $(pidof cygnus | awk '{print $1}') -- \
    ./cygnus stress-test --files 10000 --size 1MB --upload-batch-size 100
EOF
}

label="run"
out_root="reports/stress"
provider_pid=""
interval="1"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --label)
      label="${2:?missing value for --label}"
      shift 2
      ;;
    --out-dir)
      out_root="${2:?missing value for --out-dir}"
      shift 2
      ;;
    --pid)
      provider_pid="${2:?missing value for --pid}"
      shift 2
      ;;
    --interval)
      interval="${2:?missing value for --interval}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      break
      ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ $# -eq 0 ]]; then
  echo "missing stress-test command" >&2
  usage >&2
  exit 2
fi

safe_label="$(printf '%s' "$label" | tr -cs 'A-Za-z0-9._-' '-')"
run_id="$(date -u +%Y%m%dT%H%M%SZ)"
run_dir="${out_root}/${run_id}-${safe_label}"
metrics_file="${run_dir}/stress-metrics.json"
stress_log="${run_dir}/stress.log"
report_file="${run_dir}/report.md"
mkdir -p "$run_dir"

if [[ -z "$provider_pid" ]] && command -v pidof >/dev/null 2>&1; then
  provider_pid="$(pidof cygnus 2>/dev/null | awk '{print $1}' || true)"
fi

monitor_pids=()
cleanup() {
  for pid in "${monitor_pids[@]:-}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

start_monitor() {
  local name="$1"
  shift
  if "$@" >"${run_dir}/${name}.log" 2>&1 & then
    monitor_pids+=("$!")
  fi
}

{
  echo "label=${label}"
  echo "run_id=${run_id}"
  echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "provider_pid=${provider_pid:-unknown}"
  echo "command=$* --metrics-file ${metrics_file}"
  echo
  uname -a || true
  echo
  command -v lscpu >/dev/null 2>&1 && lscpu || true
  echo
  command -v free >/dev/null 2>&1 && free -h || true
  echo
  df -h . || true
} >"${run_dir}/system.txt"

if command -v iostat >/dev/null 2>&1; then
  start_monitor iostat iostat -xz "$interval"
else
  echo "iostat not found; install sysstat for disk metrics" >"${run_dir}/iostat.log"
fi

if [[ -n "$provider_pid" ]] && command -v pidstat >/dev/null 2>&1; then
  start_monitor pidstat pidstat -p "$provider_pid" -druw "$interval"
else
  echo "pidstat unavailable or provider PID not found; install sysstat and pass --pid" >"${run_dir}/pidstat.log"
fi

if command -v vmstat >/dev/null 2>&1; then
  start_monitor vmstat vmstat "$interval"
fi

echo "Writing report data to ${run_dir}"
set +e
"$@" --metrics-file "$metrics_file" 2>&1 | tee "$stress_log"
stress_status=${PIPESTATUS[0]}
set -e

cleanup
trap - EXIT

{
  echo "# Cygnus Stress Report"
  echo
  echo "- Label: \`${label}\`"
  echo "- Run directory: \`${run_dir}\`"
  echo "- Stress exit code: \`${stress_status}\`"
  echo "- Provider PID: \`${provider_pid:-unknown}\`"
  echo
  if [[ -f "$metrics_file" ]] && command -v jq >/dev/null 2>&1; then
    echo "## Summary"
    echo
    echo "| Metric | Value |"
    echo "| --- | ---: |"
    jq -r '
      def ms: tostring + " ms";
      [
        ["success", (.success|tostring)],
        ["files", (.file_count|tostring)],
        ["file size", (.file_size_bytes|tostring + " bytes")],
        ["total duration", (.duration_ms|ms)],
        ["create files", (.phases.create_files.duration_ms|ms)],
        ["post files", (.phases.post_files.duration_ms|ms)],
        ["upload files", (.phases.upload_files.duration_ms|ms)],
        ["uploaded", ((.upload.uploaded // 0)|tostring)],
        ["failed", ((.upload.failed // 0)|tostring)],
        ["uploads/sec", ((.upload.uploads_per_second // 0)|tostring)],
        ["first completion", ((.upload.first_completion_ms // 0)|ms)],
        ["upload p50", ((.upload.latency_ms.p50 // 0)|ms)],
        ["upload p95", ((.upload.latency_ms.p95 // 0)|ms)],
        ["upload p99", ((.upload.latency_ms.p99 // 0)|ms)],
        ["upload max", ((.upload.latency_ms.max // 0)|ms)]
      ] | .[] | "| \(.[0]) | \(.[1]) |"
    ' "$metrics_file"
    echo
  else
    echo "Install jq to render metric summaries in this report."
    echo
  fi
  echo "## Files"
  echo
  echo "- Stress metrics: \`stress-metrics.json\`"
  echo "- Stress log: \`stress.log\`"
  echo "- Disk stats: \`iostat.log\`"
  echo "- Process stats: \`pidstat.log\`"
  echo "- System info: \`system.txt\`"
} >"$report_file"

echo "Report written to ${report_file}"
exit "$stress_status"
