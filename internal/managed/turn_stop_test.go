package managed

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnStopRequiredFieldAcceptsExplicitEmptyOnly(t *testing.T) {
	task := model.Task{ID: "a", Requires: []string{"answer", artifactstore.TurnRequestField}, Turn: &model.TurnIdentity{AgentID: "a", Role: "main", Index: 2}}
	for _, stop := range []string{"", "  ", "{}"} {
		if err := artifactstore.Accept(task, map[string]string{"answer": "done", artifactstore.TurnRequestField: stop}); err != nil {
			t.Fatalf("stop %q: %v", stop, err)
		}
	}
	for _, output := range []map[string]string{{"answer": "done"}, {"answer": "done", "other": ""}, {"answer": "", artifactstore.TurnRequestField: ""}} {
		if err := artifactstore.Accept(task, output); err == nil {
			t.Fatalf("accepted invalid output: %#v", output)
		}
	}
	task.Turn = nil
	if err := artifactstore.Accept(task, map[string]string{"answer": "done", artifactstore.TurnRequestField: ""}); err == nil {
		t.Fatal("legacy nonempty contract weakened")
	}
}

func TestManagedTurnStopAcceptedThroughAdapter(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","total_cost_usd":0,"structured_output":{"answer":"done","turn_request":""}}'`)
	req.Task.Requires = []string{"answer", artifactstore.TurnRequestField}
	req.Task.Turn = &model.TurnIdentity{AgentID: req.Task.ID, Role: "main", Index: 2}
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err != nil || response.Output["answer"] != "done" {
		t.Fatalf("explicit stop rejected: %+v %v", response, err)
	}

}

func TestTurnFieldErrorsAreDeterministicAndLocated(t *testing.T) {
	value := map[string]any{"helpers": []any{map[string]any{"wait_for_helpers": true, "ZZZ": true}}, "zzz": true}
	for i := 0; i < 100; i++ {
		err := model.ExactTurnJSONFields(value, reflect.TypeOf(checkpoint.TurnRequest{}))
		if err == nil || err.Error() != `noncanonical turn request field "ZZZ" at $.helpers[0]` {
			t.Fatalf("unstable field error: %v", err)
		}
	}
	value = map[string]any{"helpers": []any{map[string]any{"profile": "small"}}}
	if err := model.ExactTurnJSONFields(value, reflect.TypeOf(checkpoint.TurnRequest{})); err != nil {
		t.Fatalf("canonical profile blamed: %v", err)
	}
	err := model.ExactTurnJSONFields(map[string]any{"helpers": []any{map[string]any{"wait_for_helpers": true}}}, reflect.TypeOf(checkpoint.TurnRequest{}))
	if err == nil || !strings.Contains(err.Error(), "helpers[0]") {
		t.Fatal(err)
	}
}
