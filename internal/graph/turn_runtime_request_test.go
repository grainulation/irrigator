package graph

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Request validation cases for the turn runtime.
func TestTurnRuntimeRejectsAmbiguousRequest(t *testing.T) {
	for _, source := range []string{`{"helpers":[],"helpers":[]}`, `{"continue":null,"Continue":null}`, `null`} {
		rootTask := runfixture.Task("a", 0)
		rootTask.Requires = []string{"answer", artifactstore.TurnRequestField}
		rootTask.MockResult = map[string]string{"answer": "ok", artifactstore.TurnRequestField: source}
		root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, rootTask)
		runfixture.StageExpansion(t, root, "a")
		s := runfixture.Read(t, root)
		if _, _, err := turnExpansion(s, s.Tasks["a"]); err == nil {
			t.Errorf("ambiguous/non-object request accepted: %s", source)
		}
	}
}
func TestTurnRuntimeRejectsHelperStableIdentityCollision(t *testing.T) {
	rootTask := runfixture.TurnRequestTask(t, "first", checkpoint.TurnRequest{Helpers: []model.Task{runfixture.Task("h", 0)}})
	rootTask.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
	other := runfixture.Task("second", 0)
	other.Turn = &model.TurnIdentity{AgentID: "a__turn1__helper_h", Role: "main", Index: 1}
	shape := model.TurnShape{Roots: 2, MainTurns: 1, HelpersPerTurn: 1}
	p := model.Plan{APIVersion: checkpoint.TurnPlanVersion, Name: "identity collision", TurnShape: &shape, MaxTasks: 4, Tasks: []model.Task{rootTask, other}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(p, root, "mock"); err != nil {
		return
	} // Rejecting reserved initial names also closes this case.
	runfixture.StageExpansion(t, root, "first")
	s := runfixture.Read(t, root)
	if _, _, err := turnExpansion(s, s.Tasks["first"]); err == nil {
		t.Fatal("helper shares stable identity with another main agent")
	}
}
func TestTurnRuntimeRejectsCapabilityEscalationAtomically(t *testing.T) {
	mutations := map[string]func(*model.Task){
		"provider": func(x *model.Task) { x.Provider = "haiku" },
		"context":  func(x *model.Task) { x.Context = []string{"secret"} },
		"validator": func(x *model.Task) {
			x.Checks = []model.Check{{Kind: "command", Command: []string{"sh", "-c", "exit 0"}}}
		},
		"output":     func(x *model.Task) { x.Outputs = map[string]string{"answer": "../escape"} },
		"dependency": func(x *model.Task) { x.DependsOn = []string{"other"} },
		"parent":     func(x *model.Task) { x.Parent = "other" },
		"spawn":      func(x *model.Task) { x.Spawn = &model.SpawnPolicy{Field: "answer", MaxChildren: 1} },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			child := runfixture.Task("h", 0)
			mutate(&child)
			root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Helpers: []model.Task{child}}))
			runfixture.StageExpansion(t, root, "a")
			before := runfixture.Read(t, root)
			rawBefore, _ := json.Marshal(before)
			if err := commitTurnExpansion(root, "a", "structural"); err == nil {
				t.Fatal("capability escalation admitted")
			}
			rawAfter, _ := json.Marshal(runfixture.Read(t, root))
			if string(rawBefore) != string(rawAfter) {
				t.Fatal("rejected expansion partially modified checkpoint")
			}
		})
	}
}
