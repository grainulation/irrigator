//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runops"
)

func TestDownPreservesProducedCheckpoint(t *testing.T) {
	root := fixture(t, task("a", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "down"
		s.Tasks["a"].Status = "produced"
		s.Tasks["a"].Result = map[string]string{"answer": "saved"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := runops.Reconcile(root); err != nil {
		t.Fatal(err)
	}
	if read(t, root).Tasks["a"].Status != "produced" {
		t.Fatal("down should preserve response for resume")
	}
	_, done := start(t, root)
	finished(t, done, true)
	row := read(t, root).Tasks["a"]
	if row.Status != "verified" || row.Attempt != 0 {
		t.Fatalf("response replayed: %#v", row)
	}
}
func TestDownCannotResumeUntilLeaseDrains(t *testing.T) {
	root := fixture(t, task("a", 0))
	if err := checkpoint.SetControl(root, "down"); err != nil {
		t.Fatal(err)
	}
	lease, err := checkpoint.FileLock(filepath.Join(root, "cells", "a", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lease)
	_, err = runops.Run(root, model.Options{Workers: 1})
	if err == nil || !strings.Contains(err.Error(), "draining") {
		t.Fatalf("expected draining refusal: %v", err)
	}
	if read(t, root).Control != "down" {
		t.Fatal("resume cleared sticky down")
	}
}
func TestGCConflictingFlags(t *testing.T) {
	code, err := runCommand([]string{"gc", "unused", "--apply", "--dry-run"})
	if code != 2 || err == nil {
		t.Fatalf("unsafe flags accepted: %d %v", code, err)
	}
}

func TestUnconfirmedSupervisorCleanupBlocksSafeReplay(t *testing.T) {
	for _, marker := range []string{`{"status":"active"}`, `{"broken":`} {
		t.Run(marker, func(t *testing.T) {
			root := fixture(t, task("a", .01))
			original := read(t, root).Tasks["a"].OperationID
			if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "running"; s.Tasks["a"].Attempt = 1; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "cells", "a", "1.process.json"), []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runops.Reconcile(root); err != nil {
				t.Fatal(err)
			}
			row := read(t, root).Tasks["a"]
			if row.Status != "uncertain" || row.OperationID != original {
				t.Fatalf("unsafe recovery: %#v", row)
			}
			_, done := start(t, root, "--retry-failed")
			finished(t, done, false)
			if read(t, root).Tasks["a"].Attempt != 1 {
				t.Fatal("automatically replayed unconfirmed process")
			}
		})
	}
}
