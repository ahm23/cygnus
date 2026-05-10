#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  cat >&2 <<'EOF'
Usage:
  scripts/compare-stress-metrics.sh BEFORE.json AFTER.json
EOF
  exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required to compare stress metrics" >&2
  exit 1
fi

before="$1"
after="$2"

jq -r -n --slurpfile before_file "$before" --slurpfile after_file "$after" '
  ($before_file[0]) as $before |
  ($after_file[0]) as $after |

  def pct($before; $after):
    if ($before == 0) then "n/a"
    else (((($after - $before) / $before) * 100) | tostring) + "%"
    end;

  def row($name; $path):
    ($before | getpath($path)) as $b |
    ($after | getpath($path)) as $a |
    "| \($name) | \($b) | \($a) | \(pct($b; $a)) |";

  [
    "# Cygnus Stress Comparison",
    "",
    "| Metric | Before | After | Change |",
    "| --- | ---: | ---: | ---: |",
    row("total duration ms"; ["duration_ms"]),
    row("create files ms"; ["phases", "create_files", "duration_ms"]),
    row("post files ms"; ["phases", "post_files", "duration_ms"]),
    row("upload files ms"; ["phases", "upload_files", "duration_ms"]),
    row("uploads/sec"; ["upload", "uploads_per_second"]),
    row("first completion ms"; ["upload", "first_completion_ms"]),
    row("upload p50 ms"; ["upload", "latency_ms", "p50"]),
    row("upload p95 ms"; ["upload", "latency_ms", "p95"]),
    row("upload p99 ms"; ["upload", "latency_ms", "p99"]),
    row("upload max ms"; ["upload", "latency_ms", "max"]),
    row("failed uploads"; ["upload", "failed"])
  ] | .[]
'
