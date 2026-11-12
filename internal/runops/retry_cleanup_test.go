package runops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestRetryRequiresConfirmedProviderCleanup(t *testing.T) {
	for _, marker := range []string{`{"status":"active","supervisor_pid":99999999}`, `broken marker`} {
		t.Run(marker, func(t *testing.T) {
			root := runfixture.New(t, runfixture.Task("a", 0))
			if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "uncertain"; s.Tasks["a"].Attempt = 1; return nil }); err != nil {
				t.Fatal(err)
			}
			before := runfixture.Read(t, root).Tasks["a"]
			if err := retryNode(root, "a", false, false, true); err == nil || !strings.Contains(err.Error(), "--allow-uncertain") {
				t.Fatalf("cleanup confirmation accepted as settlement: %v", err)
			}
			path := filepath.Join(root, "cells", "a", "1.process.json")
			if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			if err := retryNode(root, "a", false, true); err == nil || !strings.Contains(err.Error(), "--confirm-stopped") {
				t.Fatalf("missing cleanup gate: %v", err)
			}
			blocked := runfixture.Read(t, root).Tasks["a"]
			if blocked.Status != "uncertain" || blocked.OperationID != before.OperationID || blocked.Generation != before.Generation {
				t.Fatal("blocked retry changed operation")
			}
			if err := retryNode(root, "a", false, true, true); err != nil {
				t.Fatal(err)
			}
			after := runfixture.Read(t, root).Tasks["a"]
			if after.Status != "pending" || after.Generation != before.Generation+1 {
				t.Fatal("confirmed retry did not create new operation")
			}
			if active, err := launch.UnresolvedProcess(filepath.Dir(path), 1); active || err != nil {
				t.Fatalf("cleanup unresolved: %v %v", active, err)
			}
			if _, err := os.Stat(path + ".unresolved.json"); err != nil {
				t.Fatal("missing original marker audit", err)
			}
		})
	}
}

func TestRetryConfirmationDoesNotOverrideLiveLease(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	lease, err := checkpoint.FileLock(filepath.Join(root, "cells", "a", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lease)
	if err := retryNode(root, "a", false, true, true); err == nil || !strings.Contains(err.Error(), "active worker") {
		t.Fatalf("confirmation bypassed active worker: %v", err)
	}
}

func TestRetryMissingExternalMarkerRequiresAttestation(t *testing.T) {
	for _, status := range []string{"uncertain", "running"} {
		t.Run(status, func(t *testing.T) {
			root := runfixture.HaikuRoot(t)
			if err := checkpoint.Update(root, func(s *model.State) error { r := s.Tasks["a"]; r.Status = status; r.Attempt = 1; return nil }); err != nil {
				t.Fatal(err)
			}
			before := runfixture.Read(t, root).Tasks["a"]
			if err := retryNode(root, "a", false, false, true); err == nil || !strings.Contains(err.Error(), "--allow-uncertain") {
				t.Fatalf("cleanup confirmation accepted as settlement: %v", err)
			}
			if err := retryNode(root, "a", false, true); err == nil || !strings.Contains(err.Error(), "--confirm-stopped") {
				t.Fatalf("missing marker accepted as proof of stopped provider: %v", err)
			}
			blocked := runfixture.Read(t, root).Tasks["a"]
			if blocked.Status != status || blocked.Generation != before.Generation || blocked.OperationID != before.OperationID {
				t.Fatal("unconfirmed retry changed dispatch generation")
			}
			if err := retryNode(root, "a", false, true, true); err != nil {
				t.Fatal(err)
			}
			after := runfixture.Read(t, root)
			if after.Tasks["a"].Status != "pending" {
				t.Fatal("confirmed retry not admitted")
			}
			found := false
			for _, event := range after.Events {
				if event.Kind == "process_cleanup_confirmed" {
					found = true
				}
			}
			if !found {
				t.Fatal("user attestation not durable")
			}
			if _, err := os.Stat(filepath.Join(root, "cells", "a", "1.process.json")); !os.IsNotExist(err) {
				t.Fatal("fabricated supervisor marker")
			}
		})
	}
}
