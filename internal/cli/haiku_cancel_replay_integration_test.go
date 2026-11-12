//go:build integration

package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

func TestHaikuTimeoutPreservesReportAndBlocksRetry(t *testing.T) {
	runfixture.HaikuClaudeStub(t, "cat >/dev/null\nprintf '%s' '{\"structured_output\":{\"answer\":\"ok\"},\"total_cost_usd\":0.07}'\nsleep 10\n")
	root := runfixture.HaikuRoot(t)
	_, done := clitest.Start(t, root, "--timeout", "2s")
	clitest.Finished(t, done, false)
	s := runfixture.Read(t, root)
	row := s.Tasks["a"]
	if row.Status != "uncertain" || row.Cost != .07 || row.Attempt != 1 || provider.ProjectCost(s).UnknownOperations != 1 {
		raw, _ := os.ReadFile(filepath.Join(root, "cells/a/1.stdout.json"))
		log, _ := os.ReadFile(filepath.Join(root, "cells/a/1.stderr.log"))
		t.Fatalf("lost timeout evidence: %+v cost %+v raw=%s stderr=%s", row, provider.ProjectCost(s), raw, log)
	}
	before := row.OperationID
	_, done = clitest.Start(t, root, "--retry-failed")
	clitest.Finished(t, done, false)
	s = runfixture.Read(t, root)
	if s.Tasks["a"].Attempt != 1 || s.Tasks["a"].OperationID != before {
		t.Fatal("automatically replayed uncertain paid call")
	}
	// Explicit retry changes the operation, but cannot erase missing billing.
	if err := retryNode(root, "a", false, true); err != nil {
		t.Fatal(err)
	}
	s = runfixture.Read(t, root)
	if s.Tasks["a"].Status != "pending" || provider.ProjectCost(s).UnknownOperations != 1 || s.Tasks["a"].Cost != .07 {
		t.Fatalf("retry erased billing history: %+v", s.Tasks["a"])
	}
	if err := checkpoint.ValidateManagedState(s); err != nil {
		t.Fatal("legacy schema changed:", err)
	}
	if strings.Contains(provider.CostStatusText(s), "held exposure") {
		t.Fatal("invented a managed reservation for legacy call")
	}
}
func TestHaikuKilledWorkerRetainsCostAndUncertainty(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "dispatched")
	runfixture.HaikuClaudeStub(t, fmt.Sprintf("cat >/dev/null\nprintf '%%s' '{\"structured_output\":{\"answer\":\"ok\"},\"total_cost_usd\":0.07}'\ntouch '%s'\nsleep 10\n", marker))
	root := runfixture.HaikuRoot(t)
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	binary, _ := os.Executable()
	cmd := exec.Command(binary, "_worker", root, "a", "--timeout", "8s", "--budget", "0.25")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stub never dispatched")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for {
		if err := runops.Reconcile(root); err != nil {
			t.Fatal(err)
		}
		if runfixture.Read(t, root).Tasks["a"].Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not drain killed worker")
		}
		time.Sleep(15 * time.Millisecond)
	}
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "uncertain" || s.Tasks["a"].Cost != .07 || provider.ProjectCost(s).UnknownOperations != 1 {
		t.Fatalf("lost killed-call evidence: %+v / %+v", s.Tasks["a"], provider.ProjectCost(s))
	}
	if err := runops.Reconcile(root); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).Tasks["a"].Cost != .07 {
		t.Fatal("reconciliation double counted captured report")
	}
	_, done := clitest.Start(t, root, "--retry-failed")
	clitest.Finished(t, done, false)
	if runfixture.Read(t, root).Tasks["a"].Attempt != 1 {
		t.Fatal("killed call automatically replayed")
	}
}
func TestHaikuOutcomeIndependentOfMissingCost(t *testing.T) {
	for _, raw := range []string{`{bad`, `{"structured_output":{"answer":"ok"}}`, `{"structured_output":{"answer":"ok"},"total_cost_usd":-1}`} {
		t.Run(raw, func(t *testing.T) {
			runfixture.HaikuClaudeStub(t, "cat >/dev/null\nprintf '%s' '"+raw+"'\n")
			root := runfixture.HaikuRoot(t)
			_, done := clitest.Start(t, root)
			clitest.Finished(t, done, raw != "{bad")
			s := runfixture.Read(t, root)
			want := "verified"
			if raw == "{bad" {
				want = "uncertain"
			}
			if s.Tasks["a"].Status != want || provider.ProjectCost(s).UnknownOperations != 1 {
				t.Fatalf("outcome/cost mismatch: %+v", s.Tasks["a"])
			}
		})
	}
}
func TestCancelDuringValidationDoesNotAbandonSibling(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "validating")
	validator := filepath.Join(dir, "validator")
	if err := os.WriteFile(validator, []byte(fmt.Sprintf("#!/bin/sh\ncat >/dev/null\ntouch '%s'\nsleep 10\n", marker)), 0700); err != nil {
		t.Fatal(err)
	}
	a := runfixture.Task("a", .01)
	a.Checks = []model.Check{{Kind: "command", Command: []string{validator}}}
	// b must still be running while a validates; a wide window keeps this true on slow hosts (WSL).
	root := runfixture.New(t, a, runfixture.Task("b", 3))
	binary, _ := os.Executable()
	cmd := exec.Command(binary, "resume", root, "--workers", "2")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = checkpoint.SetControl(root, "down") })
	waitFor(t, root, func(s *model.State) bool {
		_, err := os.Stat(marker)
		return err == nil && s.Tasks["b"].Status == "running"
	})
	if err := runops.CancelNode(root, "a"); err != nil {
		t.Fatal(err)
	}
	clitest.Finished(t, done, false) // Deliberately cancelled graph is blocked, not complete.
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "cancelled" || s.Tasks["b"].Status != "verified" || strings.Contains(stderr.String(), "worker a exited") {
		t.Fatalf("cancel killed controller: a=%s b=%s stderr=%s", s.Tasks["a"].Status, s.Tasks["b"].Status, stderr.String())
	}
}
