//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/admission"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// admissionConfig is a config that only honors the operator override file, so the
// tests control admission without touching the host's battery or network.
func admissionConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admission.json")
	body := `{"schema":"irrigator.admission/v1","preset":"test","exit_dwell_seconds":0.3,"ramp_throttle_seconds":0.1,"override":true}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func admissionStatus(root string) (admission.Status, bool) {
	raw, err := os.ReadFile(filepath.Join(root, admission.StatusFile))
	if err != nil {
		return admission.Status{}, false
	}
	var st admission.Status
	return st, json.Unmarshal(raw, &st) == nil
}

func waitAdmission(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestAdmissionTransientHoldKeepsTheRunAliveAndResumesWhenItClears(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0))
	override := filepath.Join(root, admission.OverrideFile)
	if err := os.WriteFile(override, []byte(`{"hold":"transient","reason":"integration test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, root, "--admission", admissionConfig(t))
	waitAdmission(t, "the status file to report the hold", 30*time.Second, func() bool {
		st, ok := admissionStatus(root)
		return ok && st.Envelope.Hold == admission.HoldTransient
	})
	st, _ := admissionStatus(root)
	if len(st.Envelope.Bindings) == 0 || st.Envelope.Bindings[0].Sensor != "override" {
		t.Fatalf("the status should name the override as the binding sensor: %+v", st.Envelope.Bindings)
	}
	// Held with work waiting is not "blocked": the controller must stay alive and
	// start nothing, for as long as the hold lasts.
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("the controller exited during a transient hold: %v", err)
		default:
		}
		s := runfixture.Read(t, root)
		if s.Control != "run" {
			t.Fatalf("control = %q during a transient hold", s.Control)
		}
		for id, row := range s.Tasks {
			if row.Attempt != 0 || row.Status != "pending" {
				t.Fatalf("task %s started during a hold: %+v", id, row)
			}
		}
	}
	if err := os.Remove(override); err != nil {
		t.Fatal(err)
	}
	clitest.Finished(t, done, true)
	for id, row := range runfixture.Read(t, root).Tasks {
		if row.Status != "verified" {
			t.Fatalf("task %s is %s after the hold cleared", id, row.Status)
		}
	}
}

func TestAdmissionPermanentHoldBlocksTheRun(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := os.WriteFile(filepath.Join(root, admission.OverrideFile), []byte(`{"hold":"permanent"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, root, "--admission", admissionConfig(t))
	clitest.Finished(t, done, false)
	if got := runfixture.Read(t, root).Control; got != "blocked" {
		t.Fatalf("control = %q after a permanent hold, want blocked", got)
	}
}

func TestAdmissionLocalLimitRunsTasksOneAtATime(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .3), runfixture.Task("b", .3), runfixture.Task("c", .3))
	if err := os.WriteFile(filepath.Join(root, admission.OverrideFile), []byte(`{"max_local_slots":1,"reason":"one at a time"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, root, "--admission", admissionConfig(t), "--workers", "3")
	maxRunning := 0
	finished := make(chan error, 1)
	go func() { finished <- <-done }()
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(15 * time.Millisecond) {
		running := 0
		for _, row := range runfixture.Read(t, root).Tasks {
			if row.Status == "running" {
				running++
			}
		}
		if running > maxRunning {
			maxRunning = running
		}
		select {
		case err := <-finished:
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if maxRunning != 1 {
				t.Fatalf("at most %d tasks ran at once, want exactly 1 under a local limit of 1", maxRunning)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the run did not finish")
		}
	}
}

func TestAdmissionUnboundedLeavesNoTraceAndChangesNothing(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0))
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, true)
	for _, name := range []string{admission.ConfigFile, admission.StatusFile} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			t.Fatalf("a run without --admission wrote %s", name)
		}
	}
}
