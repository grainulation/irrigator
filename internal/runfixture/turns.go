package runfixture

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func LcRow(id, parent, status, validation string, deps []string, children ...string) *model.TaskState {
	return &model.TaskState{Spec: model.Task{ID: id, Parent: parent, DependsOn: deps}, Status: status, Validation: validation, Generation: 1,
		OperationID: model.OperationID("run-lc", id, 1), Children: children}
}

func LcState(rows ...*model.TaskState) *model.State {
	s := &model.State{Version: 1, RunID: "run-lc", Tasks: map[string]*model.TaskState{}, ActiveTasks: []model.Task{}}
	for _, r := range rows {
		s.Tasks[r.Spec.ID] = r
		s.ActiveTasks = append(s.ActiveTasks, r.Spec)
	}
	return s
}

func TurnRequestTask(t *testing.T, id string, req checkpoint.TurnRequest) model.Task {
	t.Helper()
	b, e := json.Marshal(req)
	if e != nil {
		t.Fatal(e)
	}
	return model.Task{ID: id, Prompt: "execute one turn", Requires: []string{"answer", artifactstore.TurnRequestField}, MockResult: map[string]string{"answer": "ok", artifactstore.TurnRequestField: string(b)}}
}

func SpawnTask(t *testing.T, id string, children ...model.Task) model.Task {
	t.Helper()
	b, err := json.Marshal(struct {
		Tasks []model.Task `json:"tasks"`
	}{children})
	if err != nil {
		t.Fatal(err)
	}
	return model.Task{ID: id, Prompt: "spawn bounded work", Requires: []string{"graph"}, Spawn: &model.SpawnPolicy{Field: "graph", MaxChildren: 3}, MockResult: map[string]string{"graph": string(b)}}
}

func StageExpansion(t *testing.T, root, id string) {
	t.Helper()
	if err := checkpoint.Update(root, func(s *model.State) error {
		r := s.Tasks[id]
		r.Status = "produced"
		r.Result = r.Spec.MockResult
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func RunningHelperFixture(t *testing.T) string {
	root := TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 2}, Task("a", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "run"
		s.Tasks["a"].Status = "running"
		s.Tasks["a"].Attempt = 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func TurnFixture(t *testing.T, shape model.TurnShape, mains ...model.Task) string {
	t.Helper()
	r, e := model.ReportTurnShape(shape)
	if e != nil {
		t.Fatal(e)
	}
	for i := range mains {
		mains[i].Turn = &model.TurnIdentity{AgentID: mains[i].ID, Role: "main", Index: 1}
	}
	p := model.Plan{APIVersion: checkpoint.TurnPlanVersion, Name: "turn fixture", TurnShape: &shape, MaxTasks: r.MaxExecutions, Tasks: mains}
	root := filepath.Join(t.TempDir(), "run")
	if e := checkpoint.Initialize(p, root, "mock"); e != nil {
		t.Fatal(e)
	}
	return root
}

func RequireCellLeaseReleased(t *testing.T, root, id string) {
	t.Helper()
	lease, err := checkpoint.FileLock(filepath.Join(root, "cells", id, "lease"), true)
	if err != nil {
		t.Fatalf("%s still active after controller returned: %v", id, err)
	}
	checkpoint.Unlock(lease)
}

func V2Run(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	root := filepath.Join(dir, name)
	p := model.Plan{Name: name, Tasks: []model.Task{{ID: "a", Prompt: "SECRET-PROMPT-MARKER", Requires: []string{"o"}}}}
	if err := checkpoint.Initialize(p, root, "mock"); err != nil {
		t.Fatal(err)
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, s.RunID
}

func RevProposal(id string, base int, tasks ...model.Task) model.RevisionProposal {
	return model.RevisionProposal{ID: id, BaseRevision: base, Tasks: tasks, Reason: "test", ExpectedSavingsMS: 5000, Confidence: 0.9}
}

func RevRun(t *testing.T, root string, mutate func(*model.State)) {
	t.Helper()
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "run"
		if mutate != nil {
			mutate(s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
