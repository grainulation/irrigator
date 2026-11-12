//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
	"github.com/grainulation/irrigator/internal/session"
)

func hcCalls(t *testing.T, logPath, verb string) int {
	t.Helper()
	raw, _ := os.ReadFile(logPath)
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, verb+" ") || line == verb {
			n++
		}
	}
	return n
}

func runOptions() model.Options { return model.Options{Workers: 1, Timeout: 20 * 1e9, Budget: .01} }

func withController(t *testing.T, name string) {
	t.Helper()
	oldExec, oldReg := executor.ActiveExecutor, session.ActiveSessionRegistry
	executor.ActiveExecutor = name
	session.ActiveSessionRegistry = filepath.Join(t.TempDir(), "session.json")
	t.Cleanup(func() { executor.ActiveExecutor, session.ActiveSessionRegistry = oldExec, oldReg })
}

func TestExecutorLocalRunsWorkerAndRecordsLocal(t *testing.T) {
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	withController(t, "")
	root := runfixture.New(t, runfixture.Task("a", 0))
	code, err := runops.Run(root, runOptions())
	if err != nil || code != 0 {
		t.Fatalf("run: %d %v", code, err)
	}
	if s := runfixture.Read(t, root); s.Tasks["a"].Status != "verified" {
		t.Fatalf("status %s", s.Tasks["a"].Status)
	}
	if rec, _ := executor.ReadExecutorRecord(root); rec != "local" {
		t.Fatalf("record %q", rec)
	}
}

func TestHerdrExecutorRunsWorkerThroughHc(t *testing.T) {
	logPath := runfixture.FakeHc(t, "0.1.19")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	withController(t, "herdr")
	root := runfixture.New(t, runfixture.Task("a", 0))
	code, err := runops.Run(root, runOptions())
	if err != nil || code != 0 {
		t.Fatalf("run: %d %v", code, err)
	}
	if s := runfixture.Read(t, root); s.Tasks["a"].Status != "verified" {
		t.Fatalf("status %s", s.Tasks["a"].Status)
	}
	raw, _ := os.ReadFile(logPath)
	log := string(raw)
	runID := runfixture.Read(t, root).RunID
	for _, want := range []string{"irrigator.run=" + runID, "irrigator.cell=a", "irrigator.attempt=1", "--timeout "} {
		if !strings.Contains(log, want) {
			t.Errorf("hc log missing %q:\n%s", want, log)
		}
	}
	if rec, _ := executor.ReadExecutorRecord(root); rec != "herdr" {
		t.Fatalf("record %q", rec)
	}
}

func TestHerdrVersionGateBlocksBeforeAnyWorker(t *testing.T) {
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	for _, version := range []string{"0.1.18", "0.0.99"} {
		logPath := runfixture.FakeHc(t, version)
		withController(t, "herdr")
		root := runfixture.New(t, runfixture.Task("a", 0))
		code, err := runops.Run(root, runOptions())
		if err == nil || code == 0 {
			t.Fatalf("hc %s accepted: %d %v", version, code, err)
		}
		for _, want := range []string{version, executor.HerdrMinVersion, "https://herdr.dev/core/"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("hc %s: message %q lacks %q", version, err, want)
			}
		}
		if hcCalls(t, logPath, "run") != 0 || runfixture.Read(t, root).Tasks["a"].Attempt != 0 {
			t.Errorf("hc %s: a worker was launched", version)
		}
	}
}

func TestExecutorTimeoutAboveMaximumRejectedAtApply(t *testing.T) {
	code, err := builtinCLI([]string{"apply", "plan.json", "--run", t.TempDir() + "/r", "--backend", "mock", "--timeout", "90m"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "per-call maximum") {
		t.Fatalf("code %d err %v", code, err)
	}
}
