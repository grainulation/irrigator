package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// tmuxMinVersion is the oldest tmux whose new-session/new-window -P -F, exact
// (=) targets and per-window options the executor relies on.
const tmuxMinVersion = "3.2"

const tmuxInstallURL = "https://github.com/tmux/tmux/wiki/Installing"

const (
	tmuxOK           = "ok"
	tmuxTooOld       = "too_old"
	tmuxNotInstalled = "not_installed"
	tmuxUnknown      = "unknown_version"
)

// tmuxCallTimeout bounds each tmux client call made by the controller.
const tmuxCallTimeout = 10 * time.Second

type tmuxExecutor struct {
	tmux string // resolved tmux path
}

func (tmuxExecutor) name() string { return "tmux" }

// newTmuxExecutor is the preflight: it fails before any worker is launched.
func newTmuxExecutor() (workerExecutor, error) {
	st := tmuxStatus()
	switch st.Status {
	case tmuxNotInstalled:
		return nil, fmt.Errorf("the tmux executor needs tmux (found none on PATH; required >= %s); install it, see %s", tmuxMinVersion, tmuxInstallURL)
	case tmuxTooOld:
		return nil, fmt.Errorf("tmux %s is too old: the tmux executor requires >= %s; upgrade, see %s", st.Version, tmuxMinVersion, tmuxInstallURL)
	case tmuxUnknown:
		return nil, fmt.Errorf("cannot read the version of %s (%s); the tmux executor requires tmux >= %s", st.Path, st.Detail, tmuxMinVersion)
	}
	return tmuxExecutor{tmux: st.Path}, nil
}

type tmuxInfo struct {
	Status  string
	Path    string
	Version string
	Detail  string
}

var tmuxVersionPattern = regexp.MustCompile(`^tmux\s+(?:next-)?(\d+)\.(\d+)`)

// parseTmuxVersion reads `tmux -V` output such as "tmux 3.8", "tmux 3.3a" or
// "tmux next-3.6". Only major.minor matters; a letter suffix is a patch release.
func parseTmuxVersion(out string) (version string, parts [3]int, ok bool) {
	m := tmuxVersionPattern.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return "", parts, false
	}
	parts[0], _ = strconv.Atoi(m[1])
	parts[1], _ = strconv.Atoi(m[2])
	return fmt.Sprintf("%d.%d", parts[0], parts[1]), parts, true
}

// tmuxStatus locates tmux and checks its version against tmuxMinVersion.
func tmuxStatus() tmuxInfo {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return tmuxInfo{Status: tmuxNotInstalled}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-V").Output()
	if err != nil {
		return tmuxInfo{Status: tmuxUnknown, Path: path, Detail: "tmux -V failed: " + err.Error()}
	}
	version, have, ok := parseTmuxVersion(string(out))
	if !ok {
		return tmuxInfo{Status: tmuxUnknown, Path: path, Detail: fmt.Sprintf("unrecognised version output %q", strings.TrimSpace(string(out)))}
	}
	_, min, _ := parseTmuxVersion("tmux " + tmuxMinVersion)
	if !versionAtLeast(have, min) {
		return tmuxInfo{Status: tmuxTooOld, Path: path, Version: version}
	}
	return tmuxInfo{Status: tmuxOK, Path: path, Version: version}
}

// tmuxDoctorEntry is the tmux row of `irrigator doctor`'s executors section.
func tmuxDoctorEntry() map[string]any {
	st := tmuxStatus()
	e := map[string]any{"name": "tmux", "available": st.Status == tmuxOK, "status": st.Status,
		"min_version": tmuxMinVersion, "tmux_path": st.Path, "version": st.Version}
	switch st.Status {
	case tmuxOK:
		e["summary"] = "OK"
	case tmuxTooOld:
		e["summary"] = fmt.Sprintf("too old: found %s, need >= %s; see %s", st.Version, tmuxMinVersion, tmuxInstallURL)
	case tmuxNotInstalled:
		e["summary"] = "not installed (optional); see " + tmuxInstallURL
	default:
		e["summary"] = "version unknown: " + st.Detail
	}
	return e
}

// tmuxSafeName keeps A-Z a-z 0-9 - _ and turns everything else (including the
// '.', ':' and spaces tmux treats specially in targets) into '_'.
func tmuxSafeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// tmuxSessionName is one session per run: irrigator-<first 8 of the run id>.
func TmuxSessionName(runID string) string {
	id := runID
	if len(id) > 8 {
		id = id[:8]
	}
	return "irrigator-" + tmuxSafeName(id)
}

// tmuxWindowName is <cell>-a<attempt>. The attempt keeps a retry from
// colliding with a finished window kept for inspection.
func tmuxWindowName(cell string, attempt int) string {
	slug := tmuxSafeName(cell)
	if slug == "" {
		slug = "cell"
	}
	return slug + "-a" + strconv.Itoa(attempt)
}

// tmuxQuote single-quotes s for a POSIX-compatible shell (and fish).
func tmuxQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tmuxWindowCommand is the shell command tmux runs in the window. tmux hands
// it to the user's default shell, whose syntax we cannot rely on, so it is a
// single `exec /bin/sh -c '<script>'` with every word quoted. The script runs
// the worker under `env` (a tmux server's environment is the server's, not the
// controller's), then writes its exit code to a temp file and renames it so
// the waiter never reads a partial file. Nothing is redirected: the window
// shows the worker's progress. The no-op trap lets the wrapper outlive a
// SIGTERM sent to the pane's process group so it can still record the code.
func tmuxWindowCommand(exitFile string, env []string, binary string, workerArgs []string) string {
	words := []string{"env"}
	words = append(words, env...)
	words = append(words, binary)
	words = append(words, workerArgs...)
	for i, w := range words {
		words[i] = tmuxQuote(w)
	}
	script := "trap : TERM INT HUP\n" +
		strings.Join(words, " ") + "\n" +
		"c=$?\n" +
		"printf '%s\\n' \"$c\" > " + tmuxQuote(exitFile+".tmp") + " && mv -f " + tmuxQuote(exitFile+".tmp") + " " + tmuxQuote(exitFile) + "\n" +
		"exit \"$c\"\n"
	return "exec /bin/sh -c " + tmuxQuote(script)
}

// tmuxWaitScript is what the controller Waits on. It exits with the worker's
// recorded exit code once the exit file appears. If the window or pane is gone
// (or dead) and there is still no exit file, the outcome is unknown: it exits
// non-zero, which the controller treats as "resume to recover", never success.
const tmuxWaitScript = `f=$1; tmux=$2; target=$3; n=0
finish() {
  if [ -f "$f" ]; then
    c=$(cat "$f" 2>/dev/null)
    case "$c" in ''|*[!0-9]*) echo "tmux worker exit file $f is not a number: $c" >&2; exit 70;; esac
    exit "$c"
  fi
}
while :; do
  finish
  n=$((n+1))
  if [ $((n % 5)) -eq 0 ]; then
    out=$("$tmux" list-panes -t "$target" -F '#{pane_pid} #{pane_dead}' 2>/dev/null)
    if [ $? -ne 0 ]; then
      finish
      echo "tmux window $target is gone and left no exit file; the worker outcome is unknown" >&2
      exit 70
    fi
    case "$out" in
    *" 1"*)
      finish
      echo "tmux pane $target ended without an exit file; the worker outcome is unknown" >&2
      exit 70 ;;
    esac
  fi
  sleep 0.2
done
`

func runTmux(tmux string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxCallTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, tmux, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("tmux %s: %v: %s", args[0], err, text)
	}
	return text, nil
}

// tmuxSessionMu serialises "does the session exist? else create it" within
// this controller.
var tmuxSessionMu sync.Mutex

func tmuxSessionExists(tmux, session string) bool {
	_, err := runTmux(tmux, "has-session", "-t", "="+session)
	return err == nil
}

// tmuxLiveWorkers returns the windows of this run's session that still run a
// worker for the cell (any attempt). A dead pane kept by remain-on-exit is not
// live. If the session exists but cannot be listed, that is an error: the
// question is whether a duplicate could start.
func tmuxLiveWorkers(tmux, session, cell string) ([]string, error) {
	if !tmuxSessionExists(tmux, session) {
		return nil, nil
	}
	out, err := runTmux(tmux, "list-panes", "-s", "-t", "="+session, "-F", "#{window_name} #{pane_dead}")
	if err != nil {
		return nil, err
	}
	return liveWindowsFromListing(out, cell), nil
}

func liveWindowsFromListing(listing, cell string) []string {
	slug := tmuxSafeName(cell)
	if slug == "" {
		slug = "cell"
	}
	match := regexp.MustCompile("^" + regexp.QuoteMeta(slug) + `-a\d+$`)
	var live []string
	for _, line := range strings.Split(listing, "\n") {
		name, dead, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && match.MatchString(name) && strings.TrimSpace(dead) != "1" {
			live = append(live, name)
		}
	}
	return live
}

// start launches `binary workerArgs...` for one cell. The caller Waits on cmd
// (the waiter), signals the returned handle, and calls done after Wait returns.
func (t tmuxExecutor) Start(binary, root, runID, cell string, attempt int, workerArgs []string) (cmd *exec.Cmd, child ChildProcess, done func(), err error) {
	session := TmuxSessionName(runID)
	// A worker from an earlier controller may still run in its window. Never
	// start a second one for the same cell.
	live, err := tmuxLiveWorkers(t.tmux, session, cell)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cannot confirm that no tmux worker is already running for cell %s: %w", cell, err)
	}
	if len(live) > 0 {
		return nil, nil, nil, fmt.Errorf("tmux window %s:%s is still running cell %s; not launching a duplicate. Wait for it, or stop it with tmux kill-window -t %s:%s and resume", session, live[0], cell, session, live[0])
	}
	window := tmuxWindowName(cell, attempt)
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, nil, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, nil, err
	}
	dir := filepath.Join(absRoot, "tmux")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, nil, err
	}
	exitFile := filepath.Join(dir, window+".exit")
	// No worker is live, so an exit file here is left over from an earlier
	// launch of the same attempt name and must not be read as this one's.
	if err := os.Remove(exitFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, window+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, nil, err
	}
	command := tmuxWindowCommand(exitFile, HerdrWorkerEnv(os.Environ()), binary, workerArgs)
	target, err := t.launch(session, window, cwd, command, exitFile)
	if err != nil {
		logFile.Close()
		return nil, nil, nil, err
	}
	cmd = exec.Command("/bin/sh", "-c", tmuxWaitScript, "irrigator-tmux-wait", exitFile, t.tmux, target)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Only the waiter's own notes go to the log; the worker's output stays in
	// the window.
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, nil, nil, err
	}
	return cmd, tmuxChild{waiter: cmd.Process, tmux: t.tmux, target: target}, func() { logFile.Close() }, nil
}

// launch creates the window (and the run's session on first use) and returns a
// target for it: the unique window id when tmux reports one.
func (t tmuxExecutor) launch(session, window, cwd, command, exitFile string) (string, error) {
	tmuxSessionMu.Lock()
	defer tmuxSessionMu.Unlock()
	var args []string
	if tmuxSessionExists(t.tmux, session) {
		args = []string{"new-window", "-d", "-t", session + ":", "-n", window}
	} else {
		args = []string{"new-session", "-d", "-s", session, "-n", window}
	}
	args = append(args, "-c", cwd, "-P", "-F", "#{window_id}", command)
	out, err := runTmux(t.tmux, args...)
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(out)
	if !strings.HasPrefix(target, "@") {
		target = "=" + session + ":=" + window
	}
	// Per-window only: never the global options or another session's.
	if _, err := runTmux(t.tmux, "set-option", "-w", "-t", target, "remain-on-exit", "on"); err != nil {
		// A worker that already finished and closed its window is fine: the
		// exit file carries the outcome.
		if _, statErr := os.Stat(exitFile); statErr != nil {
			return "", err
		}
	}
	return target, nil
}

// tmuxChild stops the worker inside tmux. The waiter is not the worker, so
// signalling it would leave the worker running.
type tmuxChild struct {
	waiter *os.Process
	tmux   string
	target string
}

// panePID returns the pane's process id; ok is false when the window or pane
// is gone or dead. A pid of 1 or less is never returned: kill(-1) would
// signal every process we may signal.
func (c tmuxChild) panePID() (pid int, ok bool) {
	out, err := runTmux(c.tmux, "list-panes", "-t", c.target, "-F", "#{pane_pid} #{pane_dead}")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == "0" {
			if p, err := strconv.Atoi(f[0]); err == nil && p > 1 {
				return p, true
			}
		}
	}
	return 0, false
}

func (c tmuxChild) Signal(sig os.Signal) error {
	pid, ok := c.panePID()
	if !ok {
		return os.ErrProcessDone
	}
	s, isSig := sig.(syscall.Signal)
	if !isSig {
		return fmt.Errorf("cannot send %v to a tmux pane", sig)
	}
	// The pane process leads its own process group, which holds the wrapper
	// and the worker; fall back to the pane process alone.
	err := syscall.Kill(-pid, s)
	if errors.Is(err, syscall.ESRCH) {
		err = syscall.Kill(pid, s)
	}
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if s == syscall.SIGTERM || s == syscall.SIGINT || s == syscall.SIGKILL {
		return c.killWindow(err)
	}
	return err
}

func (c tmuxChild) Kill() error {
	err := c.Signal(syscall.SIGKILL)
	if errors.Is(err, os.ErrProcessDone) {
		err = nil
	}
	if kerr := c.waiter.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		err = errors.Join(err, kerr)
	}
	return err
}

func (c tmuxChild) killWindow(cause error) error {
	if out, err := runTmux(c.tmux, "kill-window", "-t", c.target); err != nil {
		if strings.Contains(out, "can't find") {
			return nil
		}
		return errors.Join(cause, err)
	}
	return nil
}
