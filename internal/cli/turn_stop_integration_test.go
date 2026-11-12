//go:build integration

package cli

import (
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnStopContinuationRuntime(t *testing.T) {
	next := model.Task{Prompt: "finish", Requires: []string{"answer", artifactstore.TurnRequestField}, MockResult: map[string]string{"answer": "done", artifactstore.TurnRequestField: ""}}
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2}, runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next}))
	_, done := start(t, root)
	finished(t, done, true)
	state := read(t, root)
	if len(state.Tasks) != 2 || state.Tasks["a__turn2"].Status != "verified" {
		t.Fatalf("lane did not stop cleanly: %+v", state.Tasks)
	}
}
