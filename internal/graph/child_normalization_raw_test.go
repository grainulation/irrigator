package graph

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func childNormalizationState(source, effort string) (*model.State, *model.TaskState) {
	p := model.Task{ID: "parent", Prompt: "spawn", Requires: []string{"graph"}, Effort: effort, Spawn: &model.SpawnPolicy{Field: "graph", MaxChildren: 2}}
	row := &model.TaskState{Spec: p, Status: "produced", Result: map[string]string{"graph": source}, Generation: 1, OperationID: "original-operation", Cost: 0.7}
	return &model.State{Plan: model.Plan{Tasks: []model.Task{p}, MaxTasks: 8, MaxDepth: 3}, Backend: "mock", Tasks: map[string]*model.TaskState{"parent": row}}, row
}
func TestChildNormalizationBareArrayPreservesRawIdentity(t *testing.T) {
	raw := `[{"id":"child","prompt":"work","requires":["answer"]}]`
	s, row := childNormalizationState(raw, "high")
	children, hash, err := Expansion(s, row)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].ID != "parent__child" {
		t.Fatalf("bad child mapping: %+v", children)
	}
	if hash != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) || row.Result["graph"] != raw || row.OperationID != "original-operation" || row.Cost != 0.7 {
		t.Fatal("normalization rewrote settled evidence")
	}
	row.ExpansionHash = hash
	again, againHash, err := Expansion(s, row)
	if err != nil || len(again) != 0 || againHash != hash {
		t.Fatal("committed raw expansion not idempotent")
	}
}
func TestChildNormalizationExplicitLowerEffortPreserved(t *testing.T) {
	s, row := childNormalizationState(`{"tasks":[{"id":"child","prompt":"work","requires":["answer"],"effort":"low"}]}`, "high")
	children, _, err := Expansion(s, row)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Effort != "low" {
		t.Fatalf("lower effort silently raised: %+v", children)
	}
}
func TestChildNormalizationDuplicateKeysRejected(t *testing.T) {
	for _, raw := range []string{
		`{"tasks":[],"tasks":[{"id":"child","prompt":"work","requires":["answer"]}]}`,
		`{"tasks":[{"id":"first","id":"second","prompt":"work","requires":["answer"]}]}`,
		`[{"id":"child","prompt":"work","requires":["answer"],"spawn":{"field":"graph","max_children":1,"max_children":2}}]`,
	} {
		s, row := childNormalizationState(raw, "high")
		if _, _, err := Expansion(s, row); err == nil {
			t.Fatalf("ambiguous duplicate accepted: %s", raw)
		}
	}
}
func TestChildNormalizationNormalizationNoAuthorityExpansion(t *testing.T) {
	for _, raw := range []string{
		`[{"id":"child","prompt":"work","requires":["answer"],"effort":"high"}]`,
		`[{"id":"child","prompt":"work","requires":["answer"],"provider":"haiku"}]`,
		`[{"id":"child","prompt":"work","requires":["answer"],"checks":[{"kind":"command","command":["sh","-c","true"]}]}]`,
		`[{"id":"child","prompt":"work","requires":["answer"],"spawn":{"field":"graph","max_children":3}}]`,
		`[{"id":"child","prompt":"work","requires":["answer"],"budget":100}]`,
		`[{"id":"child","prompt":"work","requires":["answer"],"effort":"maximum"}]`,
		`[{"id":"child","prompt":"work","requires":["answer"]}] {"tasks":[]}`,
	} {
		s, row := childNormalizationState(raw, "low")
		if _, _, err := Expansion(s, row); err == nil {
			t.Fatalf("unsafe escalation accepted: %s", raw)
		}
		if len(s.Tasks) != 1 || row.Cost != 0.7 {
			t.Fatal("failed expansion mutated state")
		}
	}
}
