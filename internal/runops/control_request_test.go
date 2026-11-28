package runops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func controlFixture(t *testing.T) (string, ControlRequest) {
	t.Helper()
	root := runfixture.New(t, runfixture.Task("cell", 0), runfixture.Task("accepted", 0), runfixture.Task("unknown", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["accepted"].Status = "verified"
		s.Tasks["accepted"].Result = map[string]string{"answer": "immutable"}
		s.Tasks["unknown"].Status = "uncertain"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	return root, ControlRequest{RequestID: "request-one", ExpectedRunID: s.RunID, ExpectedPlanHash: s.PlanHash, ExpectedRevision: s.GraphRevision, Cell: "cell", OperationID: s.Tasks["cell"].OperationID}
}

func TestControlRequestIdentityAndNoDuplicatePublication(t *testing.T) {
	root, in := controlFixture(t)
	receipt, err := RequestControl(context.Background(), root, "cancel_cell", in)
	if err != nil || !receipt.Changed {
		t.Fatal(receipt, err)
	}
	path := filepath.Join(root, "state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := RequestControl(context.Background(), root, "cancel_cell", in)
	if err != nil || again != receipt {
		t.Fatal(again, err)
	}
	after, _ := os.ReadFile(path)
	next, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !os.SameFile(info, next) {
		t.Fatal("duplicate request published checkpoint")
	}
	in.Cell, in.OperationID = "unknown", runfixture.Read(t, root).Tasks["unknown"].OperationID
	if _, err := RequestControl(context.Background(), root, "cancel_cell", in); err == nil {
		t.Fatal("request ID reused with different target")
	}
	in.RequestID = "different-request"
	if _, err := RequestControl(context.Background(), root, "cancel_cell", in); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.Tasks["unknown"].Status != "uncertain" || !s.Tasks["unknown"].CancelRequested {
		t.Fatal("uncertainty lost")
	}
	if s.Tasks["accepted"].Status != "verified" || s.Tasks["accepted"].Result["answer"] != "immutable" {
		t.Fatal("accepted result changed")
	}
}

func TestControlRequestsRefuseStaleRunGraphAndOperation(t *testing.T) {
	for _, field := range []string{"run", "plan", "revision", "operation"} {
		t.Run(field, func(t *testing.T) {
			root, in := controlFixture(t)
			switch field {
			case "run":
				in.ExpectedRunID = "another"
			case "plan":
				in.ExpectedPlanHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "revision":
				in.ExpectedRevision++
			case "operation":
				in.OperationID = "other"
			}
			before, _ := os.ReadFile(filepath.Join(root, "state.json"))
			if _, err := RequestControl(context.Background(), root, "cancel_cell", in); err == nil {
				t.Fatal("stale identity accepted")
			}
			after, _ := os.ReadFile(filepath.Join(root, "state.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("failed request changed state")
			}
		})
	}
}

func TestStopRequestIsCooperativeAndDoesNotReopenCompleteRun(t *testing.T) {
	root, in := controlFixture(t)
	in.Cell, in.OperationID = "", ""
	if _, err := RequestControl(context.Background(), root, "stop_run", in); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.Control != "down" || s.Tasks["unknown"].Status != "uncertain" || s.Tasks["accepted"].Status != "verified" {
		t.Fatal("stop fabricated cleanup or changed accepted work")
	}
	if err := checkpoint.Update(root, func(s *model.State) error { s.Control = "complete"; return nil }); err != nil {
		t.Fatal(err)
	}
	in.RequestID = "stop-complete"
	receipt, err := RequestControl(context.Background(), root, "stop_run", in)
	if err != nil || receipt.Changed || runfixture.Read(t, root).Control != "complete" {
		t.Fatal(receipt, err)
	}
}
