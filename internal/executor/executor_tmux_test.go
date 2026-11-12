package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTmuxParseVersion(t *testing.T) {
	for _, tc := range []struct {
		out  string
		ver  string
		good bool
	}{
		{"tmux 3.8", "3.8", true},
		{"tmux 3.3a\n", "3.3", true},
		{"tmux next-3.6", "3.6", true},
		{"tmux 3.2", "3.2", true},
		{"tmux 3.2a", "3.2", true},
		{"tmux 3.1c", "3.1", false},
		{"tmux 2.9", "2.9", false},
		{"tmux 10.0", "10.0", true},
		{"tmux master", "", false},
		{"garbage", "", false},
	} {
		v, parts, ok := parseTmuxVersion(tc.out)
		if tc.ver == "" {
			if ok {
				t.Errorf("%q parsed as %q", tc.out, v)
			}
			continue
		}
		_, min, _ := parseTmuxVersion("tmux " + tmuxMinVersion)
		if !ok || v != tc.ver || versionAtLeast(parts, min) != tc.good {
			t.Errorf("%q: version %q ok=%v atLeast=%v, want %q atLeast=%v", tc.out, v, ok, versionAtLeast(parts, min), tc.ver, tc.good)
		}
	}
}

func TestTmuxPreflightErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := newTmuxExecutor(); err == nil || !strings.Contains(err.Error(), "needs tmux") {
		t.Errorf("no tmux: %v", err)
	}
	runfixture.FakeTmux(t, "3.1c")
	if _, err := newTmuxExecutor(); err == nil || !strings.Contains(err.Error(), "too old") || !strings.Contains(err.Error(), tmuxMinVersion) {
		t.Errorf("old tmux: %v", err)
	}
	runfixture.FakeTmux(t, "garbage")
	if _, err := newTmuxExecutor(); err == nil || !strings.Contains(err.Error(), "cannot read the version") {
		t.Errorf("garbage tmux: %v", err)
	}
	runfixture.FakeTmux(t, "3.3a")
	if _, err := newTmuxExecutor(); err != nil {
		t.Errorf("good tmux: %v", err)
	}
}

func TestTmuxDoctorExecutorsReport(t *testing.T) {
	find := func() map[string]any {
		for _, e := range DoctorExecutors() {
			if e["name"] == "tmux" {
				return e
			}
		}
		t.Fatal("doctor lists no tmux executor")
		return nil
	}
	t.Setenv("PATH", t.TempDir())
	if e := find(); e["status"] != tmuxNotInstalled || e["available"] != false {
		t.Errorf("none: %v", e)
	}
	runfixture.FakeTmux(t, "3.0")
	if e := find(); e["status"] != tmuxTooOld || !strings.Contains(e["summary"].(string), "too old") {
		t.Errorf("old: %v", e)
	}
	runfixture.FakeTmux(t, "next-3.6")
	if e := find(); e["status"] != tmuxOK || e["version"] != "3.6" || e["summary"] != "OK" || e["tmux_path"] == "" {
		t.Errorf("good: %v", e)
	}
}

func TestTmuxNames(t *testing.T) {
	if got := TmuxSessionName("abcdef0123456789"); got != "irrigator-abcdef01" {
		t.Errorf("session %q", got)
	}
	if got := TmuxSessionName("ab.c:d"); strings.ContainsAny(got, ".: ") {
		t.Errorf("session %q", got)
	}
	safe := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for _, cell := range []string{"build", "Parse.Config/v2", "a:b c", "日本", ""} {
		w := tmuxWindowName(cell, 3)
		if !safe.MatchString(w) || !strings.HasSuffix(w, "-a3") {
			t.Errorf("cell %q: window %q", cell, w)
		}
	}
	if tmuxWindowName("x", 1) == tmuxWindowName("x", 2) {
		t.Error("a retry must not reuse the previous attempt's window name")
	}
	if got := tmuxWindowName("build", 2); got != "build-a2" {
		t.Errorf("window %q", got)
	}
}

// The window command is run by tmux through the user's shell; every word must
// reach the program unchanged, and the exit code must be recorded.
func TestTmuxShellQuoting(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	prog := filepath.Join(dir, "prog with space.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$ARGV_FILE\"\nprintf '%s' \"$MARKER\" > \"$ARGV_FILE.env\"\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"_worker", "/runs/with space/r", "it's", `say "hi"`, "$HOME", "`id`", "$(id)", "a;b && c", "line1\nline2", "*", "", `back\slash`, "--x=y z"}
	env := []string{"ARGV_FILE=" + argvFile, "MARKER=it's $HOME \"q\" a b", "PATH=" + os.Getenv("PATH")}
	exitFile := filepath.Join(dir, "x dir.exit")
	command := tmuxWindowCommand(exitFile, env, prog, args)

	for _, shell := range []string{"/bin/sh", "/bin/bash", "/bin/zsh"} {
		if _, err := os.Stat(shell); err != nil {
			continue
		}
		_ = os.Remove(exitFile)
		if err := exec.Command(shell, "-c", command).Run(); err == nil {
			t.Fatalf("%s: wrapper swallowed the worker's exit code 7", shell)
		} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
			t.Fatalf("%s: %v", shell, err)
		}
		raw, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		if !slices.Equal(got, args) {
			t.Errorf("%s: argv\n got %q\nwant %q", shell, got, args)
		}
		if m, _ := os.ReadFile(argvFile + ".env"); string(m) != env[1][len("MARKER="):] {
			t.Errorf("%s: env value %q", shell, m)
		}
		if code, _ := os.ReadFile(exitFile); strings.TrimSpace(string(code)) != "7" {
			t.Errorf("%s: exit file %q", shell, code)
		}
		if _, err := os.Stat(exitFile + ".tmp"); err == nil {
			t.Errorf("%s: temp exit file left behind", shell)
		}
	}
}

func TestTmuxWindowCommandNeverForwardsCredentials(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("GITHUB_TOKEN", "ghp-secret")
	t.Setenv("IRRIGATOR_FOO", "bar")
	t.Setenv("IRRIGATOR_WORKER_PROGRESS", "0")
	command := tmuxWindowCommand("/r/x.exit", HerdrWorkerEnv(os.Environ()), "/bin/irrigator", []string{"_worker", "/r"})
	if strings.Contains(command, "secret") || strings.Contains(command, "ANTHROPIC") || strings.Contains(command, "GITHUB") {
		t.Errorf("credentials in the window command: %s", command)
	}
	for _, want := range []string{"'IRRIGATOR_FOO=bar'", "'" + workerProgressEnv + "=1'", "'PATH="} {
		if !strings.Contains(command, want) {
			t.Errorf("window command lacks %s: %s", want, command)
		}
	}
	if strings.Contains(command, workerProgressEnv+"=0") {
		t.Errorf("progress must be forced on: %s", command)
	}
}

func TestTmuxLiveWindowsFromListing(t *testing.T) {
	listing := "build-a1 0\nbuild-a2 1\nbuild-a-a1 0\nother-a1 0\nbuild-a3x 0\nweird_cell-a4 0"
	if got := liveWindowsFromListing(listing, "build"); !slices.Equal(got, []string{"build-a1"}) {
		t.Errorf("build: %v", got)
	}
	if got := liveWindowsFromListing(listing, "weird.cell"); !slices.Equal(got, []string{"weird_cell-a4"}) {
		t.Errorf("weird.cell: %v", got)
	}
	if got := liveWindowsFromListing(listing, "build-a"); !slices.Equal(got, []string{"build-a-a1"}) {
		t.Errorf("build-a: %v", got)
	}
}

func TestTmuxExecutorRefusesLiveDuplicate(t *testing.T) {
	logPath := runfixture.FakeTmux(t, "3.8")
	t.Setenv("FAKE_TMUX_RUN", "exec sleep 30")
	ex := tmuxExecutor{tmux: mustLookPath(t, "tmux")}
	root := t.TempDir()
	cmd, child, done, err := ex.Start("/bin/sh", root, "run-12345678-x", "build", 1, []string{"-c", "true"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Kill(); _ = cmd.Wait(); done() })

	_, _, _, err = ex.Start("/bin/sh", root, "run-12345678-x", "build", 2, []string{"-c", "true"})
	if err == nil || !strings.Contains(err.Error(), "not launching a duplicate") || !strings.Contains(err.Error(), "build-a1") {
		t.Fatalf("duplicate not refused: %v", err)
	}
	if n := strings.Count(runfixture.TmuxLog(t, logPath), "new-window"); n != 0 {
		t.Errorf("a second window was created (%d new-window calls)", n)
	}
	// A different cell in the same run is fine.
	cmd2, child2, done2, err := ex.Start("/bin/sh", root, "run-12345678-x", "other", 1, []string{"-c", "true"})
	if err != nil {
		t.Fatalf("different cell refused: %v", err)
	}
	_ = child2.Kill()
	_ = cmd2.Wait()
	done2()
}

func TestTmuxExecutorIgnoresFinishedWindow(t *testing.T) {
	runfixture.FakeTmux(t, "3.8")
	t.Setenv("FAKE_TMUX_RUN", "exit 0") // pane ends at once and stays, dead
	ex := tmuxExecutor{tmux: mustLookPath(t, "tmux")}
	root := t.TempDir()
	cmd, _, done, err := ex.Start("/bin/sh", root, "run-12345678-x", "build", 1, []string{"-c", "true"})
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	done()
	time.Sleep(100 * time.Millisecond)
	cmd, _, done, err = ex.Start("/bin/sh", root, "run-12345678-x", "build", 2, []string{"-c", "true"})
	if err != nil {
		t.Fatalf("a finished window blocked the retry: %v", err)
	}
	_ = cmd.Wait()
	done()
}

func TestTmuxExecutorRefusesWhenListingFails(t *testing.T) {
	runfixture.FakeTmux(t, "3.8")
	// A tmux whose has-session succeeds but whose list-panes fails.
	bin := filepath.Dir(mustLookPath(t, "tmux"))
	script := "#!/bin/sh\ncase \"$1\" in -V) echo 'tmux 3.8';; has-session) exit 0;; *) echo boom >&2; exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ex := tmuxExecutor{tmux: filepath.Join(bin, "tmux")}
	_, _, _, err := ex.Start("/bin/sh", t.TempDir(), "run-12345678", "a", 1, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot confirm") {
		t.Fatalf("err %v", err)
	}
}

func TestTmuxExecutorWaitReturnsWorkerExitCode(t *testing.T) {
	runfixture.FakeTmux(t, "3.8")
	ex := tmuxExecutor{tmux: mustLookPath(t, "tmux")}
	root := t.TempDir()
	cmd, _, done, err := ex.Start("/bin/sh", root, "run-12345678", "code", 1, []string{"-c", "sleep 0.3; exit 7"})
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	done()
	if code := cmd.ProcessState.ExitCode(); code != 7 {
		t.Fatalf("exit code %d (%v), want 7", code, err)
	}
}

func TestTmuxExecutorStaleExitFileIsNotRead(t *testing.T) {
	runfixture.FakeTmux(t, "3.8")
	ex := tmuxExecutor{tmux: mustLookPath(t, "tmux")}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tmux"), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "tmux", "stale-a1.exit")
	if err := os.WriteFile(stale, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_RUN", "sleep 0.5") // dies without writing an exit file
	cmd, _, done, err := ex.Start("/bin/sh", root, "run-12345678", "stale", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	done()
	if err == nil {
		t.Fatal("a stale exit file was taken as the worker's success")
	}
}

func TestTmuxChildSignalAndKill(t *testing.T) {
	runfixture.FakeTmux(t, "3.8")
	t.Setenv("FAKE_TMUX_RUN", "exec sleep 30")
	ex := tmuxExecutor{tmux: mustLookPath(t, "tmux")}
	cmd, child, done, err := ex.Start("/bin/sh", t.TempDir(), "run-12345678", "sig", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait(); done() }()
	select {
	case <-waited:
		t.Fatal("waiter returned while the pane was alive")
	case <-time.After(400 * time.Millisecond):
	}
	if err := child.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("a signalled pane without an exit file read as success")
		}
	case <-time.After(10 * time.Second):
		_ = child.Kill()
		t.Fatal("waiter kept waiting after the pane's process was signalled")
	}
	// The pane is dead now: further signals report a finished process.
	if err := child.Signal(syscall.SIGTERM); err == nil {
		t.Error("signalling a dead pane should report it is done")
	}
	if err := child.Kill(); err != nil {
		t.Errorf("kill of a dead pane: %v", err)
	}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
