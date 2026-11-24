//go:build integration

package cli

import (
	"encoding/json"
	"testing"

	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnRecoveryRetriesOwnerWithoutReplayingVerifiedHelper(t *testing.T) {
	for _, mode := range []string{"explicit-node", "retry-failed"} {
		t.Run(mode, func(t *testing.T) {
			root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, task("a", 0))
			runfixture.HoldMock(t, "a") // the owner can only end by timing out, after its helper verified
			_, done := start(t, root, "--workers", "2", "--timeout", "3s")
			waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
			owner := read(t, root).Tasks["a"]
			request := graph.TurnHelperRequest{ID: "original-batch", Helpers: []model.Task{task("h", .01)}}
			if err := graph.AdmitTurnHelpers(root, "a", owner.OperationID, owner.Attempt, request); err != nil {
				t.Fatal(err)
			}
			id := "a__turn1__helper_h"
			waitFor(t, root, func(s *model.State) bool { return s.Tasks[id] != nil && s.Tasks[id].Status == "verified" })
			finished(t, done, false)
			before := read(t, root)
			savedHelper, _ := json.Marshal(before.Tasks[id])
			if before.Tasks["a"].Status != "failed" {
				t.Fatalf("expected known mock timeout failure, got %s", before.Tasks["a"].Status)
			}
			options := []string{"--workers", "2", "--timeout", "5s"}
			if mode == "explicit-node" {
				if err := retryNode(root, "a", false, false); err != nil {
					t.Fatal(err)
				}
				if err := graph.ReconcileParents(root); err != nil {
					t.Fatal(err)
				}
				if read(t, root).Tasks["a"].Status != "pending" {
					t.Fatal("helper completion falsely verified owner")
				}
			} else {
				options = append(options, "--retry-failed")
			}
			release := runfixture.HoldMock(t, "a") // the retry stays running while admissions are checked
			_, done = start(t, root, options...)
			waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" && s.Tasks["a"].Attempt == 2 })
			current := read(t, root).Tasks["a"]
			extra := graph.TurnHelperRequest{ID: "extra-batch", Helpers: []model.Task{task("extra", 0)}}
			if graph.AdmitTurnHelpers(root, "a", owner.OperationID, owner.Attempt, extra) == nil {
				t.Fatal("stale owner attempt admitted new work")
			}
			if graph.AdmitTurnHelpers(root, "a", current.OperationID, current.Attempt, extra) == nil {
				t.Fatal("owner retry refilled helper allowance")
			}
			if err := graph.AdmitTurnHelpers(root, "a", owner.OperationID, owner.Attempt, request); err != nil {
				t.Fatalf("original admission lost idempotency: %v", err)
			}
			release("a")
			finished(t, done, true)
			after := read(t, root)
			helperAfter, _ := json.Marshal(after.Tasks[id])
			if string(savedHelper) != string(helperAfter) {
				t.Fatal("verified helper history changed")
			}
			if after.PlanHash != before.PlanHash || after.Tasks["a"].Attempt != 2 || after.Tasks["a"].Generation != before.Tasks["a"].Generation+1 || after.Tasks["a"].OperationID == owner.OperationID || after.Tasks["a"].Status != "verified" || after.Tasks["a"].Result["answer"] != "mock answer" {
				t.Fatal("owner did not execute and validate a fresh operation")
			}
		})
	}
}
