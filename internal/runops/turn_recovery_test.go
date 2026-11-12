package runops

import (
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnRecoveryPreservesCancelledHelperAndUncertainOwner(t *testing.T) {
	root := runfixture.RunningHelperFixture(t)
	row := runfixture.Read(t, root).Tasks["a"]
	if err := graph.AdmitTurnHelpers(root, "a", row.OperationID, 1, graph.TurnHelperRequest{ID: "batch", Helpers: []model.Task{runfixture.Task("h", 0)}}); err != nil {
		t.Fatal(err)
	}
	id := "a__turn1__helper_h"
	if err := CancelNode(root, id); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "uncertain"; return nil }); err != nil {
		t.Fatal(err)
	}
	if retryNode(root, "a", false, false) == nil {
		t.Fatal("uncertain owner replayed without explicit authorization")
	}
	s := runfixture.Read(t, root)
	if s.Tasks["a"].OperationID != row.OperationID || s.Tasks["a"].Status != "uncertain" {
		t.Fatal("uncertain owner changed")
	}
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "failed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := retryNode(root, "a", false, false); err != nil {
		t.Fatal(err)
	}
	s = runfixture.Read(t, root)
	if s.Tasks[id].Status != "cancelled" || !s.Tasks[id].CancelRequested || s.Tasks[id].Attempt != 0 {
		t.Fatal("owner retry bypassed helper cancellation")
	}
	grant := s.Tasks[id].Spec.Turn.Async
	if !checkpoint.HelperAdmissionOwnerMatches(s, s.Tasks["a"], grant) {
		t.Fatal("valid historical owner grant rejected")
	}
	forged := *grant
	forged.OperationID = model.OperationID(s.RunID, "other-owner", 1)
	if checkpoint.HelperAdmissionOwnerMatches(s, s.Tasks["a"], &forged) {
		t.Fatal("unrelated owner grant accepted")
	}
}
