package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestWorkerBoundariesDepthAndAuthority(t *testing.T) {
	root := model.Task{ID: "root", Spawn: &model.SpawnPolicy{Field: "children", MaxChildren: 3}}
	child := model.Task{ID: "root__child", Parent: "root", Spawn: &model.SpawnPolicy{Field: "children", MaxChildren: 3}}
	leaf := model.Task{ID: "root__child__leaf", Parent: child.ID, Spawn: &model.SpawnPolicy{Field: "children", MaxChildren: 3}, Requires: []string{"children"}}
	s := &model.State{Plan: model.Plan{MaxTasks: 5, MaxDepth: 2}, Tasks: map[string]*model.TaskState{root.ID: {Spec: root}, child.ID: {Spec: child}, leaf.ID: {Spec: leaf}}}
	b := workerBoundaries(s, leaf)
	if b.CurrentDepth != 2 || b.RemainingDepth != 0 || b.SpawnChildrenRemaining != 0 || b.TaskSlotsRemaining != 2 {
		t.Fatalf("%+v", b)
	}
	schema := artifactstore.ModelSchemaForRequest(model.WorkRequest{Task: leaf, Boundaries: b})
	tasks := schema["properties"].(map[string]any)["children"].(map[string]any)["properties"].(map[string]any)["tasks"].(map[string]any)
	if tasks["maxItems"] != 0 {
		t.Fatal("terminal node may propose children")
	}
	child.Requires = []string{"children"}
	schema = artifactstore.ModelSchemaForRequest(model.WorkRequest{Task: child, Boundaries: workerBoundaries(s, child)})
	props := schema["$defs"].(map[string]any)["Task"].(map[string]any)["properties"].(map[string]any)
	if props["spawn"].(map[string]any)["type"] != "null" {
		t.Fatal("last-level children not constrained to leaves")
	}
	child.Spawn = nil
	if workerBoundaries(s, child).SpawnChildrenRemaining != 0 {
		t.Fatal("inherited spawn authority")
	}
}

func TestWorkerBoundariesTurnsAndTerminalSchema(t *testing.T) {
	main := model.Task{ID: "a", Requires: []string{"answer", artifactstore.TurnRequestField}, Turn: &model.TurnIdentity{AgentID: "a", Role: "main", Index: 2}}
	s := &model.State{Plan: model.Plan{APIVersion: checkpoint.TurnPlanVersion, MaxTasks: 8, TurnShape: &model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 3}}, Tasks: map[string]*model.TaskState{"a": {Spec: main, Children: []string{"existing"}}, "existing": {}}}
	b := workerBoundaries(s, main)
	if b.RemainingTurns != 0 || b.HelpersRemaining != 2 || b.CanWaitForHelpers || b.Topology != "turns" {
		t.Fatalf("%+v", b)
	}
	schema := artifactstore.ModelSchemaForRequest(model.WorkRequest{Task: main, Boundaries: b})
	turnSchema := schema["properties"].(map[string]any)[artifactstore.TurnRequestField].(map[string]any)
	if turnSchema["type"] != "object" || turnSchema["anyOf"] != nil {
		t.Fatal("final turn must have an unambiguous object schema")
	}
	props := turnSchema["properties"].(map[string]any)
	if _, ok := props["continue"]; ok {
		t.Fatal("final turn permits continuation")
	}
	if props["helpers"].(map[string]any)["maxItems"] != 2 {
		t.Fatal("helper allowance omitted")
	}
	main.Turn.Role = "helper"
	b = workerBoundaries(s, main)
	if b.HelpersRemaining != 0 || b.RemainingTurns != 0 {
		t.Fatal("helper allowed recursive work")
	}
}

func TestWorkerBoundariesCustomWireUnchanged(t *testing.T) {
	task := model.Task{ID: "a"}
	s := &model.State{Backend: "custom", Tasks: map[string]*model.TaskState{"a": {Spec: task, OperationID: "operation"}}}
	raw, _ := json.Marshal(request(s, task, 1, nil, model.Options{}))
	if strings.Contains(string(raw), "execution_boundaries") {
		t.Fatal("legacy custom wire changed")
	}
	s.Backend = "haiku"
	raw, _ = json.Marshal(request(s, task, 1, nil, model.Options{}))
	if !strings.Contains(string(raw), "execution_boundaries") {
		t.Fatal("haiku metadata absent")
	}
}

func TestTurnRequestObjectWirePreservesStringCheckpoint(t *testing.T) {
	task := model.Task{Requires: []string{"answer", artifactstore.TurnRequestField}, Turn: &model.TurnIdentity{Role: "main", Index: 1}}
	output, err := artifactstore.NormalizeModelOutput(task, json.RawMessage(`{"answer":"ok","turn_request":{"helpers":[]}}`))
	if err != nil || output[artifactstore.TurnRequestField] != `{"helpers":[]}` {
		t.Fatalf("%v %v", output, err)
	}
	output, err = artifactstore.NormalizeModelOutput(task, json.RawMessage(`{"answer":"ok","turn_request":""}`))
	if err != nil || output[artifactstore.TurnRequestField] != "" {
		t.Fatalf("stop lost: %v %v", output, err)
	}
}

func TestKnownFinalTurnWaitRejectedBeforeRun(t *testing.T) {
	last := runfixture.TurnRequestTask(t, "last", checkpoint.TurnRequest{WaitForHelpers: true})
	first := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &last})
	first.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
	plan := model.Plan{APIVersion: checkpoint.TurnPlanVersion, MaxTasks: 4, TurnShape: &model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, Tasks: []model.Task{first}}
	if err := checkpoint.ValidateInitialTurns(plan); err == nil || !strings.Contains(err.Error(), "final main turn") {
		t.Fatalf("late wait accepted: %v", err)
	}
	if err := checkpoint.Initialize(plan, filepath.Join(t.TempDir(), "run"), "mock"); err == nil {
		t.Fatal("invalid static wait admitted")
	}
	// Historical plans still parse; only new admission applies static knowledge.
	if err := checkpoint.Validate(plan); err != nil {
		t.Fatalf("historical validation changed: %v", err)
	}
}

func TestWorkerBoundariesManagedWireAndTypedStop(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok","turn_request":{}},"total_cost_usd":0.01}'`)
	req.Task.Requires = []string{"answer", artifactstore.TurnRequestField}
	req.Task.Turn = &model.TurnIdentity{AgentID: "lane", Role: "main", Index: 2}
	req.Boundaries = &model.WorkerBoundaries{Topology: "turns", CurrentTurn: 2, HelpersRemaining: 1, TaskSlotsRemaining: 1, Basis: "admission_snapshot_not_reservation"}
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err != nil || response.Output[artifactstore.TurnRequestField] != "{}" {
		t.Fatalf("typed stop failed: %+v %v", response, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "captured.json"))
	if err != nil || !strings.Contains(string(raw), `"execution_boundaries"`) {
		t.Fatalf("boundary delivery missing: %s %v", raw, err)
	}
	argv, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
	if err != nil || !strings.Contains(string(argv), `"turn_request":{"additionalProperties":false`) {
		t.Fatalf("typed wire schema missing: %s %v", argv, err)
	}
}

func TestWorkerBoundariesHaikuDepthWire(t *testing.T) {
	runfixture.HaikuClaudeStub(t, `cat > captured.json
printf '%s' '{"type":"result","structured_output":{"children":{"tasks":[]}},"total_cost_usd":0}'`)
	dir := t.TempDir()
	task := model.Task{ID: "leaf", Prompt: "finish", Requires: []string{"children"}, Spawn: &model.SpawnPolicy{Field: "children", MaxChildren: 3}}
	bounds := &model.WorkerBoundaries{Topology: "recursive", CurrentDepth: 2, RemainingDepth: 0, TaskSlotsRemaining: 3, Basis: "admission_snapshot_not_reservation"}
	output, _, err := callHaikuBounded(context.Background(), dir, 1, task, nil, .25, bounds)
	if err != nil || output["children"] != `{"tasks":[]}` {
		t.Fatalf("leaf result: %v %v", output, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "captured.json"))
	if err != nil || !strings.Contains(string(raw), `"maxItems":0`) || !strings.Contains(string(raw), `"current_depth":2`) {
		t.Fatalf("depth boundary delivery: %s %v", raw, err)
	}
}

func TestWorkerBoundariesManagedMockWireUnchanged(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "mock", "0", 1)
	s := runfixture.Read(t, root)
	req := request(s, s.Tasks["a"].Spec, 1, nil, model.Options{})
	if req.Boundaries != nil {
		t.Fatal("mock request carries model guidance")
	}
	if err := worker(root, "a", model.Options{Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if row := runfixture.Read(t, root).Tasks["a"]; row.Status != "verified" {
		t.Fatalf("small-input mock failed: %+v", row)
	}
	raw, err := os.ReadFile(filepath.Join(root, "cells", "a", "1.input.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "execution_boundaries") {
		t.Fatal("mock delivered wire changed")
	}
}
