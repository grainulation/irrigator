//go:build integration

package cli

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTypedDynamicTurnAndHelper(t *testing.T) {
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	profile := catalog.Profiles["small"]
	profile.Limits = model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}
	catalog.Profiles["small"] = profile
	typed := func(id, value string) model.Task {
		x := task(id, 0)
		x.ArtifactContracts = map[string]model.ArtifactContract{"answer": {Type: "integer", MaxBytes: 8}}
		x.MockResult = map[string]string{"answer": value}
		return x
	}
	child := typed("h", "7")
	next := typed("next", "8")
	first := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Helpers: []model.Task{child}, Continue: &next, WaitForHelpers: true})
	first.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
	shape := model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}
	p := model.Plan{APIVersion: launch.ArtifactPlanVersion, Name: "independent rich dynamic review", TurnShape: &shape, MaxTasks: 4, Tasks: []model.Task{first}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, CostPolicy: "retain_unknown", RunBudgetUSD: "0"}}
	root := filepath.Join(t.TempDir(), "run")
	if e := checkpoint.Initialize(p, root, "managed"); e != nil {
		t.Fatal(e)
	}
	_, done := start(t, root, "--workers", "2")
	finished(t, done, true)
	s := read(t, root)
	for _, id := range []string{"a__turn1__helper_h", "a__turn2"} {
		r := s.Tasks[id]
		if r == nil || r.Status != "verified" || !reflect.DeepEqual(r.Spec.ArtifactContracts, child.ArtifactContracts) {
			t.Fatalf("dynamic contract lost: %s %+v", id, r)
		}
	}
}
