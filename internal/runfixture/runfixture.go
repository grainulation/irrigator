package runfixture

import (
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

// Task is a mock task producing "answer".
func Task(id string, delay float64, deps ...string) model.Task {
	return model.Task{ID: id, Prompt: "produce answer", Requires: []string{"answer"}, DependsOn: deps, MockDelay: delay}
}

// New initializes a mock run with tasks and returns its root.
func New(t *testing.T, tasks ...model.Task) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(model.Plan{Name: "test", Tasks: tasks}, root, "mock"); err != nil {
		t.Fatal(err)
	}
	return root
}

// Read reads the run checkpoint or fails the test.
func Read(t *testing.T, root string) *model.State {
	t.Helper()
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
