//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

func TestTmuxExecutorRunsWorkersInWindows(t *testing.T) {
	logPath := runfixture.FakeTmux(t, "3.8")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	withController(t, "tmux")
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0, "a"))
	code, err := runops.Run(root, runOptions())
	if err != nil || code != 0 {
		t.Fatalf("run: %d %v", code, err)
	}
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "verified" || s.Tasks["b"].Status != "verified" {
		t.Fatalf("statuses %s %s", s.Tasks["a"].Status, s.Tasks["b"].Status)
	}
	session := executor.TmuxSessionName(s.RunID)
	log := runfixture.TmuxLog(t, logPath)
	for _, want := range []string{
		"new-session -d -s " + session + " -n a-a1 ",
		"new-window -d -t " + session + ": -n b-a1 ",
		"set-option -w -t @1 remain-on-exit on",
		"set-option -w -t @2 remain-on-exit on",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("tmux log missing %q:\n%s", want, log)
		}
	}
	if strings.Count(log, "new-session") != 1 {
		t.Errorf("exactly one session per run:\n%s", log)
	}
	if strings.Contains(log, "set-option -g") || strings.Contains(log, "set-option -s") || strings.Contains(log, "source-file") {
		t.Errorf("global tmux options or config touched:\n%s", log)
	}
	for _, cell := range []string{"a", "b"} {
		raw, err := os.ReadFile(filepath.Join(root, "tmux", cell+"-a1.exit"))
		if err != nil || strings.TrimSpace(string(raw)) != "0" {
			t.Errorf("exit file for %s: %q %v", cell, raw, err)
		}
	}
	if rec, _ := executor.ReadExecutorRecord(root); rec != "tmux" {
		t.Fatalf("record %q", rec)
	}
}

func TestTmuxExecutorSelectedByEnvironment(t *testing.T) {
	runfixture.FakeTmux(t, "3.8")
	t.Setenv("IRRIGATOR_EXECUTOR", "tmux")
	withController(t, "")
	root := runfixture.New(t, runfixture.Task("a", 0))
	if code, err := runops.Run(root, runOptions()); err != nil || code != 0 {
		t.Fatalf("run: %d %v", code, err)
	}
	if rec, _ := executor.ReadExecutorRecord(root); rec != "tmux" {
		t.Fatalf("record %q", rec)
	}
}

func TestTmuxVersionGateBlocksBeforeAnyWorker(t *testing.T) {
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	logPath := runfixture.FakeTmux(t, "3.1c")
	withController(t, "tmux")
	root := runfixture.New(t, runfixture.Task("a", 0))
	if _, err := runops.Run(root, runOptions()); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(runfixture.TmuxLog(t, logPath), "new-") {
		t.Error("a worker was launched on an unsupported tmux")
	}
}

// A pane that vanishes without writing an exit file must never read as
// success: the controller stops with "resume to recover" and nothing is marked
// verified.
func TestTmuxPaneGoneWithoutExitFileIsUncertain(t *testing.T) {
	runfixture.FakeTmux(t, "3.8")
	t.Setenv("FAKE_TMUX_RUN", "exit 0")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	withController(t, "tmux")
	root := runfixture.New(t, runfixture.Task("a", 0))
	code, err := runops.Run(root, runOptions())
	if err == nil || code == 0 || !strings.Contains(err.Error(), "resume to recover") {
		t.Fatalf("run: %d %v", code, err)
	}
	if st := runfixture.Read(t, root).Tasks["a"].Status; st == "verified" {
		t.Fatalf("status %s", st)
	}
}
