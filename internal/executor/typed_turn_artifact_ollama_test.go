package executor

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTypedInputProjectionAndPoisonedDependency(t *testing.T) {
	c := model.ArtifactContract{Type: "string", MaxBytes: 64}
	producer := model.Task{ID: "p", Requires: []string{"public", "secret"}, ArtifactContracts: map[string]model.ArtifactContract{"public": c, "secret": c}}
	consumer := model.Task{ID: "c", DependsOn: []string{"p"}, InputFields: map[string][]string{"p": {"public"}}}
	s := &model.State{Plan: model.Plan{APIVersion: launch.ArtifactPlanVersion}, Tasks: map[string]*model.TaskState{"p": {Spec: producer, Status: "verified", Result: map[string]string{"public": `"safe"`, "secret": `"PRIVATE-CANARY"`}}}}
	inputs, e := managed.InputsForTask(s, consumer)
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(inputs)
	if strings.Contains(string(encoded), "PRIVATE-CANARY") || len(inputs["p"]) != 1 {
		t.Fatalf("projection leaked private artifact: %s", encoded)
	}
	s.Tasks["p"].Result["public"] = `{"wrong":"type"}`
	if _, e = managed.InputsForTask(s, consumer); e == nil {
		t.Fatal("tampered verified typed dependency accepted")
	}
}

func TestArtifactParserResourceBoundaries(t *testing.T) {
	c := model.ArtifactContract{Type: "integer", MaxBytes: 1048576}
	for _, raw := range []string{"1e1000000000", "1e-1000000000", strings.Repeat("[", 129) + "1" + strings.Repeat("]", 129), strings.Repeat("1", 1048577)} {
		if _, e := artifactstore.ArtifactValue(c, raw); e == nil {
			t.Fatal("oversized or depth/exponent unbounded artifact accepted")
		}
	}
	object := model.ArtifactContract{Type: "object", MaxBytes: 128, Properties: map[string]model.ArtifactContract{"public": {Type: "string", MaxBytes: 16}}}
	if _, e := artifactstore.ArtifactValue(object, `{"public":"ok","private":"hidden"}`); e == nil {
		t.Fatal("undeclared nested artifact accepted")
	}
}

func TestManagedOllamaStrictEndpointAndCompletion(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:11434", "http://127.0.0.1:011434", "http://127.0.0.1:11434/", "http://127.0.0.1:11434?", "http://127.0.0.1:11434#", "http://user@127.0.0.1:11434", "http://127.0.0.2:11434", "http://[::ffff:127.0.0.1]:11434"} {
		if launch.ValidateOllamaEndpoint(endpoint) == nil {
			t.Errorf("endpoint accepted: %s", endpoint)
		}
	}
	task := model.Task{Requires: []string{"answer"}}
	good := `{"model":"pinned","done":true,"done_reason":"stop","response":"{\"answer\":\"ok\"}","prompt_eval_count":1,"eval_count":2}`
	if _, _, e := managed.ParseManagedOllama([]byte(good), task, "pinned"); e != nil {
		t.Fatal(e)
	}
	for _, raw := range []string{strings.Replace(good, `"done":true`, `"done":true,"done":false`, 1), strings.Replace(good, `"done_reason":"stop"`, `"done_reason":"length"`, 1), strings.Replace(good, `"eval_count":2`, `"eval_count":-1`, 1), strings.Replace(good, `"done":true`, `"done":true,"remote_host":"external"`, 1), strings.Replace(good, `"done":true`, `"done":true,"tool_calls":[{}]`, 1), strings.Replace(good, `"model":"pinned"`, `"model":"other"`, 1)} {
		if _, _, e := managed.ParseManagedOllama([]byte(raw), task, "pinned"); e == nil {
			t.Fatalf("unsafe completion accepted: %s", raw)
		}
	}
}

func TestTypedAsyncHelperAdmission(t *testing.T) {
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	main := runfixture.Task("a", 0)
	main.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
	shape := model.TurnShape{Roots: 1, MainTurns: 1, HelpersPerTurn: 1}
	plan := model.Plan{APIVersion: launch.ArtifactPlanVersion, Name: "typed async helper review", TurnShape: &shape, MaxTasks: 2, Tasks: []model.Task{main}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, CostPolicy: "retain_unknown", RunBudgetUSD: "0"}}
	root := filepath.Join(t.TempDir(), "run")
	if e := checkpoint.Initialize(plan, root, "managed"); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.SetControl(root, "run"); e != nil {
		t.Fatal(e)
	}
	a, e := (CheckpointCells{Root: root}).BeginAttempt("a", "", 123)
	if e != nil {
		t.Fatal(e)
	}
	child := runfixture.Task("h", 0)
	child.ArtifactContracts = map[string]model.ArtifactContract{"answer": {Type: "boolean", MaxBytes: 5}}
	child.MockResult = map[string]string{"answer": "true"}
	if e = graph.AdmitTurnHelpers(root, "a", a.Fence.OperationID, a.Fence.Attempt, graph.TurnHelperRequest{ID: "typed", Helpers: []model.Task{child}}); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, root)
	helper := s.Tasks["a__turn1__helper_h"]
	if helper == nil || helper.Spec.Turn.Async == nil || !reflect.DeepEqual(helper.Spec.ArtifactContracts, child.ArtifactContracts) {
		t.Fatal("async helper contract lost")
	}
	if e = artifactstore.Accept(helper.Spec, map[string]string{"answer": `"true"`}); e == nil {
		t.Fatal("async helper accepted wrong typed result")
	}
}
