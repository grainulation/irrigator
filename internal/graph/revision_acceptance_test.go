package graph

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Acceptance tests use only the public revision API from CONTRACT.md:
// queueRevision, processRevisionProposals, validateRevisionState and State.

func accGood(id string, base int, tasks []model.Task) model.RevisionProposal {
	return model.RevisionProposal{ID: id, BaseRevision: base, Tasks: tasks, Reason: "acceptance",
		ExpectedSavingsMS: 5000, SwitchCostMS: 10, Confidence: 0.95, SubmittedAt: time.Now().UTC()}
}

// accGraph builds a(verified) -> b -> c, plus independent verified d, in run mode.
func accGraph(t *testing.T) string {
	t.Helper()
	root := runfixture.New(t, runfixture.Task("a", .01), runfixture.Task("b", .01, "a"), runfixture.Task("c", .01, "b"), runfixture.Task("d", .01))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "run"
		for _, id := range []string{"a", "d"} {
			s.Tasks[id].Status = "verified"
			s.Tasks[id].Attempt = 1
			s.Tasks[id].Result = map[string]string{"answer": "evidence-" + id}
		}
		s.Tasks["a"].Cost = 0.1
		s.Tasks["d"].Cost = 0.05
		s.Tasks["b"].Attempt = 2
		s.Tasks["b"].Cost = 0.2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func accActive(t *testing.T, root string) []model.Task {
	t.Helper()
	return runfixture.Read(t, root).AllTasks()
}

func accEdit(tasks []model.Task, id string, f func(*model.Task)) []model.Task {
	out := make([]model.Task, len(tasks))
	for i, tk := range tasks {
		raw, _ := json.Marshal(tk)
		_ = json.Unmarshal(raw, &out[i])
		if tk.ID == id {
			f(&out[i])
		}
	}
	return out
}

func accSnapshot(t *testing.T, s *model.State) string {
	t.Helper()
	raw, err := json.Marshal(struct {
		Tasks    map[string]*model.TaskState
		Archived []model.TaskState
		Rev      int
		Revs     []model.GraphRevisionRecord
		Active   []model.Task
	}{s.Tasks, s.ArchivedTasks, s.GraphRevision, s.Revisions, s.AllTasks()})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func accDecision(s *model.State, id string) *model.AdaptiveDecision {
	var last *model.AdaptiveDecision
	for i := range s.Decisions {
		if s.Decisions[i].ProposalID == id {
			last = &s.Decisions[i]
		}
	}
	return last
}

// accSubmit queues and processes; a queue-time rejection is acceptable and
// returned to the caller instead of failing the test.
func accSubmit(t *testing.T, root string, p model.RevisionProposal) error {
	t.Helper()
	if err := QueueRevision(root, p); err != nil {
		return err
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatalf("process: %v", err)
	}
	return nil
}

func accRejected(t *testing.T, root string, p model.RevisionProposal) {
	t.Helper()
	before := accSnapshot(t, runfixture.Read(t, root))
	_ = accSubmit(t, root, p)
	s := runfixture.Read(t, root)
	if got := accSnapshot(t, s); got != before {
		t.Fatalf("rejected proposal %s mutated graph/evidence:\nbefore %s\nafter  %s", p.ID, before, got)
	}
	if d := accDecision(s, p.ID); d != nil && d.Action == "apply" {
		t.Fatalf("proposal %s unexpectedly applied", p.ID)
	}
	if err := checkpoint.ValidateRevisionState(s); err != nil {
		t.Fatalf("state invalid after rejection: %v", err)
	}
}

func TestAcceptanceEnqueueIdempotencyAndConflict(t *testing.T) {
	root := accGraph(t)
	p := accGood("p1", 0, accEdit(accActive(t, root), "c", func(x *model.Task) { x.Prompt = "new" }))
	if err := QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	if err := QueueRevision(root, p); err != nil {
		t.Fatalf("identical enqueue must be idempotent: %v", err)
	}
	n := 0
	for _, q := range runfixture.Read(t, root).Proposals {
		if q.ID == "p1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected one queued p1, got %d", n)
	}
	conflict := p
	conflict.Reason = "different payload"
	if err := QueueRevision(root, conflict); err == nil {
		t.Fatal("same ID with different payload must be rejected")
	}
}

func TestAcceptanceInvalidProposalsNotQueued(t *testing.T) {
	root := accGraph(t)
	tasks := accActive(t, root)
	bad := map[string]func(*model.RevisionProposal){
		"empty-id":     func(p *model.RevisionProposal) { p.ID = "" },
		"empty-reason": func(p *model.RevisionProposal) { p.Reason = " " },
		"nan-savings":  func(p *model.RevisionProposal) { p.ExpectedSavingsMS = math.NaN() },
		"inf-cost":     func(p *model.RevisionProposal) { p.ExtraCostUSD = math.Inf(1) },
		"nan-conf":     func(p *model.RevisionProposal) { p.Confidence = math.NaN() },
		"neg-base":     func(p *model.RevisionProposal) { p.BaseRevision = -1 },
		"empty-tasks":  func(p *model.RevisionProposal) { p.Tasks = nil },
		"empty-task-id": func(p *model.RevisionProposal) {
			p.Tasks = accEdit(tasks, "c", func(x *model.Task) { x.ID = "" })
		},
	}
	for name, mut := range bad {
		p := accGood("bad-"+name, 0, tasks)
		mut(&p)
		if err := QueueRevision(root, p); err == nil {
			t.Errorf("%s: invalid proposal accepted by queueRevision", name)
		}
	}
	if len(runfixture.Read(t, root).Proposals) != 0 {
		t.Fatalf("invalid proposals left in queue: %#v", runfixture.Read(t, root).Proposals)
	}
}

func TestAcceptanceApplyPreservesEvidenceAndAdvancesGenerations(t *testing.T) {
	root := accGraph(t)
	before := runfixture.Read(t, root)
	oldB, oldC := before.Tasks["b"].OperationID, before.Tasks["c"].OperationID
	p := accGood("p1", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "better prompt" }))
	if err := accSubmit(t, root, p); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 1 || len(s.Revisions) != 1 {
		t.Fatalf("revision not committed: rev=%d revs=%d decisions=%#v", s.GraphRevision, len(s.Revisions), s.Decisions)
	}
	for _, id := range []string{"a", "d"} {
		if !reflect.DeepEqual(s.Tasks[id], before.Tasks[id]) {
			t.Fatalf("unaffected verified %s changed: %#v -> %#v", id, before.Tasks[id], s.Tasks[id])
		}
	}
	b := s.Tasks["b"]
	if b.Generation != before.Tasks["b"].Generation+1 || b.OperationID != model.OperationID(s.RunID, "b", b.Generation) ||
		b.OperationID == oldB {
		t.Fatalf("b generation/operation wrong: %#v", b)
	}
	if b.Attempt != 2 || b.Cost != 0 || b.Spec.Prompt != "better prompt" {
		t.Fatalf("b must keep attempt, reset cost, take new spec: %#v", b)
	}
	if c := s.Tasks["c"]; c.Generation != before.Tasks["c"].Generation+1 || c.OperationID == oldC {
		t.Fatalf("dependency consumer c not regenerated: %#v", c)
	}
	arch := map[string]int{}
	var archCost float64
	for _, a := range s.ArchivedTasks {
		arch[a.Spec.ID]++
		archCost += a.Cost
	}
	if arch["b"] != 1 || arch["c"] != 1 || arch["a"] != 0 || arch["d"] != 0 {
		t.Fatalf("archive must hold affected old states exactly once: %v", arch)
	}
	if math.Abs(model.AdaptiveTotalCost(s)-0.35) > 1e-9 {
		t.Fatalf("total cost must include archived spend once: %v", model.AdaptiveTotalCost(s))
	}
	rec := s.Revisions[0]
	retired := strings.Join(rec.RetiredOperations, ",")
	if !strings.Contains(retired, oldB) || !strings.Contains(retired, oldC) ||
		rec.ProposalID != "p1" || rec.Revision != 1 || rec.ParentRevision != 0 ||
		rec.BeforeHash == "" || rec.AfterHash == "" || rec.BeforeHash == rec.AfterHash ||
		rec.PredictedGainMS != 4990 || rec.CostAtCommitUSD < 0.35-1e-9 || rec.At.IsZero() {
		t.Fatalf("revision record incomplete: %#v", rec)
	}
	if d := accDecision(s, "p1"); d == nil || d.Action != "apply" || d.AppliedRevision != 1 {
		t.Fatalf("apply decision missing: %#v", d)
	}
	if s.PlanHash != before.PlanHash {
		t.Fatal("original plan hash must stay immutable")
	}
	if err := checkpoint.ValidateRevisionState(s); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptanceRepeatedPollingIsIdempotent(t *testing.T) {
	root := accGraph(t)
	p := accGood("p1", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "v2" }))
	if err := accSubmit(t, root, p); err != nil {
		t.Fatal(err)
	}
	first := accSnapshot(t, runfixture.Read(t, root))
	events := len(runfixture.Read(t, root).Events)
	for i := 0; i < 5; i++ {
		if err := ProcessRevisionProposals(root); err != nil {
			t.Fatal(err)
		}
	}
	s := runfixture.Read(t, root)
	if accSnapshot(t, s) != first || len(s.Events) != events {
		t.Fatal("polling after apply changed state or logged more events")
	}
	// re-queueing the already-applied proposal must not re-apply it
	_ = QueueRevision(root, p)
	_ = ProcessRevisionProposals(root)
	if got := runfixture.Read(t, root); got.GraphRevision != 1 || got.Tasks["b"].Generation != 2 {
		t.Fatalf("applied proposal replayed: rev=%d gen=%d", got.GraphRevision, got.Tasks["b"].Generation)
	}
}

func TestAcceptanceNoOpAndReorderDoNotIncrementRevision(t *testing.T) {
	root := accGraph(t)
	tasks := accActive(t, root)
	rev := append([]model.Task{}, tasks...)
	rev[0], rev[len(rev)-1] = rev[len(rev)-1], rev[0]
	before := runfixture.Read(t, root)
	_ = accSubmit(t, root, accGood("same", 0, tasks))
	_ = accSubmit(t, root, accGood("reorder", 0, rev))
	s := runfixture.Read(t, root)
	if s.GraphRevision != 0 || len(s.Revisions) != 0 || len(s.ArchivedTasks) != 0 {
		t.Fatalf("no-op revised graph: rev=%d archived=%d", s.GraphRevision, len(s.ArchivedTasks))
	}
	for id, row := range before.Tasks {
		if !reflect.DeepEqual(row, s.Tasks[id]) {
			t.Fatalf("no-op changed %s", id)
		}
	}
}

func TestAcceptanceStaleBaseRevisionRejected(t *testing.T) {
	root := accGraph(t)
	if err := accSubmit(t, root, accGood("p1", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "v2" }))); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).GraphRevision != 1 {
		t.Fatal("setup revision failed")
	}
	// second proposal still based on revision 0
	accRejected(t, root, accGood("stale", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "v3" })))
	if got := runfixture.Read(t, root).Tasks["b"].Spec.Prompt; got != "v2" {
		t.Fatalf("stale proposal overwrote newer graph: %q", got)
	}
	// future base revision likewise
	accRejected(t, root, accGood("future", 9, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "v4" })))
}

func TestAcceptanceControlNotRunDoesNotConsume(t *testing.T) {
	root := accGraph(t)
	if err := checkpoint.Update(root, func(s *model.State) error { s.Control = "paused"; return nil }); err != nil {
		t.Fatal(err)
	}
	p := accGood("p1", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "v2" }))
	if err := QueueRevision(root, p); err != nil {
		t.Fatal(err)
	}
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 0 || len(s.Proposals) != 1 {
		t.Fatalf("proposal consumed while not running: rev=%d queue=%d", s.GraphRevision, len(s.Proposals))
	}
}

func TestAcceptanceInvalidTopologyRejectedWithoutMutation(t *testing.T) {
	root := accGraph(t)
	tasks := accActive(t, root)
	cases := map[string][]model.Task{
		"cycle":       accEdit(accEdit(tasks, "b", func(x *model.Task) { x.DependsOn = []string{"c"} }), "c", func(x *model.Task) { x.DependsOn = []string{"b"} }),
		"self":        accEdit(tasks, "c", func(x *model.Task) { x.DependsOn = []string{"c"} }),
		"missing-dep": accEdit(tasks, "c", func(x *model.Task) { x.DependsOn = []string{"ghost"} }),
		"duplicate":   append(append([]model.Task{}, tasks...), tasks[len(tasks)-1]),
		"bad-parent":  append(append([]model.Task{}, tasks...), model.Task{ID: "h", Prompt: "p", Requires: []string{"answer"}, Parent: "ghost"}),
		"bad-context": accEdit(tasks, "c", func(x *model.Task) { x.Context = []string{"no-such-context"} }),
	}
	i := 0
	for name, ts := range cases {
		i++
		t.Run(name, func(t *testing.T) { accRejected(t, root, accGood("topo-"+name, 0, ts)) })
	}
	// after all failures a legitimate revision still applies
	if err := accSubmit(t, root, accGood("ok", 0, accEdit(tasks, "c", func(x *model.Task) { x.Prompt = "fine" }))); err != nil {
		t.Fatal(err)
	}
	if s := runfixture.Read(t, root); s.GraphRevision != 1 || s.Tasks["c"].Spec.Prompt != "fine" {
		t.Fatalf("recovery after rejected proposals failed: rev=%d decisions=%#v", s.GraphRevision, s.Decisions)
	}
}

func TestAcceptanceWeakeningAndCapabilityExpansionRejected(t *testing.T) {
	c := runfixture.Task("c", .01, "b")
	c.Equals = map[string]string{"answer": "yes"}
	root := runfixture.New(t, runfixture.Task("a", .01), runfixture.Task("b", .01, "a"), c, runfixture.Task("d", .01))
	if err := checkpoint.Update(root, func(s *model.State) error { s.Control = "run"; return nil }); err != nil {
		t.Fatal(err)
	}
	tasks := accActive(t, root)
	cases := map[string][]model.Task{
		"drop-requires": accEdit(tasks, "c", func(x *model.Task) { x.Requires = nil }),
		"drop-equals":   accEdit(tasks, "c", func(x *model.Task) { x.Equals = nil }),
		"change-equals": accEdit(tasks, "c", func(x *model.Task) { x.Equals = map[string]string{"answer": "anything"} }),
		"new-provider":  accEdit(tasks, "c", func(x *model.Task) { x.Provider = "unlisted-provider" }),
		"new-effort":    accEdit(tasks, "c", func(x *model.Task) { x.Effort = "max-unlisted" }),
		"new-context":   accEdit(tasks, "c", func(x *model.Task) { x.Context = []string{"unlisted"} }),
		"exec-check": accEdit(tasks, "c", func(x *model.Task) {
			x.Checks = append(x.Checks, model.Check{Kind: "command", Command: []string{"true"}})
		}),
		"helper-new-provider": append(append([]model.Task{}, tasks...), model.Task{ID: "h", Prompt: "p", Requires: []string{"answer"}, Provider: "unlisted-provider"}),
	}
	for name, ts := range cases {
		t.Run(name, func(t *testing.T) { accRejected(t, root, accGood("weak-"+name, 0, ts)) })
	}
}

func TestAcceptanceVerifiedAffectedNodeRejectedWithEvidenceKept(t *testing.T) {
	root := accGraph(t)
	before := runfixture.Read(t, root)
	accRejected(t, root, accGood("touch-verified", 0, accEdit(accActive(t, root), "a", func(x *model.Task) { x.Prompt = "rewrite history" })))
	s := runfixture.Read(t, root)
	if !reflect.DeepEqual(s.Tasks["a"], before.Tasks["a"]) || s.Tasks["a"].Result["answer"] != "evidence-a" {
		t.Fatalf("verified evidence lost: %#v", s.Tasks["a"])
	}
	// changing a verified node's dependency consumer closure also hits verified a->... ; removal of verified d
	var without []model.Task
	for _, tk := range accActive(t, root) {
		if tk.ID != "d" {
			without = append(without, tk)
		}
	}
	accRejected(t, root, accGood("drop-verified", 0, without))
	if runfixture.Read(t, root).Tasks["d"] == nil {
		t.Fatal("verified task removed")
	}
}

func TestAcceptanceUncertainOutcomeBlocksRevision(t *testing.T) {
	root := accGraph(t)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["b"].Status = "uncertain"
		s.Tasks["b"].Error = "provider outcome unknown"
		s.Tasks["b"].Result = map[string]string{"partial": "keep-me"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	accRejected(t, root, accGood("p1", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "retry" })))
	b := runfixture.Read(t, root).Tasks["b"]
	if b.Status != "uncertain" || b.Result["partial"] != "keep-me" || b.Error == "" {
		t.Fatalf("uncertain evidence destroyed: %#v", b)
	}
}

func TestAcceptanceProducedAndRunningAreDeferredNotDiscarded(t *testing.T) {
	for _, status := range []string{"produced", "running"} {
		t.Run(status, func(t *testing.T) {
			root := accGraph(t)
			if err := checkpoint.Update(root, func(s *model.State) error {
				s.Tasks["b"].Status = status
				s.Tasks["b"].Result = map[string]string{"answer": "unverified-output"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := accSnapshot(t, runfixture.Read(t, root))
			p := accGood("p1", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "v2" }))
			if err := accSubmit(t, root, p); err != nil {
				t.Fatal(err)
			}
			s := runfixture.Read(t, root)
			if s.GraphRevision != 0 || s.Tasks["b"].Result["answer"] != "unverified-output" || len(s.ArchivedTasks) != 0 {
				t.Fatalf("in-flight work was replaced: %s", accSnapshot(t, s))
			}
			if d := accDecision(s, "p1"); d != nil && d.Action == "apply" {
				t.Fatal("applied over in-flight work")
			}
			_ = before
		})
	}
}

func TestAcceptanceLiveLeaseDefersWithoutLogSpamThenApplies(t *testing.T) {
	root := accGraph(t)
	lease, err := checkpoint.FileLock(filepath.Join(root, "cells", "b", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			checkpoint.Unlock(lease)
		}
	}()
	// an unrelated task's lease is irrelevant; hold only b's.
	p := accGood("p1", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "v2" }))
	if err := accSubmit(t, root, p); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 0 || s.Tasks["b"].Generation != 1 || s.Tasks["b"].Spec.Prompt == "v2" {
		t.Fatal("revision applied under live lease")
	}
	if d := accDecision(s, "p1"); d != nil && d.Action == "apply" {
		t.Fatal("apply decision under live lease")
	}
	if len(s.Proposals) != 1 {
		t.Fatalf("deferred proposal must remain queued, got %d", len(s.Proposals))
	}
	baseEvents, baseDecisions := len(runfixture.Read(t, root).Events), len(runfixture.Read(t, root).Decisions)
	for i := 0; i < 10; i++ {
		if err := ProcessRevisionProposals(root); err != nil {
			t.Fatal(err)
		}
	}
	s = runfixture.Read(t, root)
	if len(s.Events) > baseEvents+1 || len(s.Decisions) > baseDecisions+1 {
		t.Fatalf("repeated deferral spam: events %d->%d decisions %d->%d", baseEvents, len(s.Events), baseDecisions, len(s.Decisions))
	}
	checkpoint.Unlock(lease)
	held = false
	if err := ProcessRevisionProposals(root); err != nil {
		t.Fatal(err)
	}
	if s = runfixture.Read(t, root); s.GraphRevision != 1 || s.Tasks["b"].Generation != 2 {
		t.Fatalf("deferred proposal not applied after lease release: %#v", s.Decisions)
	}
	// leases taken during processing must be released
	l2, err := checkpoint.FileLock(filepath.Join(root, "cells", "b", "lease"), true)
	if err != nil {
		t.Fatalf("lease leaked after revision: %v", err)
	}
	checkpoint.Unlock(l2)
}

func TestAcceptanceRetiredIDsCannotBeRevived(t *testing.T) {
	root := accGraph(t)
	if err := checkpoint.Update(root, func(s *model.State) error {
		p := DefaultAdaptivePolicy()
		p.CooldownMS = 0
		s.Policy = &p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := accSubmit(t, root, accGood("add-helper", 0, append(accActive(t, root), runfixture.Task("helper", .01)))); err != nil {
		t.Fatal(err)
	}
	var withoutC []model.Task
	for _, tk := range accActive(t, root) {
		if tk.ID != "helper" {
			withoutC = append(withoutC, tk)
		}
	}
	if err := accSubmit(t, root, accGood("drop", 1, withoutC)); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 2 || s.Tasks["helper"] != nil {
		t.Fatalf("setup: helper not retired: rev=%d decisions=%#v", s.GraphRevision, s.Decisions)
	}
	archived := 0
	for _, a := range s.ArchivedTasks {
		if a.Spec.ID == "helper" {
			archived++
		}
	}
	if archived != 1 {
		t.Fatalf("retired helper archived %d times", archived)
	}
	accRejected(t, root, accGood("revive", 2, append(accActive(t, root), runfixture.Task("helper", .01))))
}

func TestAcceptanceNestedOwnershipAndChildrenRecomputed(t *testing.T) {
	parent := runfixture.Task("p", .01)
	parent.Spawn = &model.SpawnPolicy{Field: "answer", MaxChildren: 2}
	root := runfixture.New(t, parent, runfixture.Task("q", .01), runfixture.Task("w", .01))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "run"
		policy := DefaultAdaptivePolicy()
		policy.CooldownMS = 0
		s.Policy = &policy
		s.Tasks["q"].Status = "verified"
		s.Tasks["q"].Result = map[string]string{"answer": "q-ev"}
		s.Tasks["p"].Status = "produced"
		s.Tasks["p"].Result = map[string]string{"answer": `{"tasks":[{"id":"h1","prompt":"nested owner","requires":["answer"],"spawn":{"field":"answer","max_children":2}}]}`}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := commitExpansion(root, "p", "contract"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["p__h1"].Status = "produced"
		s.Tasks["p__h1"].Result = map[string]string{"answer": `{"tasks":[{"id":"n","prompt":"nested leaf","requires":["answer"]}]}`}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := commitExpansion(root, "p__h1", "contract"); err != nil {
		t.Fatal(err)
	}
	helper := model.Task{ID: "p__h1__extra", Prompt: "helper", Requires: []string{"answer"}, Parent: "p__h1", Provider: "mock"}
	tasks := append(accActive(t, root), helper)
	if err := accSubmit(t, root, accGood("grow", 0, tasks)); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.GraphRevision != 1 || s.Tasks["p__h1"] == nil || s.Tasks["p__h1__extra"] == nil {
		t.Fatalf("helper nodes not added: rev=%d decisions=%#v", s.GraphRevision, s.Decisions)
	}
	if !reflect.DeepEqual(s.Tasks["p"].Children, []string{"p__h1"}) || !reflect.DeepEqual(s.Tasks["p__h1"].Children, []string{"p__h1__n", "p__h1__extra"}) {
		t.Fatalf("children not recomputed from parent links: %v %v", s.Tasks["p"].Children, s.Tasks["p__h1"].Children)
	}
	if s.Tasks["q"].Status != "verified" || s.Tasks["q"].Result["answer"] != "q-ev" || s.Tasks["q"].Generation != 1 {
		t.Fatalf("unrelated verified subtree disturbed: %#v", s.Tasks["q"])
	}
	if s.Tasks["p__h1__extra"].Generation != 1 || s.Tasks["p__h1__extra"].Status != "pending" {
		t.Fatalf("new helper should start at generation 1 pending: %#v", s.Tasks["p__h1__extra"])
	}
	if err := checkpoint.ValidateRevisionState(s); err != nil {
		t.Fatal(err)
	}
	// Adaptive additions retain the owner's original per-parent spawn limit.
	tooMany := model.Task{ID: "p__h1__overflow", Prompt: "extra helper", Requires: []string{"answer"}, Parent: "p__h1", Provider: "mock"}
	accRejected(t, root, accGood("child-limit", 1, append(accActive(t, root), tooMany)))
	if d := accDecision(runfixture.Read(t, root), "child-limit"); d == nil || d.Action != "reject" || !strings.Contains(d.Reason, "max_children") {
		t.Fatalf("child limit not explicitly enforced: %#v", d)
	}
	// ownership cycle / re-parenting under a descendant is rejected
	accRejected(t, root, accGood("own-cycle", 1, accEdit(accActive(t, root), "p", func(x *model.Task) { x.Parent = "p__h1__n" })))
	// a nested owner can't be orphaned silently
	accRejected(t, root, accGood("orphan", 1, accEdit(accActive(t, root), "p__h1", func(x *model.Task) { x.Parent = "gone" })))
}

func TestAcceptanceCorruptedRevisionStateDetected(t *testing.T) {
	root := accGraph(t)
	if err := accSubmit(t, root, accGood("p1", 0, accEdit(accActive(t, root), "b", func(x *model.Task) { x.Prompt = "v2" }))); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	bad := map[string]func(*model.State){
		"revision-without-record": func(x *model.State) { x.Revisions = nil },
		"missing-task-state":      func(x *model.State) { delete(x.Tasks, "d") },
		"extra-task-state":        func(x *model.State) { x.Tasks["zz"] = x.NewTask(runfixture.Task("zz", 0)) },
		"duplicate-archive": func(x *model.State) {
			if len(x.ArchivedTasks) > 0 {
				x.ArchivedTasks = append(x.ArchivedTasks, x.ArchivedTasks[0])
			} else {
				x.ArchivedTasks = []model.TaskState{*x.Tasks["a"], *x.Tasks["a"]}
			}
		},
	}
	for name, mut := range bad {
		raw, _ := json.Marshal(s)
		var c model.State
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		mut(&c)
		if err := checkpoint.ValidateRevisionState(&c); err == nil {
			t.Errorf("%s: corrupted state accepted", name)
		}
	}
}

func TestAcceptanceProposalQueueBounded(t *testing.T) {
	root := accGraph(t)
	tasks := accActive(t, root)
	accepted := 0
	for i := 0; i < 200; i++ {
		p := accGood("q"+string(rune('A'+i%26))+strings.Repeat("x", i/26), 0, tasks)
		if QueueRevision(root, p) == nil {
			accepted++
		}
	}
	if n := len(runfixture.Read(t, root).Proposals); n > 64 || accepted > 64 {
		t.Fatalf("proposal queue unbounded: stored=%d accepted=%d", n, accepted)
	}
}
