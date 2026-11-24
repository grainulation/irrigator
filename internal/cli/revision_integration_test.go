//go:build integration

package cli

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

func TestControllerConsumesRevisionAndPreservesAcceptedWork(t *testing.T) {
	a, b := runfixture.Task("a", .01), runfixture.Task("b", .01, "a")
	root := runfixture.New(t, a, b)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Policy = &model.AdaptivePolicy{MinBenefitMS: 1, MinConfidence: .5, MaxExtraCostUSD: 1, MaxTotalCostUSD: 5, MaxRevisions: 5}
		s.Tasks["a"].Status, s.Tasks["a"].Validation = "verified", "contract"
		s.Tasks["a"].Result = map[string]string{"answer": "retained"}
		s.Tasks["a"].Attempt, s.Tasks["a"].Cost = 1, .1
		s.Tasks["b"].Status, s.Tasks["b"].Error = "failed", "old implementation failed"
		s.Tasks["b"].Attempt, s.Tasks["b"].Cost = 3, .2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root)
	b.Prompt = "use the retained answer with the revised approach"
	p := model.RevisionProposal{ID: "repair-b", BaseRevision: 0, Tasks: []model.Task{a, b}, Reason: "repair a failed implementation", ExpectedSavingsMS: 1000, Confidence: 1}
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	// The normal controller must consume the proposal without a separate commit.
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, true)
	after := runfixture.Read(t, root)
	if after.GraphRevision != 1 || after.PlanHash != before.PlanHash || !reflect.DeepEqual(after.Plan, before.Plan) {
		t.Fatalf("revision lost original provenance: %#v", after)
	}
	if !reflect.DeepEqual(after.Tasks["a"], before.Tasks["a"]) {
		t.Fatal("unrelated accepted work was modified or replayed")
	}
	if after.Tasks["b"].Generation != 2 || after.Tasks["b"].Attempt != 4 || after.Tasks["b"].OperationID == before.Tasks["b"].OperationID {
		t.Fatalf("replacement lacks new operation/monotonic attempt: %#v", after.Tasks["b"])
	}
	if len(after.ArchivedTasks) != 1 || math.Abs(after.ReportedCost()-.3) > 1e-9 {
		t.Fatalf("retired cost lost or counted twice: %#v", after.ArchivedTasks)
	}
	if len(after.Revisions) != 1 || after.Revisions[0].Outcome == nil || after.Revisions[0].Outcome.Status != "complete" {
		t.Fatal("controller did not record adaptive outcome")
	}
	// Raw logs still referenced by retained revision history are not GC candidates.
	path := filepath.Join(root, "cells", "b", "1.stdout.json")
	if err := os.WriteFile(path, []byte(`{"old":"evidence"}`), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := runops.GarbageCollect(root, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range report.Files {
		if candidate == filepath.Join("cells", "b", "1.stdout.json") {
			t.Fatal("GC would remove archived revision evidence")
		}
	}
}

func TestAdaptiveCLIQueuesWithoutExecuting(t *testing.T) {
	a := runfixture.Task("a", .01)
	root := runfixture.New(t, a)
	policy := model.AdaptivePolicy{MinBenefitMS: 1, MinConfidence: .5, MaxExtraCostUSD: 1, MaxTotalCostUSD: 5, MaxRevisions: 5}
	policyFile := filepath.Join(t.TempDir(), "policy.json")
	if err := jsonio.AtomicJSON(policyFile, policy); err != nil {
		t.Fatal(err)
	}
	if code, err := runCommand([]string{"policy", root, policyFile}); code != 0 || err != nil {
		t.Fatalf("policy: %d %v", code, err)
	}
	a.Prompt = "an improved approach"
	p := model.RevisionProposal{ID: "cli-revision", Tasks: []model.Task{a}, Reason: "improve pending work", ExpectedSavingsMS: 1000, Confidence: 1}
	proposalFile := filepath.Join(t.TempDir(), "proposal.json")
	if err := jsonio.AtomicJSON(proposalFile, p); err != nil {
		t.Fatal(err)
	}
	if code, err := runCommand([]string{"propose", root, proposalFile}); code != 0 || err != nil {
		t.Fatalf("propose: %d %v", code, err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 0 || s.Tasks["a"].Attempt != 0 || len(s.Proposals) != 1 || !reflect.DeepEqual(s.Policy, &policy) {
		t.Fatalf("CLI must queue without executing or committing: %#v", s)
	}
	for _, command := range []string{"policy", "propose"} {
		file := filepath.Join(t.TempDir(), "invalid.json")
		for _, raw := range []string{`{"unknown_typo":1}`, `{} {}`} {
			if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := runCommand([]string{command, root, file}); err == nil {
				t.Fatalf("%s accepted malformed input %s", command, raw)
			}
		}
	}
	before, _ := json.Marshal(s.Policy)
	after, _ := json.Marshal(runfixture.Read(t, root).Policy)
	if string(before) != string(after) {
		t.Fatal("invalid policy input mutated durable bounds")
	}
}

func TestControllerRevisesPendingWorkWhileUnrelatedWorkerRuns(t *testing.T) {
	a, b := runfixture.Task("a", .5), runfixture.Task("b", .01, "a")
	root := runfixture.New(t, a, b)
	release := runfixture.HoldMock(t, "a") // a stays running until the revision has landed
	_, done := clitest.Start(t, root, "--workers", "1")
	waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
	original := runfixture.Read(t, root).Tasks["a"].OperationID
	b.Prompt = "use a better implementation after the dependency completes"
	p := model.RevisionProposal{ID: "revise-pending", Tasks: []model.Task{a, b}, Reason: "shorten remaining work", ExpectedSavingsMS: 1000, Confidence: 1}
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	waitFor(t, root, func(s *model.State) bool { return s.GraphRevision == 1 })
	during := runfixture.Read(t, root)
	if during.Tasks["a"].Status != "running" || during.Tasks["a"].OperationID != original {
		t.Fatal("revision waited for or disrupted unrelated worker")
	}
	release("a")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Attempt != 1 || s.Tasks["b"].Attempt != 1 || s.Tasks["b"].Generation != 2 {
		t.Fatal("revision replayed unrelated work or ran old pending generation")
	}
}

func TestRevisedCancelledOwnerReexpandsWithFreshChildOperation(t *testing.T) {
	parent := runfixture.Task("owner", .01)
	parent.Spawn = &model.SpawnPolicy{Field: "answer", MaxChildren: 2}
	parent.MockResult = map[string]string{"answer": `{"tasks":[{"id":"child","prompt":"work","requires":["answer"],"mock_delay":0.01}]}`}
	root := runfixture.New(t, parent)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["owner"].Status = "produced"
		s.Tasks["owner"].Result = parent.MockResult
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := commitExpansion(root, "owner", "contract"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["owner__child"].Attempt = 3
		s.Tasks["owner__child"].Cost = .2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	oldOperation := runfixture.Read(t, root).Tasks["owner__child"].OperationID
	oldOutput := filepath.Join(root, "cells", "owner__child", "3.stdout.json")
	if err := os.WriteFile(oldOutput, []byte("prior execution evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runops.CancelNode(root, "owner"); err != nil {
		t.Fatal(err)
	}
	parent.Prompt = "recreate the cancelled child graph with an improved approach"
	p := model.RevisionProposal{ID: "replace-owner", Tasks: []model.Task{parent}, Reason: "restart cancelled graph", ExpectedSavingsMS: 1000, Confidence: 1}
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	child := s.Tasks["owner__child"]
	if s.Revisions[0].TaskCount != 1 || len(s.ActiveTasks) != 2 || checkpoint.ValidateRevisionState(s) != nil {
		t.Fatal("legitimate expansion did not preserve the committed snapshot prefix")
	}
	if child == nil || child.Status != "verified" || child.Generation != 2 || child.Attempt != 4 || child.OperationID == oldOperation || child.Cost != 0 {
		t.Fatalf("reexpanded child reused retired operation, attempts, or cost: %#v", child)
	}
	if math.Abs(s.ReportedCost()-.2) > 1e-9 {
		t.Fatal("retired child spend was lost or double-counted")
	}
	if raw, err := os.ReadFile(oldOutput); err != nil || string(raw) != "prior execution evidence" {
		t.Fatal("reexpanded child overwrote prior attempt evidence")
	}
}
