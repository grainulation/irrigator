package graph

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

func TestInputFieldsLegacyPlanEncodingUnchanged(t *testing.T) {
	// Checkpoint identity hashes this exact encoding; adding an omitted selector
	// must not invalidate existing durable plans or operation identities.
	legacy := []byte(`{"name":"legacy","tasks":[{"id":"a","prompt":"build","requires":["files"]},{"id":"b","prompt":"review","depends_on":["a"],"requires":["summary"]}]}`)
	var plan model.Plan
	if err := json.Unmarshal(legacy, &plan); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil || !bytes.Equal(encoded, legacy) {
		t.Fatalf("legacy plan hash input changed: %s (%v)", encoded, err)
	}
}

func TestInputFieldsNarrowArtifactsWithoutChangingReadiness(t *testing.T) {
	s := &model.State{Tasks: map[string]*model.TaskState{
		"build":        {Status: "verified", Result: map[string]string{"summary": "done", "files": strings.Repeat("x", 100000)}, Children: []string{"build__child"}},
		"build__child": {Status: "verified", Result: map[string]string{"files": "child contents"}},
	}}
	task := model.Task{ID: "review", DependsOn: []string{"build"}, InputFields: map[string][]string{"build": {"summary"}}}
	selected, err := managed.InputsForTask(s, task)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(selected)
	if string(b) != `{"build":{"summary":"done"}}` {
		t.Fatal(string(b))
	}
	full, _ := json.Marshal(map[string]map[string]string{"build": s.Tasks["build"].Result, "build__child": s.Tasks["build__child"].Result})
	t.Logf("dependency JSON: %d bytes full -> %d bytes selected", len(full), len(b))
	selected["build"]["summary"] = "mutated"
	if s.Tasks["build"].Result["summary"] != "done" {
		t.Fatal("selected inputs alias durable state")
	}
	task.InputFields = nil
	all, err := managed.InputsForTask(s, task)
	if err != nil || len(all) != 2 || len(all["build"]["files"]) != 100000 {
		t.Fatalf("legacy inputs lost: %v", err)
	}
	task.InputFields = map[string][]string{"build": {"summary"}}
	s.Tasks["build"].Status = "waiting"
	if _, err := managed.InputsForTask(s, task); err != launch.ErrNoWork {
		t.Fatalf("selector bypassed readiness: %v", err)
	}
}

func TestInputFieldsAcceptsDependencyDescendantsAndRejectsMissingOutput(t *testing.T) {
	parent := model.Task{ID: "build", Prompt: "build", Requires: []string{"summary"}}
	child := model.Task{ID: "build__module", Parent: "build", Prompt: "module", Requires: []string{"files"}}
	review := model.Task{ID: "review", Prompt: "review", Requires: []string{"summary"}, DependsOn: []string{"build"}, InputFields: map[string][]string{"build__module": {"files"}}}
	if err := checkpoint.Validate(model.Plan{Tasks: []model.Task{parent, child, review}}); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.SelectInputFields(review, map[string]map[string]string{"build__module": {}}); err == nil {
		t.Fatal("missing selected output silently dropped")
	}
	if _, err := launch.SelectInputFields(review, map[string]map[string]string{}); err == nil {
		t.Fatal("missing selected source silently dropped")
	}
}

func TestInputFieldsValidation(t *testing.T) {
	base := []model.Task{
		{ID: "build", Prompt: "build", Requires: []string{"summary", "files"}},
		{ID: "other", Prompt: "other", Requires: []string{"summary"}},
		{ID: "review", Prompt: "review", Requires: []string{"summary"}, DependsOn: []string{"build"}},
	}
	for _, tc := range []struct {
		name     string
		selector map[string][]string
		valid    bool
	}{
		{"valid", map[string][]string{"build": {"summary"}}, true},
		{"empty fields", map[string][]string{"build": {}}, true},
		{"unknown source", map[string][]string{"missing": {"summary"}}, false},
		{"unrelated", map[string][]string{"other": {"summary"}}, false},
		{"unknown field", map[string][]string{"build": {"secret"}}, false},
		{"duplicate field", map[string][]string{"build": {"summary", "summary"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tasks := append([]model.Task{}, base...)
			tasks[2].InputFields = tc.selector
			err := checkpoint.Validate(model.Plan{Tasks: tasks})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestSpawnedInputFieldsNamespacesSiblingAndPreservesAncestor(t *testing.T) {
	parent := model.Task{ID: "planner", Prompt: "plan", Requires: []string{"subgraph", "contract"}, Spawn: &model.SpawnPolicy{Field: "subgraph"}}
	source := `{"tasks":[{"id":"build","prompt":"build","requires":["files","summary"]},{"id":"review","prompt":"review","requires":["summary"],"depends_on":["build"],"input_fields":{"build":["summary"],"planner":["contract"]}}]}`
	row := &model.TaskState{Spec: parent, Result: map[string]string{"subgraph": source, "contract": "contract"}}
	s := &model.State{Backend: "mock", Plan: model.Plan{Tasks: []model.Task{parent}}, Tasks: map[string]*model.TaskState{"planner": row}}
	children, _, err := Expansion(s, row)
	if err != nil {
		t.Fatal(err)
	}
	selected := children[1].InputFields
	if len(selected) != 2 || len(selected["planner__build"]) != 1 || len(selected["planner"]) != 1 {
		t.Fatalf("wrong namespace: %#v", selected)
	}
	// A durable round trip preserves the selector for retries/recovery.
	data, _ := json.Marshal(children[1])
	var recovered model.Task
	if err := json.Unmarshal(data, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.InputFields["planner__build"][0] != "summary" {
		t.Fatal("selector lost on recovery")
	}
}
