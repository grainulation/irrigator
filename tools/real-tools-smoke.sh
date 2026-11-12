#!/usr/bin/env bash
# Opt-in smoke test against the real tools. Never run by `go test` or CI.
#
#   tools/real-tools-smoke.sh [doctor] [tmux] [herdr] [container]
#
# With no arguments every check runs. Each check prints PASS, SKIP (tool not
# installed) or FAIL. Exit status is 1 if any check failed.
#
# It builds irrigator into a temp directory and keeps all runs under a temp run
# root. The tmux check uses a private tmux server (its own TMUX_TMPDIR), never
# your default server. The herdr check talks to the real herdr-core through hc:
# it creates terminals labelled for its own run and then cleans them up.
set -u

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d /tmp/irr-smoke.XXXXXX)
bin=$tmp/irrigator
runs=$tmp/runs
mkdir -p "$runs"
fails=0
wanted=" $* "

cleanup_all() {
  if [ -n "${TMUX_TMPDIR:-}" ] && command -v tmux >/dev/null 2>&1; then
    env -u TMUX tmux kill-server >/dev/null 2>&1
  fi
  rm -rf "$tmp"
}
trap cleanup_all EXIT

pass() { printf 'PASS  %s\n' "$1"; }
skip() { printf 'SKIP  %s: %s\n' "$1" "$2"; }
fail() { printf 'FAIL  %s: %s\n' "$1" "$2"; fails=$((fails + 1)); }

selected() { [ "$wanted" = "  " ] || case "$wanted" in *" $1 "*) return 0 ;; *) return 1 ;; esac; }

run_id_of() { sed -n 's/.*"run_id": *"\([^"]*\)".*/\1/p' "$1/state.json" | head -n 1; }

if ! (cd "$root" && go build -o "$bin" .) 2>"$tmp/build.log"; then
  fail build "go build failed: $(tail -n 3 "$tmp/build.log")"
  exit 1
fi
pass build

check_doctor() {
  local out
  if ! out=$("$bin" doctor 2>&1); then
    fail doctor "irrigator doctor exited non-zero: $out"
  elif printf '%s' "$out" | grep -q '"executors"'; then
    pass doctor
  else
    fail doctor "no executors section in doctor output"
  fi
}

# check_executor NAME runs the terminal-swarm example with the mock backend
# under the named executor.
check_executor() {
  local name=$1 run=$runs/$1
  if ! "$bin" apply "$root/examples/terminal-swarm.json" --run "$run" --backend mock --executor "$name" >"$tmp/$name.log" 2>&1; then
    fail "$name executor" "apply failed: $(tail -n 3 "$tmp/$name.log")"
    return 1
  fi
  if [ -z "$(run_id_of "$run")" ]; then
    fail "$name executor" "no run id in $run/state.json"
    return 1
  fi
  if ! "$bin" status "$run" 2>&1 | grep -q 'complete'; then
    fail "$name executor" "run did not complete"
    return 1
  fi
  pass "$name executor: mock run completed"
}

check_tmux() {
  if ! command -v tmux >/dev/null 2>&1; then
    skip tmux "tmux not installed"
    return
  fi
  # The executor uses plain `tmux`, which honours TMUX_TMPDIR for its socket
  # directory: a fresh one gives a private server. TMUX must be unset so an
  # enclosing session does not override it.
  TMUX_TMPDIR=$(mktemp -d /tmp/irr-tmux.XXXXXX)
  export TMUX_TMPDIR
  unset TMUX
  check_executor tmux || return
  local run=$runs/tmux id session out
  id=$(run_id_of "$run")
  session=irrigator-${id:0:8}
  if ! tmux has-session -t "=$session" 2>/dev/null; then
    fail "tmux cleanup" "session $session missing before cleanup"
    return
  fi
  out=$("$bin" cleanup "$run" --dry-run 2>&1) || { fail "tmux cleanup" "dry-run failed: $out"; return; }
  if ! tmux has-session -t "=$session" 2>/dev/null; then
    fail "tmux cleanup" "--dry-run removed the session"
    return
  fi
  out=$("$bin" cleanup "$run" 2>&1) || { fail "tmux cleanup" "cleanup failed: $out"; return; }
  if tmux has-session -t "=$session" 2>/dev/null; then
    fail "tmux cleanup" "session $session still exists after cleanup: $out"
  else
    pass "tmux cleanup: session removed"
  fi
}

check_herdr() {
  if ! command -v hc >/dev/null 2>&1; then
    skip herdr "hc (herdr-core) not installed"
    return
  fi
  check_executor herdr || return
  local run=$runs/herdr out
  out=$("$bin" cleanup "$run" 2>&1) || { fail "herdr cleanup" "cleanup failed: $out"; return; }
  if printf '%s' "$out" | grep -q '"status": "error"'; then
    fail "herdr cleanup" "reported an error: $out"
  else
    pass "herdr cleanup"
  fi
}

check_container() {
  local rt="" c
  for c in container podman docker; do
    if [ "$c" = container ] && [ "$(uname -s)" != Darwin ]; then continue; fi
    if command -v "$c" >/dev/null 2>&1; then
      rt=$c
      break
    fi
  done
  if [ -z "$rt" ]; then
    skip container "no container, podman or docker on PATH"
    return
  fi
  local dir=$tmp/sandbox run=$runs/sandbox
  mkdir -p "$dir"
  cat >"$dir/worker.py" <<'PY'
import json, socket, sys
req = json.load(sys.stdin)
try:
    socket.create_connection(("1.1.1.1", 53), timeout=3).close()
    net = "network-open"
except OSError:
    net = "network-blocked"
print(json.dumps({"protocol": "swarm.worker/v1", "request_id": req["request_id"],
                  "output": {"answer": net}}))
PY
  cat >"$dir/plan.json" <<PLAN
{
  "name": "sandbox-echo",
  "providers": {
    "boxed": {
      "command": ["python3", "/app/worker.py"],
      "sandbox": {"runtime": "$rt", "image": "python:3.13-alpine",
                  "mounts": [{"source": "$dir", "target": "/app"}]}
    }
  },
  "tasks": [{"id": "echo", "provider": "boxed", "prompt": "report network", "requires": ["answer"]}]
}
PLAN
  if ! "$bin" apply "$dir/plan.json" --run "$run" --backend boxed --workers 1 --timeout 300s --call-budget 0.01 >"$tmp/sandbox.log" 2>&1; then
    fail "container sandbox ($rt)" "apply failed: $(tail -n 3 "$tmp/sandbox.log")"
    return
  fi
  if ! "$bin" export "$run" --out "$tmp/sandbox-result.json" >/dev/null 2>&1; then
    fail "container sandbox ($rt)" "export failed"
  elif grep -q 'network-blocked' "$tmp/sandbox-result.json"; then
    pass "container sandbox ($rt): worker ran, network blocked"
  elif grep -q 'network-open' "$tmp/sandbox-result.json"; then
    fail "container sandbox ($rt)" "network was reachable from the sandbox"
  else
    fail "container sandbox ($rt)" "no answer in exported result"
  fi
}

if selected doctor; then check_doctor; fi
if selected tmux; then check_tmux; fi
if selected herdr; then check_herdr; fi
if selected container; then check_container; fi

[ "$fails" -eq 0 ]
