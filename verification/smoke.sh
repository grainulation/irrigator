#!/bin/sh
set -eu
binary=${1:-./irrigator}
case "$binary" in /*) ;; *) binary="$(pwd)/$binary" ;; esac
workspace=${2:-$(mktemp -d "${TMPDIR:-/tmp}/irrigator-smoke.XXXXXX")}
mkdir -p "$workspace"
if [ -e "$workspace/run" ]; then
  echo 'Choose a fresh output directory; existing runs are preserved.' >&2
  exit 1
fi
"$binary" init --out "$workspace/plan.json"
"$binary" plan "$workspace/plan.json" > "$workspace/plan.txt"
"$binary" prepare "$workspace/plan.json" --run "$workspace/run" --backend mock
"$binary" resume "$workspace/run" --workers 2
"$binary" status "$workspace/run" --json > "$workspace/status.json"
"$binary" export "$workspace/run" --dir "$workspace/artifacts"
test -s "$workspace/artifacts/brief.txt"
test -s "$workspace/artifacts/review.txt"
"$binary" graph "$workspace/run" > "$workspace/graph.mmd"
"$binary" doctor > "$workspace/doctor.json"
printf 'First-use smoke passed. Files and durable run: %s\n' "$workspace"
