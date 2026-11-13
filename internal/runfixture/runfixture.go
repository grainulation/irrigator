package runfixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clock"
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

// HoldMock keeps the named mock tasks running until released: each mock call
// for a held task waits while its hold file exists (see clock.MockGateEnv).
// Use it instead of sub-second mock delays when a test needs a task to still be
// running. Every hold is released when the test ends.
func HoldMock(t *testing.T, ids ...string) (release func(ids ...string)) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(clock.MockGateEnv, dir)
	for _, id := range ids {
		if err := os.WriteFile(filepath.Join(dir, id+".hold"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release = func(ids ...string) {
		for _, id := range ids {
			_ = os.Remove(filepath.Join(dir, id+".hold"))
		}
	}
	t.Cleanup(func() {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	})
	return release
}
