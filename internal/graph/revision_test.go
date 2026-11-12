package graph

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func revChanged(t model.Task) model.Task { t.Prompt = "produce a better answer"; return t }

func TestRevQueueIdempotentAndConflict(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	p := runfixture.RevProposal("p1", 0, runfixture.Task("a", 0))
	if err := QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	if err := QueueRevision(root, p); err != nil {
		t.Fatalf("identical enqueue must be idempotent: %v", err)
	}
	if n := len(runfixture.Read(t, root).Proposals); n != 1 {
		t.Fatalf("proposals = %d", n)
	}
	q := p
	q.Reason = "other"
	if err := QueueRevision(root, q); err == nil {
		t.Fatal("conflicting same-ID payload accepted")
	}
	bad := runfixture.RevProposal("p2", 0, runfixture.Task("a", 0))
	bad.Confidence = 2
	if QueueRevision(root, bad) == nil {
		t.Fatal("invalid confidence accepted")
	}
	if QueueRevision(root, runfixture.RevProposal("p3", 7, runfixture.Task("a", 0))) == nil {
		t.Fatal("stale base accepted")
	}
	if QueueRevision(root, runfixture.RevProposal("p4", 0, runfixture.Task("a", 0, "missing"))) == nil {
		t.Fatal("invalid graph accepted")
	}
}

func TestRevAppliesGenerationArchiveAndPreservesUnrelated(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0), runfixture.Task("c", 0, "b"), runfixture.Task("u", 0))
	runfixture.RevRun(t, root, func(s *model.State) {
		s.Tasks["u"].Status, s.Tasks["u"].Result, s.Tasks["u"].Cost = "verified", map[string]string{"answer": "x"}, 0.5
		s.Tasks["b"].Attempt, s.Tasks["b"].Status, s.Tasks["b"].Cost = 2, "failed", 0.25
	})
	before := runfixture.Read(t, root)
	if err := QueueRevision(root, runfixture.RevProposal("p1", 0, runfixture.Task("a", 0), revChanged(runfixture.Task("b", 0)), runfixture.Task("c", 0, "b"), runfixture.Task("u", 0), runfixture.Task("h", 0))); err != nil {
		t.Fatal(err)
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 1 || len(s.Revisions) != 1 || len(s.ActiveTasks) != 5 {
		t.Fatalf("revision not committed: %d %d %d", s.GraphRevision, len(s.Revisions), len(s.ActiveTasks))
	}
	if s.Tasks["u"].Status != "verified" || s.Tasks["u"].OperationID != before.Tasks["u"].OperationID || s.Tasks["u"].Cost != 0.5 {
		t.Fatalf("unrelated verified node not preserved: %#v", s.Tasks["u"])
	}
	if s.Tasks["a"].OperationID != before.Tasks["a"].OperationID {
		t.Fatal("unaffected node was replaced")
	}
	for _, id := range []string{"b", "c"} {
		row := s.Tasks[id]
		if row.Generation != 2 || row.Cost != 0 || row.Status != "pending" || row.OperationID == before.Tasks[id].OperationID {
			t.Fatalf("%s not regenerated: %#v", id, row)
		}
	}
	if s.Tasks["b"].Attempt != 2 {
		t.Fatalf("attempt not preserved: %d", s.Tasks["b"].Attempt)
	}
	if len(s.ArchivedTasks) != 2 || model.AdaptiveTotalCost(s) != 0.75 {
		t.Fatalf("archive/cost wrong: %d %v", len(s.ArchivedTasks), model.AdaptiveTotalCost(s))
	}
	if len(s.Revisions[0].RetiredOperations) != 2 || s.Revisions[0].BeforeHash == s.Revisions[0].AfterHash {
		t.Fatalf("record wrong: %#v", s.Revisions[0])
	}
	if _, err := filepath.Glob(filepath.Join(root, "cells", "h")); err != nil {
		t.Fatal(err)
	}
	// Polling again must be idempotent.
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	if again := runfixture.Read(t, root); again.GraphRevision != 1 || len(again.ArchivedTasks) != 2 || len(again.Decisions) != len(s.Decisions) {
		t.Fatal("repeated processing changed state")
	}
}

func TestRevRejectsVerifiedUncertainAndWeakening(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0, "a"))
	runfixture.RevRun(t, root, func(s *model.State) { s.Tasks["a"].Status = "verified" })
	if err := QueueRevision(root, runfixture.RevProposal("v", 0, revChanged(runfixture.Task("a", 0)), runfixture.Task("b", 0, "a"))); err != nil {
		t.Fatal(err)
	}
	weak := runfixture.Task("b", 0, "a")
	weak.Requires = []string{"other"}
	if err := QueueRevision(root, runfixture.RevProposal("w", 0, runfixture.Task("a", 0), weak)); err != nil {
		t.Fatal(err)
	}
	cmd := runfixture.Task("b", 0, "a")
	cmd.Checks = []model.Check{{Kind: "command", Command: []string{"true"}}}
	if err := QueueRevision(root, runfixture.RevProposal("c", 0, runfixture.Task("a", 0), cmd)); err != nil {
		t.Fatal(err)
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 0 {
		t.Fatal("unsafe revision applied")
	}
	for _, id := range []string{"v", "w", "c"} {
		d := TerminalDecision(s, id)
		if d == nil || d.Action != "reject" {
			t.Fatalf("%s not rejected: %#v", id, d)
		}
	}
	runfixture.RevRun(t, root, func(s *model.State) { s.Tasks["b"].Status = "uncertain" })
	if err := QueueRevision(root, runfixture.RevProposal("u", 0, runfixture.Task("a", 0), revChanged(runfixture.Task("b", 0, "a")))); err != nil {
		t.Fatal(err)
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	if d := TerminalDecision(runfixture.Read(t, root), "u"); d == nil || d.Action != "reject" {
		t.Fatalf("uncertain not rejected: %#v", d)
	}
}

func TestRevLiveLeaseDefersWithoutSpam(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0))
	runfixture.RevRun(t, root, nil)
	lease, err := checkpoint.FileLock(filepath.Join(root, "cells", "a", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := QueueRevision(root, runfixture.RevProposal("p", 0, revChanged(runfixture.Task("a", 0)), runfixture.Task("b", 0))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := ProcessRevisionProposals(root); err != nil {
			t.Fatal(err)
		}
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 0 || len(s.Decisions) != 1 || s.Decisions[0].Action != "defer" {
		t.Fatalf("expected one deferral: %#v", s.Decisions)
	}
	checkpoint.Unlock(lease)
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	if s := runfixture.Read(t, root); s.GraphRevision != 1 || s.Tasks["a"].Generation != 2 {
		t.Fatalf("not applied after lease release: %#v", s.Decisions)
	}
}

func TestRevRunningDefersAndPausedDoesNothing(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := QueueRevision(root, runfixture.RevProposal("p", 0, revChanged(runfixture.Task("a", 0)))); err != nil {
		t.Fatal(err)
	}
	if err := ProcessRevisionProposals(root); err != nil || len(runfixture.Read(t, root).Decisions) != 0 {
		t.Fatalf("processed while not running: %v", err)
	}
	runfixture.RevRun(t, root, func(s *model.State) { s.Tasks["a"].Status = "running" })
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	if s := runfixture.Read(t, root); s.GraphRevision != 0 || len(s.Decisions) != 1 || s.Decisions[0].Action != "defer" {
		t.Fatalf("running node not deferred: %#v", s.Decisions)
	}
}

func TestRevStaleBaseRejectedAtProcessAndNoopKeepsRevision(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0))
	runfixture.RevRun(t, root, nil)
	if err := QueueRevision(root, runfixture.RevProposal("noop", 0, runfixture.Task("b", 0), runfixture.Task("a", 0))); err != nil {
		t.Fatal(err)
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 0 || len(s.Revisions) != 0 {
		t.Fatal("no-op incremented revision")
	}
	if d := TerminalDecision(s, "noop"); d == nil || d.Action != "apply" {
		t.Fatalf("noop decision: %#v", d)
	}
	// A proposal queued against revision 0 loses the CAS after another applies.
	root2 := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0))
	runfixture.RevRun(t, root2, nil)
	for _, p := range []model.RevisionProposal{runfixture.RevProposal("one", 0, revChanged(runfixture.Task("a", 0)), runfixture.Task("b", 0)),
		runfixture.RevProposal("two", 0, runfixture.Task("a", 0), revChanged(runfixture.Task("b", 0)))} {
		if err := QueueRevision(root2, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := ProcessRevisionProposals(root2); err != nil {
		t.Fatal(err)
	}
	s = runfixture.Read(t, root2)
	if s.GraphRevision != 1 {
		t.Fatalf("revision = %d", s.GraphRevision)
	}
	if d := TerminalDecision(s, "two"); d == nil || d.Action != "reject" || !strings.Contains(d.Reason, "stale") {
		t.Fatalf("second proposal: %#v", d)
	}
}

func TestRevRetiredIDsCannotReturnAndCapabilitiesBounded(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	runfixture.RevRun(t, root, func(s *model.State) { p := DefaultAdaptivePolicy(); p.CooldownMS = 0; s.Policy = &p })
	if err := QueueRevision(root, runfixture.RevProposal("add-helper", 0, runfixture.Task("a", 0), runfixture.Task("b", 0))); err != nil {
		t.Fatal(err)
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	if err := QueueRevision(root, runfixture.RevProposal("drop", 1, runfixture.Task("a", 0))); err != nil {
		t.Fatal(err)
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 2 || s.Tasks["b"] != nil || len(s.ArchivedTasks) != 1 {
		t.Fatalf("drop failed: %#v", s.Decisions)
	}
	if err := QueueRevision(root, runfixture.RevProposal("revive", 2, runfixture.Task("a", 0), runfixture.Task("b", 0))); err != nil {
		t.Fatal(err)
	}
	helper := runfixture.Task("h", 0)
	helper.Provider = "other"
	if QueueRevision(root, runfixture.RevProposal("prov", 2, runfixture.Task("a", 0), helper)) == nil {
		t.Fatal("unknown provider accepted")
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	if d := TerminalDecision(runfixture.Read(t, root), "revive"); d == nil || d.Action != "reject" {
		t.Fatalf("retired ID revived: %#v", d)
	}
}

func TestRevValidateStateRejectsCorruption(t *testing.T) {
	s := &model.State{GraphRevision: 2}
	if checkpoint.ValidateRevisionState(s) == nil {
		t.Fatal("revision without history accepted")
	}
	if err := checkpoint.ValidateRevisionState(&model.State{}); err != nil {
		t.Fatalf("legacy state rejected: %v", err)
	}
	s = &model.State{Proposals: make([]model.RevisionProposal, checkpoint.MaxProposals+1)}
	if checkpoint.ValidateRevisionState(s) == nil {
		t.Fatal("oversized queue accepted")
	}
}
