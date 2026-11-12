//go:build integration

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

// These tests pin recovery for herdr-hosted workers with a fake hc. What a
// fake cannot show: that a real herdr-core terminal keeps the worker (and the
// worker's flock on the cell lease) alive after its client or the controller
// dies, and the exact `hc ls --json` schema; both are assumed here.

func herdrListing(t *testing.T, runID, cell string, attempt int, state string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ls.json")
	body := fmt.Sprintf(`[{"name":%q,"state":%q,"labels":{"irrigator.run":%q,"irrigator.cell":%q,"irrigator.attempt":"%d"}},
{"name":"someone-else","state":"running","labels":{"irrigator.run":"other-run","irrigator.cell":%q}}]`, executor.HerdrTerminalName(runID, cell, attempt), state, runID, cell, attempt, cell)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// markRunning is what a worker that started (and was then lost) leaves behind.
func markRunning(t *testing.T, root string) {
	t.Helper()
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Status = "running"
		s.Tasks["a"].Attempt = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func runInBackground(root string) chan struct {
	code int
	err  error
} {
	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := runops.Run(root, runOptions())
		done <- struct {
			code int
			err  error
		}{code, err}
	}()
	return done
}

// (a) The controller died; the worker keeps running in herdr-core and so keeps
// the cell lease. A resumed controller must wait, not launch a duplicate, and
// once the worker is gone without a result the cell is uncertain (paid
// provider), never verified or retried.
func TestHerdrRecoveryControllerDiedWorkerStillRunning(t *testing.T) {
	runfixture.HaikuClaudeStub(t, "echo 'unexpected fake-provider dispatch' >&2; exit 97\n")
	logPath := runfixture.FakeHc(t, "0.1.19")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	withController(t, "herdr")
	root := runfixture.HaikuRoot(t)
	markRunning(t, root)
	t.Setenv("FAKE_HC_LS", herdrListing(t, runfixture.Read(t, root).RunID, "a", 1, "running"))
	lease, err := checkpoint.FileLock(filepath.Join(root, "cells", "a", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	done := runInBackground(root)
	select {
	case r := <-done:
		checkpoint.Unlock(lease)
		t.Fatalf("resume returned while the worker was alive: %+v", r)
	case <-time.After(600 * time.Millisecond):
	}
	if s := runfixture.Read(t, root); s.Tasks["a"].Status != "running" || s.Tasks["a"].Attempt != 1 {
		t.Fatalf("live worker's cell was touched: %+v", s.Tasks["a"])
	}
	if n := hcCalls(t, logPath, "run"); n != 0 {
		t.Fatalf("launched %d duplicate worker(s)", n)
	}
	checkpoint.
		// The worker dies without committing a result.
		Unlock(lease)
	select {
	case r := <-done:
		if r.code == 0 {
			t.Fatalf("run reported success: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	row := runfixture.Read(t, root).Tasks["a"]
	if row.Status != "uncertain" || row.Attempt != 1 || len(row.Result) != 0 {
		t.Fatalf("lost paid worker must be uncertain, got %+v", row)
	}
	if hcCalls(t, logPath, "run") != 0 {
		t.Fatal("an uncertain cell was relaunched")
	}
	if err := retryNode(root, "a", false, false); err == nil {
		t.Fatal("uncertain cell retried without explicit confirmation")
	}
}

// (b) The hc client exits non-zero (for example herdr-core restarted) and the
// worker's fate is unknown. The controller stops and asks for a resume; the
// resume does not verify or relaunch the cell.
func TestHerdrRecoveryClientExitsUnexpectedly(t *testing.T) {
	runfixture.HaikuClaudeStub(t, "echo 'unexpected fake-provider dispatch' >&2; exit 97\n")
	logPath := runfixture.FakeHc(t, "0.1.19")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	t.Setenv("FAKE_HC_RUN", "exit 3")
	withController(t, "herdr")
	root := runfixture.HaikuRoot(t)
	code, err := runops.Run(root, runOptions())
	if err == nil || code == 0 || !strings.Contains(err.Error(), "resume to recover") {
		t.Fatalf("client failure must stop the controller: %d %v", code, err)
	}
	if s := runfixture.Read(t, root); s.Control != "down" || s.Tasks["a"].Status == "verified" {
		t.Fatalf("after client exit: control %s, cell %s", s.Control, s.Tasks["a"].Status)
	}
	// No hc kill is sent: the client already exited, so the controller cannot
	// tell a dead worker from a live one. If it is alive it keeps the cell
	// lease, which is what holds off a duplicate (see test (a)).
	// The worker had started before herdr-core went away; its terminal may
	// still be listed. Resume must neither verify nor relaunch it.
	markRunning(t, root)
	t.Setenv("FAKE_HC_LS", herdrListing(t, runfixture.Read(t, root).RunID, "a", 1, "running"))
	t.Setenv("FAKE_HC_RUN", `exec "$@"`)
	runsBefore := hcCalls(t, logPath, "run")
	code, err = runops.Run(root, runOptions())
	if code == 0 {
		t.Fatalf("resume reported success: %v", err)
	}
	row := runfixture.Read(t, root).Tasks["a"]
	if row.Status != "uncertain" || row.Attempt != 1 || len(row.Result) != 0 {
		t.Fatalf("cell whose worker may have run must be uncertain: %+v", row)
	}
	if n := hcCalls(t, logPath, "run"); n != runsBefore {
		t.Fatalf("resume launched %d new worker(s)", n-runsBefore)
	}
	if err := retryNode(root, "a", false, false); err == nil {
		t.Fatal("uncertain cell retried without explicit confirmation")
	}
}

// A live herdr terminal for the cell blocks a launch even when the lease
// cannot show it (for example a worker still starting).
func TestHerdrLiveTerminalBlocksDuplicateLaunch(t *testing.T) {
	logPath := runfixture.FakeHc(t, "0.1.19")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	withController(t, "herdr")
	root := runfixture.New(t, runfixture.Task("a", 0))
	t.Setenv("FAKE_HC_LS", herdrListing(t, runfixture.Read(t, root).RunID, "a", 1, "running"))
	code, err := runops.Run(root, runOptions())
	if err == nil || code == 0 || !strings.Contains(err.Error(), "not launching a duplicate") {
		t.Fatalf("duplicate not refused: %d %v", code, err)
	}
	if hcCalls(t, logPath, "run") != 0 || runfixture.Read(t, root).Tasks["a"].Attempt != 0 {
		t.Fatal("a worker was launched")
	}
	// A finished terminal kept for inspection does not block.
	t.Setenv("FAKE_HC_LS", herdrListing(t, runfixture.Read(t, root).RunID, "a", 1, "finished"))
	withController(t, "herdr")
	if code, err = runops.Run(root, runOptions()); err != nil || code != 0 {
		t.Fatalf("finished terminal blocked the run: %d %v", code, err)
	}
}

// If herdr cannot say what is running, do not guess.
func TestHerdrLaunchFailsClosedWhenListingIsUnreadable(t *testing.T) {
	logPath := runfixture.FakeHc(t, "0.1.19")
	t.Setenv("IRRIGATOR_EXECUTOR", "")
	bad := filepath.Join(t.TempDir(), "ls.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_HC_LS", bad)
	withController(t, "herdr")
	root := runfixture.New(t, runfixture.Task("a", 0))
	if code, err := runops.Run(root, runOptions()); err == nil || code == 0 {
		t.Fatalf("launched without a readable listing: %d %v", code, err)
	}
	if hcCalls(t, logPath, "run") != 0 {
		t.Fatal("a worker was launched")
	}
}
