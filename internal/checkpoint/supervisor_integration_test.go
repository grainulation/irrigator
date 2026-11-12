//go:build integration

package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func TestSupervisorWorkerHelper(t *testing.T) {
	root := os.Getenv("SWARM_PROCESS_HELPER")
	if root == "" {
		t.Skip("subprocess helper")
	}
	lease, err := FileLock(filepath.Join(root, "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer Unlock(lease)
	ctx := context.WithValue(context.Background(), launch.ProcessLeaseKey{}, lease)
	cmd := exec.Command("/bin/sh", filepath.Join(root, "provider.sh"), filepath.Join(root, "pid"))
	_, err = launch.RunProcess(ctx, cmd, filepath.Join(root, "attempt"), []byte(`{}`))
	t.Fatalf("provider returned before worker death: %v", err)
}

func TestWorkerDeathRetainsLeaseUntilProviderStops(t *testing.T) {
	root := t.TempDir()
	// Ignore TERM to exercise escalation and keep the lease observably held
	// after worker death while the supervisor drains the orphan process group.
	script := "#!/bin/sh\ntrap '' TERM\necho $$ > \"$1\"\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(root, "provider.sh"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(binary, "-test.run=^TestSupervisorWorkerHelper$")
	helper.Env = append(os.Environ(), "SWARM_PROCESS_HELPER="+root)
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = helper.Process.Kill() })
	deadline := time.Now().Add(10 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(root, "pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if pid > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("provider never started")
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := syscall.Kill(-helper.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	lease, err := FileLock(filepath.Join(root, "lease"), true)
	if err == nil {
		Unlock(lease)
		t.Fatal("worker death released lease before orphan cleanup")
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lease, err = FileLock(filepath.Join(root, "lease"), true)
		if err == nil {
			defer Unlock(lease)
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				marker, _ := os.ReadFile(filepath.Join(root, "attempt.process.json"))
				var process model.ProcessMarker
				_ = json.Unmarshal(marker, &process)
				state, psErr := exec.Command("/bin/ps", "-o", "pid,ppid,pgid,stat,command", "-p", strconv.Itoa(pid), "-p", strconv.Itoa(process.SupervisorPID)).CombinedOutput()
				t.Fatalf("retry lease became available while provider %d still exists: %v; marker=%s; supervisor_probe=%v; group_probe=%v; process=%s; ps_error=%v", pid, err, marker, syscall.Kill(process.SupervisorPID, 0), syscall.Kill(-pid, 0), state, psErr)
			}
			raw, readErr := os.ReadFile(filepath.Join(root, "attempt.process.json"))
			var marker model.ProcessMarker
			if readErr != nil || json.Unmarshal(raw, &marker) != nil || marker.Status != "stopped" {
				t.Fatalf("lease released without confirmed cleanup: marker=%s err=%v", raw, readErr)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("supervisor did not finish cleanup and release lease")
}

func TestSupervisorDeathLeavesUnresolvedMarker(t *testing.T) {
	cell := t.TempDir()
	prefix := filepath.Join(cell, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", `echo $$ > "$1"; exec sleep 30`, "provider", filepath.Join(cell, "pid"))
	done := make(chan error, 1)
	go func() { _, err := launch.RunProcess(ctx, cmd, prefix, []byte(`{}`)); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(cell, "pid"))
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if pid > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("provider did not start")
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	raw, err := os.ReadFile(prefix + ".process.json")
	if err != nil {
		t.Fatal(err)
	}
	var marker model.ProcessMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Status != "active" || marker.SupervisorPID <= 0 {
		t.Fatalf("missing launch intent: %+v", marker)
	}
	// This PID is obtained from this test's live supervisor; production recovery
	// deliberately never kills processes by persisted IDs.
	if err := syscall.Kill(marker.SupervisorPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("supervisor death was reported as success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor death did not return")
	}
	unresolved, err := launch.UnresolvedProcess(cell, 1)
	if err != nil || !unresolved {
		t.Fatalf("lost unresolved process marker: %v %v", unresolved, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("test did not retain orphan for ambiguity check: %v", err)
	}
	// Reusing the same attempt must not overwrite the active marker or launch.
	if _, err := launch.RunProcess(ctx, exec.Command("/bin/echo", "should not launch"), prefix, nil); err == nil {
		t.Fatal("active attempt was replayed")
	}
	if unresolved, err := launch.UnresolvedProcess(cell, 1); err != nil || !unresolved {
		t.Fatal("active marker was overwritten")
	}
}

func TestExplicitProcessCleanupAcknowledgementPreservesEvidence(t *testing.T) {
	cell := t.TempDir()
	originals := map[string][]byte{
		"1.process.json":         []byte(`{"status":"active","supervisor_pid":123}`),
		"1.check-0.process.json": []byte{0xff, '{', 'b', 'a', 'd'},
	}
	for name, original := range originals {
		if err := os.WriteFile(filepath.Join(cell, name), original, 0600); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := FileLock(filepath.Join(cell, "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer Unlock(lease)
	if err := launch.AcknowledgeProcessCleanup(cell, 1); err != nil {
		t.Fatal(err)
	}
	if unresolved, err := launch.UnresolvedProcess(cell, 1); unresolved || err != nil {
		t.Fatalf("acknowledged cleanup still unresolved: %v %v", unresolved, err)
	}
	for name, original := range originals {
		raw, err := os.ReadFile(filepath.Join(cell, name))
		if err != nil {
			t.Fatal(err)
		}
		var marker model.ProcessMarker
		if err := json.Unmarshal(raw, &marker); err != nil {
			t.Fatal(err)
		}
		if marker.Status != "stopped" || marker.ConfirmedBy != "explicit-user" || marker.PreviousStatus == "" {
			t.Fatalf("missing explicit acknowledgement provenance: %+v", marker)
		}
		raw, err = os.ReadFile(filepath.Join(cell, name) + ".unresolved.json")
		if err != nil {
			t.Fatal(err)
		}
		var audit struct {
			Original    []byte `json:"original_bytes"`
			ConfirmedBy string `json:"confirmed_by"`
		}
		if err := json.Unmarshal(raw, &audit); err != nil {
			t.Fatal(err)
		}
		if string(audit.Original) != string(original) || audit.ConfirmedBy != "explicit-user" {
			t.Fatal("original evidence not preserved exactly")
		}
	}
	if err := launch.AcknowledgeProcessCleanup(cell, 1); err != nil {
		t.Fatalf("acknowledgement not idempotent: %v", err)
	}
}

func TestProcessCleanupAcknowledgementDoesNotDiscardEvidence(t *testing.T) {
	for _, scenario := range []string{"oversized", "conflicting-audit"} {
		t.Run(scenario, func(t *testing.T) {
			cell := t.TempDir()
			path := filepath.Join(cell, "1.process.json")
			raw := []byte(`{"status":"active"}`)
			if scenario == "oversized" {
				raw = []byte(strings.Repeat("x", 65537))
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "conflicting-audit" {
				if err := os.WriteFile(path+".unresolved.json", []byte(`{"original_bytes":"b3RoZXI="}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := launch.AcknowledgeProcessCleanup(cell, 1); err == nil {
				t.Fatal("unsafe evidence replacement allowed")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(raw) {
				t.Fatal("unresolved evidence changed on failure")
			}
		})
	}
}

func TestLeaderExitConfirmation(t *testing.T) {
	t.Run("transient visibility", func(t *testing.T) {
		calls := 0
		err := launch.WaitProcessGone(func() error {
			calls++
			if calls < 3 {
				return nil
			}
			return syscall.ESRCH
		}, time.Second)
		if err != nil || calls != 3 {
			t.Fatalf("did not wait for confirmed exit: calls=%d err=%v", calls, err)
		}
	})
	t.Run("transient permission error", func(t *testing.T) {
		calls := 0
		err := launch.WaitProcessGone(func() error {
			calls++
			if calls < 3 {
				return syscall.EPERM
			}
			return syscall.ESRCH
		}, time.Second)
		if err != nil || calls != 3 {
			t.Fatalf("did not await confirmed exit after permission error: calls=%d err=%v", calls, err)
		}
	})
	t.Run("visible fails closed", func(t *testing.T) {
		err := launch.WaitProcessGone(func() error { return nil }, time.Millisecond)
		if !errors.Is(err, launch.ErrProcessCleanup) {
			t.Fatalf("live leader authorized cleanup: %v", err)
		}
	})
	t.Run("permission error fails closed", func(t *testing.T) {
		err := launch.WaitProcessGone(func() error { return syscall.EPERM }, time.Millisecond)
		if !errors.Is(err, launch.ErrProcessCleanup) {
			t.Fatalf("unknown leader authorized cleanup: %v", err)
		}
	})
}
