package runops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestManagedReservationsLastDollarAtomic(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "claude_cli", "0.25", 2)
	cells := executor.CheckpointCells{Root: root}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan executor.AdmittedCellAttempt, 2)
	for _, id := range []string{"a", "b"} {
		go func(id string) {
			defer wg.Done()
			lease, e := checkpoint.FileLock(filepath.Join(root, "cells", id, "lease"), true)
			if e != nil {
				t.Error(e)
				return
			}
			defer checkpoint.Unlock(lease)
			a, e := cells.BeginAttempt(id, "", 123)
			if e == nil {
				results <- a
			} else if !errors.Is(e, launch.ErrNoWork) {
				t.Error(e)
			}
		}(id)
	}
	wg.Wait()
	close(results)
	winners := []executor.AdmittedCellAttempt{}
	for a := range results {
		winners = append(winners, a)
	}
	if len(winners) != 1 {
		t.Fatalf("admitted %d calls with one reservation available", len(winners))
	}
	s := runfixture.Read(t, root)
	if len(s.Execution.Reservations) != 1 {
		t.Fatal("duplicate reservation")
	}
	used, e := checkpoint.ManagedBudgetUsed(s.Execution)
	if e != nil || used != 250000 {
		t.Fatalf("used %d %v", used, e)
	}
	a := winners[0]
	if e := cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Err: model.MarkNotStarted(errors.New("positive preflight failure"))}); e != nil {
		t.Fatal(e)
	}
	s = runfixture.Read(t, root)
	used, _ = checkpoint.ManagedBudgetUsed(s.Execution)
	if used != 0 {
		t.Fatal("confirmed not-started did not release reservation")
	}
	other := "a"
	if a.Fence.TaskID == "a" {
		other = "b"
	}
	if _, e := cells.BeginAttempt(other, "", 123); e != nil {
		t.Fatal(e)
	}
}

func TestManagedUnknownHoldAndOvershoot(t *testing.T) {
	for _, mode := range []string{"unknown", "cleanup", "contradictory", "overshoot"} {
		t.Run(mode, func(t *testing.T) {
			root := runfixture.ManagedStateFixture(t, "claude_cli", "0.25", 2)
			cells := executor.CheckpointCells{Root: root}
			a, e := cells.BeginAttempt("a", "", 123)
			if e != nil {
				t.Fatal(e)
			}
			o := managed.CellOutcome{Fence: a.Fence, Result: map[string]string{"answer": "ok"}}
			switch mode {
			case "unknown":
				o.Err = model.WithOutcome(model.OutcomeUncertain, errors.New("response lost"))
			case "cleanup":
				o.CleanupUnresolved = true
			case "contradictory":
				o.Cost = .01
				o.Err = model.MarkNotStarted(errors.New("contradictory"))
			case "overshoot":
				o.Cost = .3
			}
			if e := cells.RecordOutcome(o); e != nil {
				t.Fatal(e)
			}
			s := runfixture.Read(t, root)
			r := s.Execution.Reservations[a.Fence.OperationID]
			used, _ := checkpoint.ManagedBudgetUsed(s.Execution)
			if mode == "overshoot" {
				if !s.Execution.Frozen || used != 300000 || r.Status != "settled" {
					t.Fatal("overshoot not retained/frozen")
				}
			} else if r.Status != "uncertain" || used != 250000 {
				t.Fatal("unknown exposure released")
			}
			before, _ := json.Marshal(s)
			if e := cells.RecordOutcome(o); !errors.Is(e, launch.ErrNoWork) {
				t.Fatalf("duplicate settlement: %v", e)
			}
			after, _ := json.Marshal(runfixture.Read(t, root))
			if string(before) != string(after) {
				t.Fatal("duplicate settlement mutated checkpoint")
			}
			if _, e := cells.BeginAttempt("b", "", 123); !errors.Is(e, launch.ErrNoWork) {
				t.Fatalf("dispatched beyond held/overspent ceiling: %v", e)
			}
		})
	}
}

func TestManagedCrashHoldsAndStaleBinding(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "claude_cli", "0.25", 1)
	cells := executor.CheckpointCells{Root: root}
	a, e := cells.BeginAttempt("a", "", 123)
	if e != nil {
		t.Fatal(e)
	}
	if e := Reconcile(root); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "uncertain" || s.Execution.Reservations[a.Fence.OperationID].Status != "uncertain" {
		t.Fatal("lost worker did not retain unknown hold")
	}
	s.Tasks["a"].BindingDigest = "tampered"
	if _, e := checkpoint.ManagedBinding(s, s.Tasks["a"]); e == nil {
		t.Fatal("stale binding accepted")
	}
}

func TestManagedMockWorkerAndLegacyVersionGate(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "mock", "0", 1)
	if e := worker(root, "a", model.Options{Timeout: 1e9}); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, root)
	if s.Version != 2 || s.Tasks["a"].Status != "verified" || s.Tasks["a"].BindingDigest == "" {
		t.Fatalf("managed mock did not finish: %+v", s.Tasks["a"])
	}
	used, _ := checkpoint.ManagedBudgetUsed(s.Execution)
	if used != 0 {
		t.Fatal("mock charged")
	}
	s.Version = 1
	if e := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); e != nil {
		t.Fatal(e)
	}
	if _, e := checkpoint.ReadState(root); e == nil {
		t.Fatal("managed checkpoint accepted legacyversion")
	}
	legacy := runfixture.New(t, runfixture.Task("legacy", 0))
	if runfixture.Read(t, legacy).Version != 1 {
		t.Fatal("legacy checkpoint changed")
	}
}

func TestManagedSettledFailureReleasesOnlyReportedCost(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "claude_cli", "0.25", 1)
	cells := executor.CheckpointCells{Root: root}
	a, e := cells.BeginAttempt("a", "", 1)
	if e != nil {
		t.Fatal(e)
	}
	if e := cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: .01, Err: model.WithOutcome(model.OutcomeSettled, context.DeadlineExceeded)}); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, root)
	used, _ := checkpoint.ManagedBudgetUsed(s.Execution)
	if s.Tasks["a"].Status != "failed" || used != 10000 {
		t.Fatalf("settled failure treated as unknown: %+v used%d", s.Tasks["a"], used)
	}
	if _, e := os.Stat(filepath.Join(root, "state.json")); e != nil {
		t.Fatal(e)
	}
}

func TestManagedValidationRestoredExposureFreezesAdmission(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "claude_cli", "0.26", 2)
	store := executor.CheckpointCells{Root: root}
	a, err := store.BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: .01, Result: map[string]string{"answer": "kept"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginAttempt("b", "", 124); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordValidation(executor.CellValidation{Fence: a.Fence, CleanupUnresolved: true}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	receipt := s.Execution.Reservations[a.Fence.OperationID]
	if err != nil || used != 500000 || !s.Execution.Frozen || receipt.ReportedMicros != 10000 || receipt.Status != "uncertain" {
		t.Fatalf("restored exposure not preserved/frozen: %+v used%d %v", s.Execution, used, err)
	}
}

func TestManagedRetryHeldOperationNeedsExplicitUncertainty(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "claude_cli", "0.50", 1)
	a, err := (executor.CheckpointCells{Root: root}).BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := retryNode(root, "a", false, false); err == nil {
		t.Fatal("unreconciled held paid operation retried without uncertainty decision")
	}
	after, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected retry changed checkpoint")
	}
	if err := retryNode(root, "a", false, true, true); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.Tasks["a"].OperationID == a.Fence.OperationID || s.Execution.Reservations[a.Fence.OperationID].Status != "uncertain" {
		t.Fatal("explicit retry erased original exposure")
	}
	used, _ := checkpoint.ManagedBudgetUsed(s.Execution)
	if used != 250000 {
		t.Fatal("retry released unknown spend")
	}
}
