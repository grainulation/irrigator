//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnHelpersExecuteDuringMainAndJoin(t *testing.T) {
	next := task("next", 0)
	main := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next, WaitForHelpers: true})
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 2}, main)
	release := runfixture.HoldMock(t, "a") // main runs until its helper has verified
	before := read(t, root)
	_, done := start(t, root, "--workers", "2")
	waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
	row := read(t, root).Tasks["a"]
	req := graph.TurnHelperRequest{ID: "live-research", Helpers: []model.Task{task("research", .01)}}
	raw, _ := json.Marshal(req)
	path := filepath.Join(t.TempDir(), "helpers.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := graph.TurnHelpersCLI([]string{root, "a", "--operation", row.OperationID, "--attempt", "1", "--request", path}); err != nil || code != 0 {
		t.Fatalf("CLI admission: %d %v", code, err)
	}
	helperID := "a__turn1__helper_research"
	waitFor(t, root, func(s *model.State) bool { return s.Tasks[helperID] != nil && s.Tasks[helperID].Status == "verified" })
	live := read(t, root)
	if live.Tasks["a"].Status != "running" {
		t.Fatal("helper did not finish while main was running")
	}
	inputs, err := managed.InputsForTask(live, live.Tasks[helperID].Spec)
	if err != nil || len(inputs) != 0 {
		t.Fatalf("unfinished owner output leaked: %v %v", inputs, err)
	}
	if err := graph.AdmitTurnHelpers(root, "a", row.OperationID, row.Attempt, req); err != nil {
		t.Fatal(err)
	}
	release("a")
	finished(t, done, true)
	s := read(t, root)
	if s.PlanHash != before.PlanHash || len(s.Tasks) != 3 || s.Tasks[helperID].Attempt != 1 || !artifactstore.TurnContains(s.Tasks["a__turn2"].Spec.DependsOn, helperID) {
		t.Fatal("lost immutable plan, idempotency or join")
	}
	if err := graph.AdmitTurnHelpers(root, "a", row.OperationID, row.Attempt, req); err != nil {
		t.Fatalf("settled admission retry: %v", err)
	}
	_, done = start(t, root)
	finished(t, done, true)
	if read(t, root).Tasks[helperID].Attempt != 1 {
		t.Fatal("helper replayed on restart")
	}
}
