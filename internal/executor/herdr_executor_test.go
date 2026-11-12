package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var hcName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func TestHerdrTerminalNameFitsHcRules(t *testing.T) {
	for _, cell := range []string{"build", "Parse.Config/v2", strings.Repeat("x", 80), "日本", ""} {
		name := HerdrTerminalName("run-123", cell, 12)
		if !hcName.MatchString(name) {
			t.Errorf("cell %q: name %q breaks hc's rule", cell, name)
		}
		if !strings.HasSuffix(name, "-12") {
			t.Errorf("cell %q: name %q lost the attempt", cell, name)
		}
	}
	if HerdrTerminalName("run-a", "x", 1) == HerdrTerminalName("run-b", "x", 1) {
		t.Error("different runs must not share terminal names")
	}
	if HerdrTerminalName("run-a", "x", 1) == HerdrTerminalName("run-a", "x", 2) {
		t.Error("a retry must not reuse the previous attempt's name")
	}
}

func TestHerdrForwardedEnvExcludesCredentials(t *testing.T) {
	got := herdrForwardedEnv([]string{
		"PATH=/bin", "IRRIGATOR_LAUNCH_CONTEXT=/x/ctx.json", "SWARM_ALLOWED_TEST=1", "NO_COLOR=1",
		"ANTHROPIC_API_KEY=secret", "OPENAI_API_KEY=secret", "GITHUB_TOKEN=secret", "HOME=/Users/a", "MALFORMED",
	})
	want := []string{"PATH=/bin", "IRRIGATOR_LAUNCH_CONTEXT=/x/ctx.json", "SWARM_ALLOWED_TEST=1", "NO_COLOR=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("forwarded %v, want %v", got, want)
	}
}

func TestHerdrRunArgs(t *testing.T) {
	got := herdrRunArgs("irr-ab12-build-1", "/repo", "run-1", "build", 1, []string{"PATH=/bin"}, "/bin/irrigator", []string{"_worker", "/runs/r", "build"})
	sep := slices.Index(got, "--")
	if got[0] != "run" || sep < 0 {
		t.Fatalf("argv %v", got)
	}
	if want := []string{"env", "PATH=/bin", "/bin/irrigator", "_worker", "/runs/r", "build"}; !slices.Equal(got[sep+1:], want) {
		t.Errorf("program argv = %v, want %v", got[sep+1:], want)
	}
	for _, flag := range []string{"--name", "--cwd", "--no-notify", "--label"} {
		if !slices.Contains(got[:sep], flag) {
			t.Errorf("missing %s in %v", flag, got[:sep])
		}
	}
	if i := slices.Index(got[:sep], "--timeout"); i < 0 || got[i+1] != herdrClientWaitSeconds {
		t.Errorf("the client must wait for the worker, not hc's 180 s check-in: %v", got[:sep])
	}
	if slices.Contains(got, "--bg") {
		t.Error("the client must stay in the foreground so Wait returns the worker's exit code")
	}
}

// Runs only where herdr-core is installed: the worker exits with its own code
// through hc, and stopping the handle ends the program inside herdr-core.
func TestHerdrLauncherLive(t *testing.T) {
	hc, err := exec.LookPath("hc")
	if err != nil || os.Getenv("IRRIGATOR_HERDR_LIVE") == "" {
		t.Skip("set IRRIGATOR_HERDR_LIVE=1 with hc installed")
	}
	l := herdrExecutor{hc: hc}
	root := t.TempDir()
	runID := "live-" + time.Now().Format("150405.000000")

	cmd, _, done, err := l.Start("/bin/sh", root, runID, "exit-code", 1, []string{"-c", "sleep 0.5; exit 7"})
	if err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	done()
	if code := cmd.ProcessState.ExitCode(); code != 7 {
		t.Fatalf("exit code through hc = %d (%v), want 7", code, err)
	}
	_ = exec.Command(hc, "rm", HerdrTerminalName(runID, "exit-code", 1)).Run()

	cmd, child, done, err := l.Start("/bin/sh", root, runID, "stop", 1, []string{"-c", "sleep 300"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := child.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); done(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(15 * time.Second):
		_ = child.Kill()
		t.Fatal("worker kept running in herdr-core after Signal")
	}
	name := HerdrTerminalName(runID, "stop", 1)
	out, _ := exec.Command(hc, "get", name).Output()
	if !strings.Contains(string(out), `"state":"finished"`) {
		t.Errorf("terminal not finished after stop: %s", out)
	}
	_ = exec.Command(hc, "rm", name).Run()
	if _, err := os.Stat(filepath.Join(root, "herdr", name+".log")); err != nil {
		t.Errorf("client log missing: %v", err)
	}
}
