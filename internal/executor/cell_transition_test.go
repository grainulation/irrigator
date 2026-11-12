package executor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func cellTestState() *model.State {
	t := runfixture.Task("cell", 0)
	s := &model.State{Version: 1, RunID: "cell-test", Plan: model.Plan{Tasks: []model.Task{t}}, Backend: "mock", Control: "run", Tasks: map[string]*model.TaskState{}}
	s.Tasks[t.ID] = s.NewTask(t)
	return s
}
func cellBytes(t *testing.T, s *model.State) string {
	t.Helper()
	b, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func cellTestBegin(t *testing.T, s *model.State) AdmittedCellAttempt {
	t.Helper()
	a, e := beginCellAttempt(s, "cell", s.Tasks["cell"].OperationID, 123, time.Unix(1, 0))
	if e != nil {
		t.Fatal(e)
	}
	return a
}

func TestCellAdmissionRejectsWithoutMutation(t *testing.T) {
	for _, name := range []string{"paused", "cancelled", "accepted", "stale-operation", "unaccepted-input"} {
		t.Run(name, func(t *testing.T) {
			s := cellTestState()
			expected := s.Tasks["cell"].OperationID
			switch name {
			case "paused":
				s.Control = "pause"
			case "cancelled":
				s.Tasks["cell"].CancelRequested = true
			case "accepted":
				s.Tasks["cell"].Status = "verified"
			case "stale-operation":
				expected = "old"
			case "unaccepted-input":
				s.Tasks["cell"].Spec.DependsOn = []string{"absent"}
			}
			before := cellBytes(t, s)
			if _, e := beginCellAttempt(s, "cell", expected, 123, time.Unix(1, 0)); !errors.Is(e, launch.ErrNoWork) {
				t.Fatalf("expected no work, got%v", e)
			}
			if cellBytes(t, s) != before {
				t.Fatal("rejected admission mutated state")
			}
		})
	}
}

func TestCellOutcomeFenceAndOnceOnlyCost(t *testing.T) {
	for _, field := range []string{"run", "operation", "attempt", "binding", "missing"} {
		t.Run(field, func(t *testing.T) {
			s := cellTestState()
			a := cellTestBegin(t, s)
			f := a.Fence
			switch field {
			case "run":
				f.RunID = "other"
			case "operation":
				f.OperationID = "other"
			case "attempt":
				f.Attempt++
			case "binding":
				f.BindingDigest = "other"
			case "missing":
				f.TaskID = "other"
			}
			before := cellBytes(t, s)
			e := recordCellOutcome(s, managed.CellOutcome{Fence: f, Result: map[string]string{"answer": "wrong"}, Cost: 1}, time.Unix(2, 0))
			if !errors.Is(e, managed.ErrStaleCellAttempt) || cellBytes(t, s) != before {
				t.Fatalf("stale outcome changed cell: %v", e)
			}
		})
	}
	s := cellTestState()
	a := cellTestBegin(t, s)
	o := managed.CellOutcome{Fence: a.Fence, Result: map[string]string{"answer": "ok"}, Cost: .25}
	if e := recordCellOutcome(s, o, time.Unix(2, 0)); e != nil {
		t.Fatal(e)
	}
	before := cellBytes(t, s)
	if e := recordCellOutcome(s, o, time.Unix(3, 0)); !errors.Is(e, launch.ErrNoWork) {
		t.Fatalf("duplicate outcome accepted: %v", e)
	}
	if cellBytes(t, s) != before {
		t.Fatal("duplicate charged or mutated state")
	}
}

func TestCellInvalidCostDoesNotMutate(t *testing.T) {
	for _, cost := range []float64{-1, math.NaN(), math.Inf(1)} {
		s := cellTestState()
		a := cellTestBegin(t, s)
		before := cellBytes(t, s)
		if e := recordCellOutcome(s, managed.CellOutcome{Fence: a.Fence, Cost: cost}, time.Unix(2, 0)); e == nil {
			t.Fatal("invalid cost accepted")
		}
		if cellBytes(t, s) != before {
			t.Fatal("invalid cost mutated state")
		}
	}
}

func TestCellOutcomeCertaintyPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		cleanup bool
		want    string
	}{
		{"unknown-before-cancel", model.WithOutcome(model.OutcomeUncertain, errors.New("lost")), false, "uncertain"},
		{"cleanup-before-success", nil, true, "uncertain"},
		{"not-started", model.MarkNotStarted(errors.New("not dispatched")), false, "failed"},
		{"cancelled", context.Canceled, false, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cellTestState()
			a := cellTestBegin(t, s)
			s.Tasks["cell"].CancelRequested = true
			if e := recordCellOutcome(s, managed.CellOutcome{Fence: a.Fence, Err: tc.err, CleanupUnresolved: tc.cleanup}, time.Unix(2, 0)); e != nil {
				t.Fatal(e)
			}
			if s.Tasks["cell"].Status != tc.want || !s.Tasks["cell"].CancelRequested {
				t.Fatalf("certainty/cancel lost: %+v", s.Tasks["cell"])
			}
		})
	}
}

func TestCellValidationPreservesProducedAndRejectsStale(t *testing.T) {
	s := cellTestState()
	a := cellTestBegin(t, s)
	if e := recordCellOutcome(s, managed.CellOutcome{Fence: a.Fence, Result: map[string]string{"answer": "saved"}}, time.Unix(2, 0)); e != nil {
		t.Fatal(e)
	}
	if e := recordCellValidation(s, CellValidation{Fence: a.Fence, Err: context.Canceled}, time.Unix(3, 0)); e != nil {
		t.Fatal(e)
	}
	if s.Tasks["cell"].Status != "produced" || s.Tasks["cell"].Result["answer"] != "saved" {
		t.Fatal("interruption lost candidate")
	}
	s.Tasks["cell"].Attempt++
	before := cellBytes(t, s)
	for _, v := range []CellValidation{{Fence: a.Fence}, {Fence: a.Fence, CleanupUnresolved: true}, {Fence: a.Fence, Err: model.WithOutcome(model.OutcomeUncertain, errors.New("lost"))}} {
		if e := recordCellValidation(s, v, time.Unix(4, 0)); !errors.Is(e, managed.ErrStaleCellAttempt) {
			t.Fatalf("stale validation accepted: %v", e)
		}
		if cellBytes(t, s) != before {
			t.Fatal("stale validation mutated newer attempt")
		}
	}
}

func TestCellStoreWorkerKeepsLegacyCheckpoint(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("cell", .01))
	before := runfixture.Read(t, root)
	if e := checkpoint.SetControl(root, "run"); e != nil {
		t.Fatal(e)
	}
	if e := worker(root, "cell", model.Options{Timeout: time.Second, Budget: .1}); e != nil {
		t.Fatal(e)
	}
	after := runfixture.Read(t, root)
	if after.Version != 1 || after.PlanHash != before.PlanHash || after.Tasks["cell"].Status != "verified" || after.Tasks["cell"].Attempt != 1 {
		t.Fatalf("legacy contract changed: %+v", after.Tasks["cell"])
	}
	if after.Tasks["cell"].Result["answer"] != "mock answer" {
		t.Fatal("worker did not execute")
	}
}
