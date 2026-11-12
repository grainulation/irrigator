//go:build integration

package cli

import (
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnRevisionPendingPreservesPlanAndExecutes(t *testing.T) {
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, task("a", 0))
	before := read(t, root)
	tasks := before.AllTasks()
	tasks[0].Prompt = "revised bounded work"
	runfixture.RevRun(t, root, nil)
	proposal := runfixture.RevProposal("improve", 0, tasks...)
	if err := graph.QueueRevision(root, proposal); err != nil {
		t.Fatal(err)
	}
	if err := graph.ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := read(t, root)
	if s.GraphRevision != 1 || s.PlanHash != before.PlanHash || s.Plan.Tasks[0].Prompt != before.Plan.Tasks[0].Prompt || s.Tasks["a"].Spec.Prompt != tasks[0].Prompt || s.Tasks["a"].OperationID == before.Tasks["a"].OperationID {
		t.Fatalf("bad revision: %+v", s)
	}
	_, done := start(t, root)
	finished(t, done, true)
	if read(t, root).Tasks["a"].Status != "verified" {
		t.Fatal("revised turn did not execute")
	}
	if err := graph.QueueRevision(root, proposal); err != nil {
		t.Fatalf("durable identical proposal retry: %v", err)
	}
}

func TestTurnRevisionAdmittedContinuationPreservesVerifiedPredecessor(t *testing.T) {
	next := task("next", 0)
	main := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next, Helpers: []model.Task{task("h", 0)}})
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, main)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "run"
		r := s.Tasks["a"]
		r.Status = "produced"
		r.Attempt = 1
		r.Result = main.MockResult
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := commitTurnExpansion(root, "a", "contract"); err != nil {
		t.Fatal(err)
	}
	before := read(t, root)
	tasks := before.AllTasks()
	for i := range tasks {
		if tasks[i].ID == "a__turn2" {
			tasks[i].Prompt = "improved continuation"
		}
	}
	if err := graph.QueueRevision(root, runfixture.RevProposal("continue-better", 0, tasks...)); err != nil {
		t.Fatal(err)
	}
	if err := graph.ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := read(t, root)
	if s.GraphRevision != 1 || s.Tasks["a"].OperationID != before.Tasks["a"].OperationID || s.Tasks["a"].Status != "verified" || s.Tasks["a__turn2"].Spec.Prompt != "improved continuation" {
		t.Fatal("continuation revision lost verified history")
	}
	_, done := start(t, root)
	finished(t, done, true)
	s = read(t, root)
	if s.Tasks["a"].Attempt != 1 || s.Tasks["a__turn2"].Status != "verified" || s.Tasks["a__turn1__helper_h"].Status != "verified" {
		t.Fatal("revised continuation execution failed")
	}
}
