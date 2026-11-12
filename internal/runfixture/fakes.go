package runfixture

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
)

// FakeHc installs a fake `hc` first on PATH. Every invocation is appended to the
// returned log. `hc ls --json` prints the file named by FAKE_HC_LS (default
// "[]"); `hc run` executes FAKE_HC_RUN, a shell snippet that sees the program
// argv (after `--`) as "$@". Never talks to a real herdr-core.
func FakeHc(t *testing.T, version string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "hc.log")
	script := `#!/bin/sh
echo "$@" >> "$FAKE_HC_LOG"
case "$1" in
--version) echo "hc $FAKE_HC_VERSION" ;;
ls) if [ -n "$FAKE_HC_LS" ]; then cat "$FAKE_HC_LS"; else echo '[]'; fi ;;
kill) exit 0 ;;
run)
  while [ "$1" != "--" ]; do shift; done
  shift
  eval "$FAKE_HC_RUN"
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "hc"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_HC_LOG", logPath)
	t.Setenv("FAKE_HC_VERSION", version)
	t.Setenv("FAKE_HC_LS", "")
	t.Setenv("FAKE_HC_RUN", `exec "$@"`)
	return logPath
}

// FakeTmux installs a fake `tmux` first on PATH. Every invocation is appended
// to the returned log. new-session/new-window run the window command through
// `sh -c` in the background (as tmux does with its default shell), or
// FAKE_TMUX_RUN when set; list-panes reports the process as the pane, and a
// finished pane stays listed as dead (remain-on-exit). Never talks to a real
// tmux server.
func FakeTmux(t *testing.T, version string) (logPath string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping tmux subprocess test in short mode")
	}
	dir := t.TempDir()
	logPath = filepath.Join(dir, "tmux.log")
	script := `#!/bin/sh
D="$FAKE_TMUX_DIR"
echo "$*" >> "$FAKE_TMUX_LOG"
case "$1" in
-V) echo "tmux $FAKE_TMUX_VERSION" ;;
has-session) [ -f "$D/session" ] ;;
set-option) exit 0 ;;
new-session|new-window)
  [ "$1" = new-session ] && touch "$D/session"
  n=$(( $(cat "$D/n" 2>/dev/null || echo 0) + 1 )); echo $n > "$D/n"
  name=""; cwd="."; prev=""
  for a; do
    case "$prev" in -n) name="$a";; -c) cwd="$a";; esac
    prev="$a"; cmd="$a"
  done
  run="${FAKE_TMUX_RUN:-$cmd}"
  (cd "$cwd" && exec sh -c "$run") </dev/null >/dev/null 2>&1 &
  echo $! > "$D/pid.$n"
  echo "$name" > "$D/name.$n"
  echo "@$n" ;;
list-panes)
  case "$*" in
  *window_name*)
    for f in "$D"/name.*; do
      [ -f "$f" ] || continue
      n=${f##*.}; pid=$(cat "$D/pid.$n")
      if kill -0 "$pid" 2>/dev/null; then d=0; else d=1; fi
      echo "$(cat "$f") $d"
    done ;;
  *)
    prev=""; t=""
    for a; do [ "$prev" = -t ] && t="$a"; prev="$a"; done
    n=${t#@}
    [ -f "$D/pid.$n" ] || { echo "can't find window: $t" >&2; exit 1; }
    pid=$(cat "$D/pid.$n")
    if kill -0 "$pid" 2>/dev/null; then echo "$pid 0"; else echo "$pid 1"; fi ;;
  esac ;;
kill-window)
  prev=""; t=""
  for a; do [ "$prev" = -t ] && t="$a"; prev="$a"; done
  n=${t#@}
  [ -f "$D/pid.$n" ] || { echo "can't find window: $t" >&2; exit 1; }
  kill "$(cat "$D/pid.$n")" 2>/dev/null
  rm -f "$D/pid.$n" "$D/name.$n" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_TMUX_DIR", dir)
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("FAKE_TMUX_VERSION", version)
	t.Setenv("FAKE_TMUX_RUN", "")
	return logPath
}

func TmuxLog(t *testing.T, logPath string) string {
	t.Helper()
	raw, _ := os.ReadFile(logPath)
	return string(raw)
}

const outcomeScript = `#!/bin/sh
id=$(sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
if [ -n "$RECOVERY_RAW" ]; then printf '%s' "$RECOVERY_RAW"; exit 0; fi
printf '{"protocol":"swarm.worker/v1","request_id":"%s","cost_usd":0%s%s}' "$id" "$RECOVERY_EXTRA" "$VALIDATOR_EXTRA"
`

func WriteOutcomeScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider.sh")
	if err := os.WriteFile(path, []byte(outcomeScript), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func SetOutcomeEnv(t *testing.T, extra, raw, validator string) {
	t.Setenv("RECOVERY_EXTRA", extra)
	t.Setenv("RECOVERY_RAW", raw)
	t.Setenv("VALIDATOR_EXTRA", validator)
}

func OutcomeLeaseContext(t *testing.T, cell string, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	lease, err := checkpoint.FileLock(filepath.Join(cell, "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { checkpoint.Unlock(lease) })
	return context.WithTimeout(context.WithValue(context.Background(), launch.ProcessLeaseKey{}, lease), d)
}

const ValidExitScript = `#!/bin/sh
id=$(sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
printf '{"protocol":"swarm.worker/v1","request_id":"%s","error":"known","cost_usd":0}' "$id"
exit 1
`

func WriteScriptBody(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider.sh")
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
