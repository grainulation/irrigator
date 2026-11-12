package managed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestFormationRealLoaderAndMockDelivery(t *testing.T) {
	template, in := runfixture.FormationFixture(t)
	before, _ := json.Marshal(template)
	proposal, err := runfixture.CompilePackageUpgrade(template, in)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(proposal)
	file := filepath.Join(t.TempDir(), "plan.json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := checkpoint.LoadPlan(file)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Execution, template.Execution) {
		t.Fatal("execution configuration changed")
	}
	after, _ := json.Marshal(template)
	if string(before) != string(after) {
		t.Fatal("compiler mutated template")
	}
	in.Packages = []string{"packages/a", "packages/b"}
	reordered, err := runfixture.CompilePackageUpgrade(template, in)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(reordered)
	if string(raw) != string(again) {
		t.Fatal("package order changed proposal")
	}
	state := &model.State{Tasks: map[string]*model.TaskState{}}
	dir := t.TempDir()
	for _, task := range loaded.Tasks[:2] {
		b, err := launch.ResolveExecutionCatalog(loaded.Execution.Catalog, "inline", task.Profile)
		if err != nil {
			t.Fatal(err)
		}
		task.MockResult = map[string]string{"candidate_tree_oid": `"` + strings.Repeat("b", 40) + `"`}
		response, err := managedWork(context.Background(), dir, filepath.Join(dir, task.ID), model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "task", RequestID: task.ID, RunID: "formation-fixture", OperationID: task.ID, Attempt: 1, TimeoutMS: 1000, Task: task}, b)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := artifactstore.ArtifactValue(task.ArtifactContracts["candidate_tree_oid"], response.Output["candidate_tree_oid"]); err != nil {
			t.Fatal(err)
		}
		response.Output["private_debug"] = "must not reach integration"
		state.Tasks[task.ID] = &model.TaskState{Status: "verified", Result: response.Output}
	}
	integration := loaded.Tasks[2]
	selected, err := InputsForTask(state, integration)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 {
		t.Fatal("missing dependency results")
	}
	for _, id := range integration.DependsOn {
		if len(selected[id]) != 1 || selected[id]["candidate_tree_oid"] == "" {
			t.Fatal("incorrect upstream selection")
		}
	}
	binding, err := launch.ResolveExecutionCatalog(loaded.Execution.Catalog, "inline", integration.Profile)
	if err != nil {
		t.Fatal(err)
	}
	integration.MockResult = map[string]string{"review_candidate_oid": `"` + strings.Repeat("c", 40) + `"`}
	prefix := filepath.Join(dir, "integration")
	result, err := managedWork(context.Background(), dir, prefix, model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "task", RunID: "formation-fixture", OperationID: "integrate", RequestID: "integrate", Attempt: 1, TimeoutMS: 1000, Task: integration, Dependencies: selected}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifactstore.ArtifactValue(integration.ArtifactContracts["review_candidate_oid"], result.Output["review_candidate_oid"]); err != nil {
		t.Fatal(err)
	}
	captured, err := os.ReadFile(prefix + ".input.json")
	if err != nil {
		t.Fatal(err)
	}
	var delivered model.WorkRequest
	if err := json.Unmarshal(captured, &delivered); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(delivered.Dependencies, selected) {
		t.Fatal("integration did not receive selected worker artifacts")
	}
	state.Tasks[integration.DependsOn[0]].Status = "failed"
	if _, err := InputsForTask(state, integration); err != launch.ErrNoWork {
		t.Fatalf("failed dependency did not block integration: %v", err)
	}
}
func TestFormationRejectsUnsupportedBindingsAndScopes(t *testing.T) {
	for _, kind := range []string{"missing_profile", "missing_execution", "overlap", "escape", "empty", "bad_oid"} {
		t.Run(kind, func(t *testing.T) {
			p, in := runfixture.FormationFixture(t)
			switch kind {
			case "missing_profile":
				in.WorkerProfile = "invented"
			case "missing_execution":
				p.Execution = nil
			case "overlap":
				in.Packages = []string{"packages/a", "packages/a/nested"}
			case "escape":
				in.Packages = []string{"../outside"}
			case "empty":
				in.Packages = nil
			case "bad_oid":
				in.BaseCommit = "HEAD"
			}
			if _, err := runfixture.CompilePackageUpgrade(p, in); err == nil {
				t.Fatal("unsafe proposal accepted")
			}
		})
	}
}
