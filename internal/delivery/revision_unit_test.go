package delivery

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestSupersededWorkerAdmissionDoesNotExecute(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	original := runfixture.Read(t, root).Tasks["a"].OperationID
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "run"
		r := s.Tasks["a"]
		r.Generation++
		r.OperationID = model.OperationID(s.RunID, "a", r.Generation)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := executor.WorkerOperation(root, "a", model.Options{}, original); err != nil {
		t.Fatal(err)
	}
	if row := runfixture.Read(t, root).Tasks["a"]; row.Attempt != 0 || row.Status != "pending" {
		t.Fatalf("superseded launch executed: %#v", row)
	}
	if err := executor.WorkerOperation(root, "removed", model.Options{}, original); err != nil {
		t.Fatalf("removed queued worker should stop cleanly: %v", err)
	}
}

func TestDeliveryCannotReuseManifestAcrossRevisions(t *testing.T) {
	s := &model.State{RunID: "r", Tasks: map[string]*model.TaskState{"a": {Status: "verified", Result: map[string]string{"files": `{"index.html":"same bytes"}`}}}}
	dir := filepath.Join(t.TempDir(), "delivery")
	if err := assembleDelivery(s, dir, []string{"a"}, "files"); err != nil {
		t.Fatal(err)
	}
	s.GraphRevision = 1
	if err := assembleDelivery(s, dir, []string{"a"}, "files"); err == nil {
		t.Fatal("accepted old delivery manifest under revised graph")
	}
}

func TestRevisionCannotEraseOriginalGoalOrOutput(t *testing.T) {
	a := runfixture.Task("required-goal", .01)
	a.Outputs = map[string]string{"answer": "answer.txt"}
	root := runfixture.New(t, a, runfixture.Task("other", .01))
	s := runfixture.Read(t, root)
	for _, tasks := range [][]model.Task{
		{runfixture.Task("other", .01)},
		{runfixture.Task("required-goal", .01), runfixture.Task("other", .01)},
	} {
		p := model.RevisionProposal{ID: "weaken-goal", Tasks: tasks, Reason: "cheaper", ExpectedSavingsMS: 5000, Confidence: 1}
		if got := graph.EvaluateRevision(root, s, p, time.Now()); got.Kind != "reject" {
			t.Fatalf("goal weakening accepted: %#v", got)
		}
	}
}

func TestRevisionLiveProcessIsDeferredNotRejected(t *testing.T) {
	a := runfixture.Task("a", .01)
	root := runfixture.New(t, a)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "run"
		s.Tasks["a"].Status, s.Tasks["a"].Attempt = "running", 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cells", "a", "1.process.json"), []byte(`{"status":"active"}`), 0600); err != nil {
		t.Fatal(err)
	}
	a.Prompt = "different implementation"
	p := model.RevisionProposal{ID: "later", Tasks: []model.Task{a}, Reason: "improve", ExpectedSavingsMS: 1000, Confidence: 1}
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	if err := graph.ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if len(s.Decisions) != 1 || s.Decisions[0].Action != "defer" || s.GraphRevision != 0 {
		t.Fatalf("live provider should defer a proposal: %#v", s.Decisions)
	}
}

func TestRevisionSnapshotRequiresJournal(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01))
	s := runfixture.Read(t, root)
	s.ActiveTasks = s.AllTasks()
	if err := checkpoint.ValidateRevisionState(s); err == nil {
		t.Fatal("unjournaled active snapshot accepted")
	}
}

func TestRevisionCannotAddChildToVerifiedOwner(t *testing.T) {
	a := runfixture.Task("owner", .01)
	a.Spawn = &model.SpawnPolicy{Field: "answer", MaxChildren: 2}
	root := runfixture.New(t, a)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["owner"].Status = "produced"
		s.Tasks["owner"].Result = map[string]string{"answer": `{"tasks":[]}`}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := commitExpansion(root, "owner", "contract"); err != nil {
		t.Fatal(err)
	}
	child := runfixture.Task("owner__new", .01)
	child.Parent = "owner"
	p := model.RevisionProposal{ID: "add-child", Tasks: []model.Task{a, child}, Reason: "new work", ExpectedSavingsMS: 1000, Confidence: 1}
	if got := graph.EvaluateRevision(root, runfixture.Read(t, root), p, time.Now()); got.Kind != "reject" {
		t.Fatalf("verified owner mutated by child addition: %#v", got)
	}
}

func TestRevisionCannotExpandSpawnThroughDefault(t *testing.T) {
	a := runfixture.Task("a", .01)
	a.Spawn = &model.SpawnPolicy{Field: "answer", MaxChildren: 2}
	root := runfixture.New(t, a)
	a.Spawn = &model.SpawnPolicy{Field: "answer", MaxChildren: 0}
	p := model.RevisionProposal{ID: "implicit-expansion", Tasks: []model.Task{a}, Reason: "use defaults", ExpectedSavingsMS: 1000, Confidence: 1}
	if got := graph.EvaluateRevision(root, runfixture.Read(t, root), p, time.Now()); got.Kind != "reject" {
		t.Fatalf("zero expanded max_children from two to eight: %#v", got)
	}
}

func TestProposalReceiptsSurviveActualHistoryPruning(t *testing.T) {
	a := runfixture.Task("a", .01)
	root := runfixture.New(t, a)
	p := model.RevisionProposal{ID: "original-request", Tasks: []model.Task{a}, Reason: "keep graph", ExpectedSavingsMS: 1000, Confidence: 1}
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		graph.AddDecision(s, model.AdaptiveDecision{ProposalID: p.ID, Action: "reject", Reason: "test", Time: time.Now()})
		// Exercise the same bounded pruning functions across more than a full
		// decision window without launching workers or paying model costs.
		for i := 0; i < checkpoint.MaxDecisions+checkpoint.MaxProposals; i++ {
			q := p
			q.ID = fmt.Sprintf("later-%d", i)
			if len(s.Proposals) >= checkpoint.MaxProposals {
				graph.PruneProposals(s)
			}
			s.Proposals = append(s.Proposals, q)
			s.ProposalReceipts[q.ID] = checkpoint.ProposalDigest(q)
			graph.AddDecision(s, model.AdaptiveDecision{ProposalID: q.ID, Action: "reject", Reason: "test", Time: time.Now()})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root)
	if graph.TerminalDecision(before, p.ID) != nil {
		t.Fatal("test did not prune original decision")
	}
	for _, q := range before.Proposals {
		if q.ID == p.ID {
			t.Fatal("test did not prune original proposal")
		}
	}
	p.SubmittedAt = time.Now()
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatalf("identical replay after pruning: %v", err)
	}
	after := runfixture.Read(t, root)
	if !reflect.DeepEqual(before, after) || after.Tasks["a"].Attempt != 0 {
		t.Fatal("identical replay queued or executed work")
	}
	p.Reason = "different payload under old ID"
	if err := graph.QueueRevision(root, p); err == nil {
		t.Fatal("reused pruned proposal ID with new payload")
	}
}

func TestProposalReceiptLegacyBackfillAndCapacity(t *testing.T) {
	a := runfixture.Task("a", .01)
	p := model.RevisionProposal{ID: "legacy", Tasks: []model.Task{a}, Reason: "test", ExpectedSavingsMS: 1000, Confidence: 1}
	root := runfixture.New(t, a)
	if err := checkpoint.Update(root, func(s *model.State) error { s.Proposals = []model.RevisionProposal{p}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).ProposalReceipts[p.ID] != checkpoint.ProposalDigest(p) {
		t.Fatal("legacy queued payload not backfilled")
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Proposals = nil
		s.ProposalReceipts = nil
		s.Decisions = []model.AdaptiveDecision{{ProposalID: p.ID, Action: "reject", Reason: "legacy"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := graph.QueueRevision(root, p); err == nil {
		t.Fatal("legacy decision-only ID reused without original payload")
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.ProposalReceipts = map[string]string{p.ID: checkpoint.ProposalDigest(p)}
		for i := 1; i < checkpoint.MaxProposalReceipts; i++ {
			s.ProposalReceipts[fmt.Sprintf("reserved-%d", i)] = checkpoint.ProposalDigest(p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatalf("full receipts must still permit identical replay: %v", err)
	}
	p.ID = "new-request"
	if err := graph.QueueRevision(root, p); err == nil {
		t.Fatal("receipt capacity silently evicted identity history")
	}
	s := runfixture.Read(t, root)
	s.ProposalReceipts["legacy"] = "not-a-digest"
	if err := checkpoint.ValidateRevisionState(s); err == nil {
		t.Fatal("invalid receipt digest accepted")
	}
}

func TestRevisionCommittedSnapshotRejectsMatchingSpecTampering(t *testing.T) {
	a := runfixture.Task("a", .01)
	root := runfixture.New(t, a)
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	a.Prompt = "legitimate revised implementation"
	p := model.RevisionProposal{ID: "pin-snapshot", Tasks: []model.Task{a}, Reason: "improve", ExpectedSavingsMS: 1000, Confidence: 1}
	if err := graph.QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	if err := graph.ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 1 || s.Revisions[0].TaskCount != 1 {
		t.Fatal("snapshot prefix was not pinned")
	}
	s.ActiveTasks[0].Prompt = "changed outside revision journal"
	s.Tasks["a"].Spec.Prompt = s.ActiveTasks[0].Prompt
	if err := checkpoint.ValidateRevisionState(s); err == nil {
		t.Fatal("matching task/spec corruption bypassed committed snapshot hash")
	}
}
