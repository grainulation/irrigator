//go:build integration

package cli

// A controller-held flock is close-on-exec and is released by the kernel when
// the controller dies; it proves nothing about a worker or descendant that
// continues to use a worktree. This test kills a real helper controller with
// SIGKILL while its child keeps running inside a dirty worktree, then runs real
// GC on the now-stopped run and asserts that the child, the worktree, its dirty
// content and the stale lock receipt naming the dead controller all survive.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	gcKillRoleEnv     = "IRRIGATOR_TEST_GC_KILL_ROLE"
	gcKillRunEnv      = "IRRIGATOR_TEST_GC_KILL_RUN"
	gcKillMarkersEnv  = "IRRIGATOR_TEST_GC_KILL_MARKERS"
	gcKillWorktreeEnv = "IRRIGATOR_TEST_GC_KILL_WORKTREE"
	gcKillHelperRun   = "-test.run=^TestGCKillHelperProcess$"
	gcKillSentinel    = "child-sentinel.txt"
)

func gcKillPublish(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func gcKillWaitFile(path string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return string(data), true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", false
}

func gcKillAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func TestGCAfterControllerSIGKILLPreservesContinuingChildAndDirtyWorktree(t *testing.T) {
	r := gcRepoFixture(t)
	run := filepath.Join(t.TempDir(), "run")
	gcRunFixture(t, run)
	markers := t.TempDir()
	logs := t.TempDir()
	logFile, err := os.Create(filepath.Join(logs, "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	diagnostics := func() string {
		data, _ := os.ReadFile(filepath.Join(logs, "controller.log"))
		errText, _ := os.ReadFile(filepath.Join(markers, "controller.err"))
		return string(errText) + "\n" + string(data)
	}

	ctrl := exec.Command(os.Args[0], gcKillHelperRun)
	ctrl.Env = append(os.Environ(), gcKillRoleEnv+"=controller", gcKillRunEnv+"="+run, gcKillMarkersEnv+"="+markers, gcKillWorktreeEnv+"="+r.sibling)
	// A file rather than a pipe: Wait must not block copying output from a
	// descriptor some surviving descendant could still hold.
	ctrl.Stdout, ctrl.Stderr = logFile, logFile
	if err := ctrl.Start(); err != nil {
		t.Fatal(err)
	}
	childPID := 0
	// Registered after the temp directories, so it runs before their removal.
	t.Cleanup(func() {
		_ = gcKillPublish(filepath.Join(markers, "stop"), "stop")
		if ctrl.ProcessState == nil {
			_ = ctrl.Process.Kill()
			_ = ctrl.Wait()
		}
		if childPID == 0 {
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for gcKillAlive(childPID) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if gcKillAlive(childPID) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
			t.Errorf("helper child %d ignored stop marker; killed", childPID)
		}
	})

	ready, ok := gcKillWaitFile(filepath.Join(markers, "controller.ready"), 20*time.Second)
	if !ok {
		t.Fatalf("helper controller not ready: %s", diagnostics())
	}
	childPID, err = strconv.Atoi(strings.TrimSpace(ready))
	if err != nil || childPID <= 0 || childPID == ctrl.Process.Pid {
		t.Fatalf("bad child pid %q: %v", ready, err)
	}
	sentinel := filepath.Join(r.sibling, gcKillSentinel)
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("child sentinel missing: %v", err)
	}

	// The live controller blocks GC.
	if code, err := runCommand([]string{"gc", run, "--apply"}); code == 0 || err == nil {
		t.Fatal("GC accepted a live controller")
	}

	// The receipt format the withdrawn collector trusted, naming the controller
	// that is about to die. Dead controller identity proves nothing here.
	receipt := gcForgeReceipt(t, r, "run00001-a-a1", r.sibling, "irrigator/run00001/a", ctrl.Process.Pid)
	receiptBefore, _ := os.ReadFile(receipt)

	controllerPID := ctrl.Process.Pid
	if err := ctrl.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := ctrl.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("controller wait: %v %s", waitErr, diagnostics())
	}
	if ws, ok := ctrl.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("controller did not die by SIGKILL: %v", ctrl.ProcessState)
	}
	if err := syscall.Kill(controllerPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("controller %d still present after reaping: %v", controllerPID, err)
	}

	beatAdvances := func(label string) {
		t.Helper()
		first, _ := os.ReadFile(filepath.Join(markers, "heartbeat"))
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if !gcKillAlive(childPID) {
				t.Fatalf("%s: continuing child %d died", label, childPID)
			}
			if now, _ := os.ReadFile(filepath.Join(markers, "heartbeat")); len(now) > 0 && string(now) != string(first) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: child heartbeat stalled", label)
	}
	beatAdvances("after controller SIGKILL")

	siblingBefore := gcTree(t, r.sibling)
	gitBefore := gcGitState(t, r)

	// The kernel dropped the controller's flock: GC now runs on a stopped run.
	if code, err := runCommand([]string{"gc", run, "--apply"}); code != 0 || err != nil {
		t.Fatalf("gc --apply on stopped run: %d %v", code, err)
	}
	for _, name := range gcObsolete {
		if _, e := os.Lstat(filepath.Join(run, "cells", "a", name)); !errors.Is(e, os.ErrNotExist) {
			t.Fatalf("GC did not collect obsolete attempt log %s: %v", name, e)
		}
	}

	beatAdvances("after GC")
	gcSameTree(t, "child worktree", siblingBefore, gcTree(t, r.sibling))
	if got := gcGitState(t, r); got != gitBefore {
		t.Fatalf("git worktrees/refs changed:\n%s\n---\n%s", gitBefore, got)
	}
	if data, err := os.ReadFile(sentinel); err != nil || !strings.Contains(string(data), "continuing worker") {
		t.Fatalf("child sentinel lost: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(r.sibling, "untracked", "work.txt")); err != nil || string(data) != "untracked work\n" {
		t.Fatalf("dirty untracked work lost: %q %v", data, err)
	}
	if data, err := os.ReadFile(receipt); err != nil || string(data) != string(receiptBefore) {
		t.Fatalf("lock receipt changed or removed: %q %v", data, err)
	}

	// Bounded, verified child shutdown: no leaked helper.
	if err := gcKillPublish(filepath.Join(markers, "stop"), "stop"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for gcKillAlive(childPID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if gcKillAlive(childPID) {
		t.Fatalf("continuing child %d did not stop within bound", childPID)
	}
	childPID = 0
}
