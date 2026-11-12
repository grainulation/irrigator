package runops

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestRetryFailedManagedOwnerKeepsHelperAndHistoricalBudget(t *testing.T) {
	base := runfixture.ManagedStateFixture(t, "claude_cli", "0.50", 1)
	p := runfixture.Read(t, base).Plan
	p.TurnShape.HelpersPerTurn = 1
	p.MaxTasks = 2
	root := filepath.Join(t.TempDir(), "managed-retry")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	cells := executor.CheckpointCells{Root: root}
	owner, err := cells.BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	request := graph.TurnHelperRequest{ID: "original", Helpers: []model.Task{runfixture.Task("h", 0)}}
	if err := graph.AdmitTurnHelpers(root, "a", owner.Fence.OperationID, owner.Fence.Attempt, request); err != nil {
		t.Fatal(err)
	}
	if err := cells.RecordOutcome(managed.CellOutcome{Fence: owner.Fence, Cost: .1, Err: model.WithOutcome(model.OutcomeSettled, errors.New("known provider failure"))}); err != nil {
		t.Fatal(err)
	}
	helperID := "a__turn1__helper_h"
	helper, err := cells.BeginAttempt(helperID, "", 124)
	if err != nil {
		t.Fatal(err)
	}
	if err := cells.RecordOutcome(managed.CellOutcome{Fence: helper.Fence, Cost: .1, Result: map[string]string{"answer": "accepted helper"}}); err != nil {
		t.Fatal(err)
	}
	if err := commitTurnExpansion(root, helperID, "validated"); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root)
	helperBytes, _ := json.Marshal(before.Tasks[helperID])
	originalReservation := before.Execution.Reservations[owner.Fence.OperationID]
	if err := retryNode(root, "a", false, false); err != nil {
		t.Fatal(err)
	}
	current, err := cells.BeginAttempt("a", "", 125)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.AdmitTurnHelpers(root, "a", current.Fence.OperationID, current.Fence.Attempt, graph.TurnHelperRequest{ID: "extra", Helpers: []model.Task{runfixture.Task("extra", 0)}}); err == nil {
		t.Fatal("retry refilled helper allowance")
	}
	if err := graph.AdmitTurnHelpers(root, "a", owner.Fence.OperationID, owner.Fence.Attempt, request); err != nil {
		t.Fatalf("original helper admission lost idempotency: %v", err)
	}
	if err := cells.RecordOutcome(managed.CellOutcome{Fence: current.Fence, Cost: .25, Err: model.WithOutcome(model.OutcomeSettled, errors.New("second known failure"))}); err != nil {
		t.Fatal(err)
	}
	if err := retryNode(root, "a", false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := cells.BeginAttempt("a", "", 126); !errors.Is(err, launch.ErrNoWork) {
		t.Fatalf("retry refilled run budget: %v", err)
	}
	after := runfixture.Read(t, root)
	afterHelper, _ := json.Marshal(after.Tasks[helperID])
	used, err := checkpoint.ManagedBudgetUsed(after.Execution)
	if err != nil || used != 450000 || len(after.Execution.Reservations) != 3 || after.Execution.Reservations[owner.Fence.OperationID] != originalReservation {
		t.Fatalf("lost historical charge/reservation: %d %v", used, err)
	}
	if string(helperBytes) != string(afterHelper) || len(after.Tasks) != 2 || after.PlanHash != before.PlanHash || after.Tasks["a"].Attempt != 2 {
		t.Fatal("retry changed accepted helper or duplicated work")
	}
}
