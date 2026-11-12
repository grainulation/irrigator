//go:build integration

package cli

import (
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

func TestScheduleFlagValidationAndScope(t *testing.T) {
	if _, err := runops.NormalizeSchedulingOrder("random"); err == nil {
		t.Fatal("accepted unknown order")
	}
	if code, err := runCommand([]string{"resume", t.TempDir(), "--schedule", "random"}); err == nil || code != 2 {
		t.Fatalf("resume accepted bad order: %d %v", code, err)
	}
	if code, err := runCommand([]string{"plan", "x.json", "--schedule", "breadth"}); err == nil || code != 2 {
		t.Fatalf("flag leaked to non apply/resume command: %d %v", code, err)
	}
}

func TestPersistedBreadthRunStillHonorsDependencies(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01), runfixture.Task("b", .01, "a"), runfixture.Task("c", .01))
	if err := checkpoint.Update(root, func(s *model.State) error { s.Scheduling = runops.ScheduleBreadth; return nil }); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, root, "--workers", "1")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	pos := map[string]int{}
	for i, e := range s.Events {
		if e.Kind == "started" {
			pos[e.Task] = i
		}
	}
	if _, ok := pos["b"]; !ok || pos["a"] > pos["b"] {
		t.Fatalf("dependent started before dependency: %v", pos)
	}
	for id, row := range s.Tasks {
		if row.Status != "verified" {
			t.Fatalf("%s %s", id, row.Status)
		}
	}
}
