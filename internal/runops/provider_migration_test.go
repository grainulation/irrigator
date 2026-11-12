package runops

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestProviderMigrationRejectsUnsafeOrStaleApproval(t *testing.T) {
	for _, kind := range []string{"digest", "attestation", "state", "binary", "lease", "controller", "active-marker", "cleanup-error", "fifo-marker"} {
		t.Run(kind, func(t *testing.T) {
			root, _, replacement := runfixture.MigrationFixture(t)
			p, err := provider.PrepareProviderMigration(root, "small", replacement)
			if err != nil {
				t.Fatal(err)
			}
			approve, confirmed := p.Digest, true
			switch kind {
			case "digest":
				approve = strings.Repeat("0", 64)
			case "attestation":
				confirmed = false
			case "state":
				if err := checkpoint.Update(root, func(s *model.State) error { s.Event("changed", "", nil); return nil }); err != nil {
					t.Fatal(err)
				}
			case "binary":
				if err := os.WriteFile(replacement, []byte("#!/bin/sh\nexit 9\n"), 0700); err != nil {
					t.Fatal(err)
				}
			case "lease", "controller":
				path := filepath.Join(root, "controller.lock")
				if kind == "lease" {
					path = filepath.Join(root, "cells", "a", "lease")
				}
				l, err := checkpoint.FileLock(path, true)
				if err != nil {
					t.Fatal(err)
				}
				defer checkpoint.Unlock(l)
			case "active-marker", "cleanup-error", "fifo-marker":
				// A past settled failed attempt with a fresh pending generation.
				cells := executor.CheckpointCells{Root: root}
				if err := checkpoint.SetControl(root, "run"); err != nil {
					t.Fatal(err)
				}
				a, err := cells.BeginAttempt("a", "", 1)
				if err != nil {
					t.Fatal(err)
				}
				if err := cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Result: map[string]string{"answer": "ok"}}); err != nil {
					t.Fatal(err)
				}
				if err := cells.RecordValidation(executor.CellValidation{Fence: a.Fence, Level: "validated"}); err != nil {
					t.Fatal(err)
				}
				if err := checkpoint.SetControl(root, "pause"); err != nil {
					t.Fatal(err)
				}
				p, err = provider.PrepareProviderMigration(root, "small", replacement)
				if err != nil {
					t.Fatal(err)
				}
				approve = p.Digest
				marker := model.ProcessMarker{Status: "active"}
				if kind == "cleanup-error" {
					marker.Status = "stopped"
					marker.CleanupError = "unresolved"
				}
				if kind == "fifo-marker" {
					if err := syscall.Mkfifo(filepath.Join(root, "cells", "a", "1.process.json"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := jsonio.AtomicJSON(filepath.Join(root, "cells", "a", "1.process.json"), marker); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, _ := os.ReadFile(filepath.Join(root, "state.json"))
			if err := provider.ApplyProviderMigration(root, p, approve, confirmed); err == nil {
				t.Fatal("unsafe migration accepted")
			}
			after, _ := os.ReadFile(filepath.Join(root, "state.json"))
			if string(before) != string(after) {
				t.Fatal("rejected migration changed state")
			}
		})
	}
}

func TestProviderMigrationRejectsUnreconciledAndTamperedState(t *testing.T) {
	root, _, replacement := runfixture.MigrationFixture(t)
	p, err := provider.PrepareProviderMigration(root, "small", replacement)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"running", "uncertain", "produced"} {
		s := runfixture.Read(t, root)
		s.Tasks["a"].Status = status
		if _, err := provider.ProviderMigrationEligibility(s, "small"); err == nil {
			t.Fatalf("accepted %s", status)
		}
	}
	if err := provider.ApplyProviderMigration(root, p, p.Digest, true); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"budget", "model", "approval", "origin", "sequence"} {
		s := runfixture.Read(t, root)
		m := &s.Execution.Migrations[0]
		switch kind {
		case "budget":
			m.Proposal.Replacement.OperationBudgetMicros++
		case "model":
			m.Proposal.Replacement.Profile.Model = "other"
		case "approval":
			m.ConfirmedStopped = false
		case "origin":
			m.Proposal.FromBinding = "other"
		case "sequence":
			m.Proposal.Sequence++
		}
		if err := checkpoint.ValidateManagedState(s); err == nil {
			t.Fatalf("accepted mutated %s", kind)
		}
	}
	raw, _ := json.Marshal(p)
	var decoded model.ProviderMigrationProposal
	raw = []byte(strings.Replace(string(raw), `"schema":`, `"schema":"duplicate","schema":`, 1))
	if err := provider.DecodeMigrationProposal(raw, &decoded); err == nil {
		t.Fatal("duplicate proposal key accepted")
	}
}

func TestProviderMigrationRejectsFIFOWithoutBlocking(t *testing.T) {
	root, _, _ := runfixture.MigrationFixture(t)
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := provider.PrepareProviderMigration(root, "small", path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked preparation")
	}
}

func TestProviderMigrationOldCheckpointReadersRejectFeature(t *testing.T) {
	root, _, replacement := runfixture.MigrationFixture(t)
	p, err := provider.PrepareProviderMigration(root, "small", replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyProviderMigration(root, p, p.Digest, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(runfixture.Read(t, root))
	// Older managed readers use exact field checking. Model their execution
	// envelope without the newly introduced feature, rather than a loose decoder.
	var envelope struct {
		Execution json.RawMessage `json:"execution"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	type priorExecution struct {
		BillingRegistry string                              `json:"billing_registry,omitempty"`
		BillingReceipts map[string]model.BillingReceipt     `json:"billing_receipts,omitempty"`
		Bindings        map[string]model.ExecutionBinding   `json:"bindings"`
		CeilingMicros   int64                               `json:"ceiling_micros"`
		Reservations    map[string]model.ManagedReservation `json:"reservations"`
		Frozen          bool                                `json:"frozen"`
		FreezeReason    string                              `json:"freeze_reason,omitempty"`
	}
	var value any
	if err := json.Unmarshal(envelope.Execution, &value); err != nil {
		t.Fatal(err)
	}
	if err := model.ExactTurnJSONFields(value, reflect.TypeOf(priorExecution{})); err == nil || !strings.Contains(err.Error(), "provider_migrations") {
		t.Fatal("old checkpoint contract silently accepted migration", err)
	}
}

func TestProviderMigrationRetryPreservesOldAccounting(t *testing.T) {
	root, _, replacement := runfixture.MigrationFixture(t)
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	cells := executor.CheckpointCells{Root: root}
	a, err := cells.BeginAttempt("a", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: .01, Err: model.WithOutcome(model.OutcomeSettled, errors.New("settled failure"))}); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "pause"); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root)
	oldReceipt := before.Execution.Reservations[a.Fence.OperationID]
	p, err := provider.PrepareProviderMigration(root, "small", replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyProviderMigration(root, p, p.Digest, true); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).Tasks["a"].BindingDigest != before.Tasks["a"].BindingDigest {
		t.Fatal("terminal failed binding rewritten")
	}
	if err := retryNode(root, "a", false, false); err != nil {
		t.Fatal(err)
	}
	after := runfixture.Read(t, root)
	if after.Tasks["a"].BindingDigest != p.Replacement.Digest || after.Tasks["a"].OperationID == a.Fence.OperationID || !reflect.DeepEqual(oldReceipt, after.Execution.Reservations[a.Fence.OperationID]) {
		t.Fatal("retry lost historical accounting or latest binding")
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	after = runfixture.Read(t, root)
	if after.Tasks["a"].Status != "verified" || len(after.Execution.Reservations) != 2 {
		t.Fatal("retry did not retain both attempts")
	}
	if !reflect.DeepEqual(oldReceipt, after.Execution.Reservations[a.Fence.OperationID]) {
		t.Fatal("historical receipt changed")
	}
}

func TestProviderMigrationApprovalChain(t *testing.T) {
	root, _, replacement := runfixture.MigrationFixture(t)
	p, err := provider.PrepareProviderMigration(root, "small", replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.ApplyProviderMigration(root, p, p.Digest, true); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(t.TempDir(), "newer")
	raw, _ := os.ReadFile(replacement)
	if err := os.WriteFile(newer, raw, 0700); err != nil {
		t.Fatal(err)
	}
	p2, err := provider.PrepareProviderMigration(root, "small", newer)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Sequence != 2 || p2.FromBinding != p.Replacement.Digest {
		t.Fatal("migration chain lost predecessor")
	}
	if err := provider.ApplyProviderMigration(root, p2, p2.Digest, true); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if len(s.Execution.Migrations) != 2 || s.Tasks["a"].BindingDigest != p2.Replacement.Digest {
		t.Fatal("migration chain not selected")
	}
	if _, err := checkpoint.ManagedBindingByDigest(s, "small", p.Replacement.Digest); err != nil {
		t.Fatal(err)
	}
}

func TestProviderMigrationIncludesArchivedProcessAttempts(t *testing.T) {
	s := &model.State{Tasks: map[string]*model.TaskState{"a": {Attempt: 2}}, ArchivedTasks: []model.TaskState{{Spec: model.Task{ID: "a"}, Attempt: 1}, {Spec: model.Task{ID: "retired"}, Attempt: 3}}}
	if got := provider.ProviderMigrationAttempts(s); !reflect.DeepEqual(got, map[string]int{"a": 2, "retired": 3}) {
		t.Fatal(got)
	}
}
