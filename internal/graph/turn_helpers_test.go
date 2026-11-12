package graph

import (
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnHelpersFencesAndAllowance(t *testing.T) {
	for _, kind := range []string{"operation", "attempt", "paused", "cancelled", "settled", "context", "provider", "validator", "dependency", "identity", "limit"} {
		t.Run(kind, func(t *testing.T) {
			root := runfixture.RunningHelperFixture(t)
			s := runfixture.Read(t, root)
			op := s.Tasks["a"].OperationID
			attempt := 1
			req := TurnHelperRequest{ID: "batch", Helpers: []model.Task{runfixture.Task("h", 0)}}
			switch kind {
			case "operation":
				op = "wrong"
			case "attempt":
				attempt = 2
			case "paused":
				if err := checkpoint.Update(root, func(s *model.State) error { s.Control = "paused"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].CancelRequested = true; return nil }); err != nil {
					t.Fatal(err)
				}
			case "settled":
				if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "failed"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "context":
				req.Helpers[0].Context = []string{"secret"}
			case "provider":
				req.Helpers[0].Provider = "haiku"
			case "validator":
				req.Helpers[0].Checks = []model.Check{{Kind: "command", Command: []string{"true"}}}
			case "dependency":
				req.Helpers[0].DependsOn = []string{"a"}
			case "identity":
				req.Helpers[0].Turn = &model.TurnIdentity{Role: "main"}
			case "limit":
				req.Helpers = []model.Task{runfixture.Task("h1", 0), runfixture.Task("h2", 0), runfixture.Task("h3", 0)}
			}
			if AdmitTurnHelpers(root, "a", op, attempt, req) == nil {
				t.Fatal("unsafe helper request admitted")
			}
			if len(runfixture.Read(t, root).Tasks) != 1 {
				t.Fatal("partial admission")
			}
		})
	}
	root := runfixture.RunningHelperFixture(t)
	row := runfixture.Read(t, root).Tasks["a"]
	one := TurnHelperRequest{ID: "one", Helpers: []model.Task{runfixture.Task("h1", 0)}}
	two := TurnHelperRequest{ID: "two", Helpers: []model.Task{runfixture.Task("h2", 0)}}
	if err := AdmitTurnHelpers(root, "a", row.OperationID, 1, one); err != nil {
		t.Fatal(err)
	}
	if err := AdmitTurnHelpers(root, "a", row.OperationID, 1, two); err != nil {
		t.Fatal(err)
	}
	if AdmitTurnHelpers(root, "a", row.OperationID, 1, TurnHelperRequest{ID: "three", Helpers: []model.Task{runfixture.Task("h3", 0)}}) == nil {
		t.Fatal("allowance refilled")
	}
	one.Helpers[0].Prompt = "different"
	if AdmitTurnHelpers(root, "a", row.OperationID, 1, one) == nil {
		t.Fatal("admission ID conflict accepted")
	}
	s := runfixture.Read(t, root)
	s.Tasks["a"].Result = map[string]string{artifactstore.TurnRequestField: `{"helpers":[{"id":"late","prompt":"late","requires":["answer"]}]}`}
	if _, _, err := turnExpansion(s, s.Tasks["a"]); err == nil {
		t.Fatal("boundary request ignored spent asynchronous allowance")
	}
}

func TestTurnHelpersInheritOnlyAcceptedInputsAndDetectTampering(t *testing.T) {
	next := runfixture.Task("next", 0)
	main := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next})
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
	if err := checkpoint.Update(root, func(s *model.State) error {
		r := s.Tasks["a__turn2"]
		r.Status = "running"
		r.Attempt = 1
		r.Result = map[string]string{"answer": "unfinished-secret"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	owner := runfixture.Read(t, root).Tasks["a__turn2"]
	if err := AdmitTurnHelpers(root, "a__turn2", owner.OperationID, 1, TurnHelperRequest{ID: "accepted-only", Helpers: []model.Task{runfixture.Task("h", 0)}}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	h := s.Tasks["a__turn2__helper_h"]
	inputs, err := managed.InputsForTask(s, h.Spec)
	if err != nil || inputs["a"]["answer"] != "ok" || inputs["a__turn2"] != nil {
		t.Fatalf("invalid inherited inputs: %+v %v", inputs, err)
	}
	h.Spec.Prompt = "tamper admission"
	if err := checkpoint.ValidateHelperAdmission(s, h.Spec); err == nil {
		t.Fatal("helper admission tampering accepted")
	}
}
