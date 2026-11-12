#!/bin/sh
set -eu
binary=${1:-./irrigator}
case "$binary" in /*) ;; *) binary="$(pwd)/$binary" ;; esac
project=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
workspace=${2:-$(mktemp -d "${TMPDIR:-/tmp}/irrigator-adaptive.XXXXXX")}
mkdir -p "$workspace"
if [ -e "$workspace/run" ]; then
  echo 'Choose a fresh output directory; existing runs are preserved.' >&2
  exit 1
fi
cp "$project/examples/adaptive/plan.json" "$workspace/plan.json"
cp "$project/examples/adaptive/proposal.json" "$workspace/proposal.json"
status=0
"$binary" apply "$workspace/plan.json" --run "$workspace/run" --backend mock || status=$?
if [ "$status" -ne 1 ]; then
  echo 'Expected the deliberately failing acceptance check to block the initial graph.' >&2
  exit 1
fi
"$binary" status "$workspace/run" --json > "$workspace/before.json"
"$binary" graph "$workspace/run" > "$workspace/before.mmd"
"$binary" propose "$workspace/run" "$workspace/proposal.json"
"$binary" resume "$workspace/run" --workers 2
"$binary" status "$workspace/run" --json > "$workspace/after.json"
"$binary" decisions "$workspace/run" > "$workspace/decisions.json"
"$binary" graph "$workspace/run" > "$workspace/after.mmd"
"$binary" export "$workspace/run" --dir "$workspace/artifacts"
test "$(cat "$workspace/artifacts/result.txt")" = 'ACCEPTED corrected implementation'
printf '\nAdaptive demo passed with mock workers; no credentials or model spend.\n'
printf 'Inspect decisions: %s decisions %s/run\n' "$binary" "$workspace"
printf 'Before/after graphs, accepted result, and durable evidence: %s\n' "$workspace"
printf 'This exercises recovery and parallel scheduling; supplied benefit estimates are not a measured model speedup.\n'
