package executor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestManagedAcceptedUnknownCostRetainsExposure(t *testing.T) {
	root := runfixture.UnknownCostFixture(t)
	c := CheckpointCells{Root: root}
	a, err := c.BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
	if err = c.RecordValidation(CellValidation{Fence: a.Fence, Level: "validated"}); err != nil {
		t.Fatal(err)
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	row := s.Tasks["a"]
	r := s.Execution.Reservations[row.OperationID]
	if s.Version != 3 || row.Status != "verified" || !row.CostUnknown || r.Status != "unknown_cost" {
		t.Fatalf("wrong accepted unknown state: %+v %+v", row, r)
	}
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 250000 {
		t.Fatalf("exposure %d %v", used, err)
	}
	if _, err = c.BeginAttempt("b", "", 123); !errors.Is(err, launch.ErrNoWork) {
		t.Fatalf("unknown cost allowed peer: %v", err)
	}
	for _, kind := range []string{"settled", "flag", "version"} {
		t.Run(kind, func(t *testing.T) {
			current, e := checkpoint.ReadState(root)
			if e != nil {
				t.Fatal(e)
			}
			switch kind {
			case "settled":
				receipt := current.Execution.Reservations[row.OperationID]
				receipt.Status = "settled"
				current.Execution.Reservations[row.OperationID] = receipt
			case "flag":
				current.Tasks["a"].CostUnknown = false
			case "version":
				current.Version = 2
			}
			if e = checkpoint.ValidateManagedState(current); e == nil {
				t.Fatal("corrupt accounting accepted")
			}
		})
	}
}

func TestManagedUnknownCostGateAndContradictions(t *testing.T) {
	for _, kind := range []string{"v3", "nonzero", "notstarted"} {
		t.Run(kind, func(t *testing.T) {
			root := runfixture.UnknownCostFixture(t)
			if kind == "v3" {
				root = runfixture.ManagedStateFixture(t, "claude_cli", "0.25", 1)
			}
			c := CheckpointCells{Root: root}
			a, e := c.BeginAttempt("a", "", 123)
			if e != nil {
				t.Fatal(e)
			}
			before, _ := os.ReadFile(filepath.Join(root, "state.json"))
			o := managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}
			if kind == "nonzero" {
				o.Cost = .01
			}
			if kind == "notstarted" {
				o.Err = model.MarkNotStarted(errors.New("not started"))
			}
			if e = c.RecordOutcome(o); e == nil {
				t.Fatal("contradictory unknown outcome accepted")
			}
			after, _ := os.ReadFile(filepath.Join(root, "state.json"))
			if string(before) != string(after) {
				t.Fatal("rejection changed checkpoint")
			}
		})
	}
}
