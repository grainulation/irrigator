#!/bin/sh
# Replay gate (experimental): a change is accepted only if every fixture in BOTH
# dev/ and heldout/ replays through the real executor path to its recorded
# terminal states, event order, cost flags and recovery decision, and every
# committed fixture passes the redaction scan and the coverage pins.
# Usage: tools/replay-gate.sh [FIXTURE_DIR]   (default internal/debug/testdata/replay,
# which must contain dev/ and heldout/ subdirectories)
set -eu
cd "$(dirname "$0")/.."
dir=${1:-$PWD/internal/debug/testdata/replay}
[ -d "$dir/dev" ] && [ -d "$dir/heldout" ] || { echo "replay-gate: $dir needs dev/ and heldout/" >&2; exit 2; }
out=$(IRRIGATOR_REPLAY_DIR=$dir go test -tags integration ./internal/cli ./internal/debug -count=1 -v -run '^(TestReplayGate|TestReplayCommittedFixturesRedacted|TestReplayCommittedFixtureCoverage|TestReplayCommittedFixtureSetsArePinned)$' 2>&1) && rc=0 || rc=$?
echo "$out" | grep -E '^(\s+)?(--- FAIL|REPLAY-GATE|.*replay_[a-z_]*test.go)' || true
if [ "$rc" -ne 0 ]; then echo "replay-gate: FAIL (dev and heldout must both pass)" >&2; exit 1; fi
# A renamed or filtered-out gate test makes `go test` exit 0 with "no tests to run";
# the gate only passes if it actually reported results for both sets.
echo "$out" | grep -q 'REPLAY-GATE dev=' || { echo "replay-gate: FAIL (the gate test did not run)" >&2; exit 1; }
echo "replay-gate: PASS"
