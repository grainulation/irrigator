#!/bin/sh
set -eu
binary=${1:-./irrigator}
case "$binary" in /*) ;; *) binary="$(pwd)/$binary" ;; esac
workspace=${2:-$(mktemp -d "${TMPDIR:-/tmp}/irrigator-smoke.XXXXXX")}
mkdir -p "$workspace"
workspace=$(cd "$workspace" && pwd -P)
if [ -e "$workspace/run" ]; then
  echo 'Choose a fresh output directory; existing runs are preserved.' >&2
  exit 1
fi
# Keep local first-use verification independent of caller routing and the
# current-session registry; use a private HOME for local configuration.
smoke_home="$workspace/home"
mkdir -p "$smoke_home"
run_cli() {
  env HOME="$smoke_home" IRRIGATOR_SESSION_REGISTRY="$workspace/session.json" \
    IRRIGATOR_EXECUTOR=local IRRIGATOR_UI=plain IRRIGATOR_LAUNCH_CONTEXT= \
    "$binary" "$@"
}
run_cli init --out "$workspace/plan.json"
run_cli plan "$workspace/plan.json" > "$workspace/plan.txt"
run_cli prepare "$workspace/plan.json" --run "$workspace/run" --backend mock
run_cli resume "$workspace/run" --workers 2
run_cli status "$workspace/run" --json > "$workspace/status.json"
run_cli export "$workspace/run" --dir "$workspace/artifacts"
test -s "$workspace/artifacts/brief.txt"
test -s "$workspace/artifacts/review.txt"
run_cli graph "$workspace/run" > "$workspace/graph.mmd"
run_cli doctor > "$workspace/doctor.json"
printf 'First-use smoke passed. Files and durable run: %s\n' "$workspace"
