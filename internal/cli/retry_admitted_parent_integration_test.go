//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

func TestRetryCommittedFailedTurnPreservesAcceptedGraph(t *testing.T) {
	for _, mode := range []string{"explicit", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 1, HelpersPerTurn: 1}, runfixture.TurnRequestTask(t, "parent", checkpoint.TurnRequest{Helpers: []model.Task{runfixture.Task("h", 0)}}))
			_, done := clitest.Start(t, root)
			clitest.Finished(t, done, true)
			// Synchronous admission atomically verifies the owner. Inject a later
			// status fault to exercise the reported precondition without pretending a
			// normal controller can commit children before accepting its owner.
			if err := checkpoint.Update(root, func(s *model.State) error {
				s.Tasks["parent"].Status = "failed"
				s.Tasks["parent"].Error = "injected post-admission fault"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := runfixture.Read(t, root)
			raw, err := os.ReadFile(filepath.Join(root, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "explicit" {
				if err := retryNode(root, "parent", true, false); err == nil || !strings.Contains(err.Error(), "already committed") {
					t.Fatalf("retry admitted parent: %v", err)
				}
				afterRaw, _ := os.ReadFile(filepath.Join(root, "state.json"))
				if string(raw) != string(afterRaw) {
					t.Fatal("rejected retry mutated checkpoint")
				}
			} else {
				_, done = clitest.Start(t, root, "--retry-failed")
				clitest.Finished(t, done, false)
			}
			after := runfixture.Read(t, root)
			if !reflect.DeepEqual(before.Tasks, after.Tasks) || !reflect.DeepEqual(before.Dynamic, after.Dynamic) || before.PlanHash != after.PlanHash {
				t.Fatal("retry rewrote accepted results or descendant identities")
			}
			if len(after.Tasks) != 2 {
				t.Fatal("duplicate descendants")
			}
		})
	}
}

func TestRetryReportExplainsVerifiedNoOp(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, true)
	report, err := runops.RetryNodeWithReport(root, "a", false, false)
	if err != nil || len(report.Retried) != 0 || len(report.Waiting) != 0 || !reflect.DeepEqual(report.Skipped, []string{"a"}) {
		t.Fatalf("no-op report: %+v %v", report, err)
	}
}

func TestFailedCommittedContinuationRejectedByCheckpointReader(t *testing.T) {
	next := runfixture.Task("next", 0)
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 0}, runfixture.TurnRequestTask(t, "parent", checkpoint.TurnRequest{Continue: &next}))
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, true)
	// Fault injection bypasses the reducers, as disk corruption would. Normal
	// turn admission verifies the predecessor and its continuation atomically.
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["parent"].Status = "failed"; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = checkpoint.ReadState(root); err == nil || !strings.Contains(err.Error(), "continuation admitted before predecessor verification") {
		t.Fatalf("corrupted predecessor accepted: %v", err)
	}
	if err = retryNode(root, "parent", true, false); err == nil {
		t.Fatal("retry accepted corrupted predecessor")
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if string(raw) != string(after) {
		t.Fatal("rejected retry changed corruption evidence")
	}
}
