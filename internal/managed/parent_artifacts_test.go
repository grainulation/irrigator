package managed

import (
	"errors"
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func TestSpawnedTaskReceivesAcceptedAncestorArtifacts(t *testing.T) {
	row := func(id, parent, status, expansion string) *model.TaskState {
		return &model.TaskState{Spec: model.Task{ID: id, Parent: parent}, Status: status, ExpansionHash: expansion, Result: map[string]string{"artifact": id + " contract"}}
	}
	s := &model.State{Tasks: map[string]*model.TaskState{
		"planner":            row("planner", "", "waiting", "accepted-root"),
		"planner__module":    row("planner__module", "planner", "waiting", "accepted-module"),
		"planner__unrelated": row("planner__unrelated", "planner", "verified", ""),
		"dependency":         row("dependency", "", "verified", "accepted-dependency"),
		"dependency__child":  row("dependency__child", "dependency", "verified", ""),
	}}
	s.Tasks["planner"].Children = []string{"planner__module", "planner__unrelated"}
	s.Tasks["dependency"].Children = []string{"dependency__child"}
	task := model.Task{ID: "planner__module__implementation", Parent: "planner__module", DependsOn: []string{"dependency"}}
	inputs, err := InputsForTask(s, task)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]string{}
	for _, id := range []string{"planner", "planner__module", "dependency", "dependency__child"} {
		want[id] = s.Tasks[id].Result
	}
	if !reflect.DeepEqual(inputs, want) {
		t.Fatalf("inputs = %#v; want ancestors and explicit dependency tree only: %#v", inputs, want)
	}
	// A sibling becomes visible only when explicitly requested as a dependency.
	task.DependsOn = append(task.DependsOn, "planner__unrelated")
	inputs, err = InputsForTask(s, task)
	if err != nil || !reflect.DeepEqual(inputs["planner__unrelated"], s.Tasks["planner__unrelated"].Result) {
		t.Fatalf("explicit sibling input missing: %v, %#v", err, inputs)
	}
	if s.Tasks["planner"].Status != "waiting" || s.Tasks["planner__module"].Status != "waiting" {
		t.Fatal("reading planner artifacts changed graph completion state")
	}
}

func TestSpawnedTaskRejectsUnacceptedAncestorArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name, status, hash string
		cancel             bool
	}{
		{"produced", "produced", "", false},
		{"uncommitted", "waiting", "", false},
		{"failed", "failed", "accepted", false},
		{"cancelled", "waiting", "accepted", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &model.State{Tasks: map[string]*model.TaskState{"parent": {Spec: model.Task{ID: "parent"}, Status: tc.status, ExpansionHash: tc.hash, CancelRequested: tc.cancel}}}
			if _, err := InputsForTask(s, model.Task{ID: "child", Parent: "parent"}); !errors.Is(err, launch.ErrNoWork) {
				t.Fatalf("unaccepted ancestor allowed: %v", err)
			}
		})
	}
}

func TestSpawnedTaskRejectsMalformedOwnership(t *testing.T) {
	for _, parent := range []string{"missing", "child"} {
		s := &model.State{Tasks: map[string]*model.TaskState{}}
		if _, err := InputsForTask(s, model.Task{ID: "child", Parent: parent}); err == nil {
			t.Fatalf("allowed invalid parent %q", parent)
		}
	}
}
