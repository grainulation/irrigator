package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// HerdrMinVersion is the oldest herdr-core whose `hc run --timeout`, labels and
// `hc ls --json` behaviour the executor relies on.
const HerdrMinVersion = "0.1.19"

const herdrInstallURL = "https://herdr.dev/core/"

type herdrExecutor struct {
	hc string // resolved hc path
}

func (herdrExecutor) name() string { return "herdr" }

// newHerdrExecutor is the preflight: it fails before any worker is launched.
func newHerdrExecutor() (workerExecutor, error) {
	st := herdrStatus()
	switch st.Status {
	case herdrNotInstalled:
		return nil, fmt.Errorf("the herdr executor needs herdr-core's hc (found none on PATH; required >= %s); install it from %s", HerdrMinVersion, herdrInstallURL)
	case herdrTooOld:
		return nil, fmt.Errorf("herdr-core %s is too old: the herdr executor requires >= %s; upgrade from %s", st.Version, HerdrMinVersion, herdrInstallURL)
	case herdrUnknown:
		return nil, fmt.Errorf("cannot read the version of %s (%s); the herdr executor requires herdr-core >= %s, see %s", st.Path, st.Detail, HerdrMinVersion, herdrInstallURL)
	}
	return herdrExecutor{hc: st.Path}, nil
}

// start launches `binary workerArgs...` for one cell. The caller Waits on cmd,
// signals the returned handle, and calls done after Wait returns.
func (l herdrExecutor) Start(binary, root, runID, cell string, attempt int, workerArgs []string) (cmd *exec.Cmd, child ChildProcess, done func(), err error) {
	// A worker from an earlier controller may still run in herdr-core (its
	// terminal outlives clients). Never start a second one for the same cell.
	live, err := herdrLiveWorkers(l.hc, runID, cell)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cannot confirm that no herdr worker is already running for cell %s: %w", cell, err)
	}
	if len(live) > 0 {
		return nil, nil, nil, fmt.Errorf("herdr terminal %s is still running cell %s; not launching a duplicate. Wait for it, or stop it with hc kill %s and resume", live[0], cell, live[0])
	}
	name := HerdrTerminalName(runID, cell, attempt)
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, nil, err
	}
	logDir := filepath.Join(root, "herdr")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, nil, nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, nil, err
	}
	argv := herdrRunArgs(name, cwd, runID, cell, attempt, HerdrWorkerEnv(os.Environ()), binary, workerArgs)
	cmd = exec.Command(l.hc, argv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The hc client mirrors the worker's screen and its own notes; keep both
	// out of the dashboard and in the run for diagnosis.
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, nil, nil, err
	}
	return cmd, herdrChild{client: cmd.Process, hc: l.hc, name: name}, func() { logFile.Close() }, nil
}

// herdrClientWaitSeconds is one year: effectively "until the worker exits".
const herdrClientWaitSeconds = "31536000"

func herdrRunArgs(name, cwd, runID, cell string, attempt int, env []string, binary string, workerArgs []string) []string {
	// A foreground `hc run` only waits hc's check-in time (180 s by default),
	// then exits 124 while the program keeps running. The controller would
	// read that as a dead worker, so wait as long as the worker may run; the
	// controller enforces the real per-call timeout itself.
	argv := []string{"run", "--name", name, "--cwd", cwd, "--no-notify", "--timeout", herdrClientWaitSeconds,
		"--label", "irrigator.run=" + runID,
		"--label", "irrigator.cell=" + cell,
		"--label", "irrigator.attempt=" + strconv.Itoa(attempt),
		"--"}
	if len(env) > 0 {
		// hc programs inherit herdr-core's environment, not ours.
		argv = append(argv, "env")
		argv = append(argv, env...)
	}
	argv = append(argv, binary)
	return append(argv, workerArgs...)
}

// herdrForwardedEnv keeps PATH and Irrigator's own settings. It never forwards
// credentials: hc shows a terminal's argv to every local API client, so
// providers must already be signed in where herdr-core runs.
func herdrForwardedEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if k == "PATH" || k == "NO_COLOR" || strings.HasPrefix(k, "IRRIGATOR_") || strings.HasPrefix(k, "SWARM_") {
			out = append(out, kv)
		}
	}
	return out
}

// herdrWorkerEnv is the forwarded environment plus worker progress lines, which
// give `hc attach` and `hc read` something to show.
func HerdrWorkerEnv(environ []string) []string {
	var out []string
	for _, kv := range herdrForwardedEnv(environ) {
		if !strings.HasPrefix(kv, workerProgressEnv+"=") {
			out = append(out, kv)
		}
	}
	return append(out, workerProgressEnv+"=1")
}

// herdrTerminalName fits hc's rule: 1-32 of a-z 0-9 - _, unique among live
// terminals. A run hash keeps concurrent runs apart; the attempt keeps a
// retry from colliding with a finished terminal kept for inspection.
func HerdrTerminalName(runID, cell string, attempt int) string {
	sum := sha256.Sum256([]byte(runID))
	suffix := "-" + strconv.Itoa(attempt)
	prefix := "irr-" + hex.EncodeToString(sum[:])[:4] + "-"
	var b strings.Builder
	for _, r := range strings.ToLower(cell) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	slug := b.String()
	if room := 32 - len(prefix) - len(suffix); len(slug) > room {
		slug = slug[:room]
	}
	if slug == "" {
		slug = "cell"
	}
	return prefix + slug + suffix
}

// herdrChild stops the worker inside herdr-core. Signalling only the hc client
// would leave the worker running there: herdr-core terminals outlive clients.
type herdrChild struct {
	client *os.Process
	hc     string
	name   string
}

func (c herdrChild) Signal(sig os.Signal) error {
	if sig == syscall.SIGTERM || sig == os.Interrupt {
		// hc kill hangs up, then escalates; the client exits with the worker.
		return c.hcKill()
	}
	return c.client.Signal(sig)
}

func (c herdrChild) Kill() error {
	err := c.hcKill()
	if kerr := c.client.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		err = errors.Join(err, kerr)
	}
	return err
}

func (c herdrChild) hcKill() error {
	out, err := exec.Command(c.hc, "kill", c.name).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "finished") && !strings.Contains(string(out), "not found") {
		return fmt.Errorf("hc kill %s: %v: %s", c.name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
