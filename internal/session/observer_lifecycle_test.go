package session

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func lcGet(t *testing.T, m map[string]*observer.PanelLifecycle, id string) *observer.PanelLifecycle {
	t.Helper()
	l := m[id]
	if l == nil {
		t.Fatalf("no lifecycle for %s", id)
	}
	return l
}

func TestLifecycleThreeLevelJoinAndRevision(t *testing.T) {
	r := runfixture.LcRow("r", "", "waiting", "task-checks", nil, "r__m")
	m := runfixture.LcRow("r__m", "r", "waiting", "structural", nil, "r__m__a", "r__m__b")
	a := runfixture.LcRow("r__m__a", "r__m", "verified", "structural", nil)
	b := runfixture.LcRow("r__m__b", "r__m", "produced", "", nil)
	s := runfixture.LcState(r, m, b, a)
	lc := observer.ProjectLifecycles(s)
	g := lcGet(t, lc, "r")
	if g.Join != "waiting_children" || g.ValidationLevel != "task-checks" || g.ExecutionStatus != "waiting" || len(g.UnfinishedChildIDs) != 1 || g.UnfinishedChildIDs[0] != "r__m" {
		t.Fatalf("root must wait on its unverified child despite own validation: %+v", g)
	}
	if mm := lcGet(t, lc, "r__m"); mm.Join != "waiting_children" || len(mm.UnfinishedChildIDs) != 1 || mm.UnfinishedChildIDs[0] != "r__m__b" {
		t.Fatalf("one produced child must block the join: %+v", mm)
	}
	if l := lcGet(t, lc, "r__m__a"); l.Join != "not_applicable" || l.DependencyGate != "satisfied" {
		t.Fatalf("leaf: %+v", l)
	}
	b.Status = "verified"
	lc = observer.ProjectLifecycles(s)
	if mm := lcGet(t, lc, "r__m"); mm.Join != "satisfied" || mm.ExecutionStatus != "waiting" {
		t.Fatalf("join satisfied but status not yet reconciled: %+v", mm)
	}
	if g := lcGet(t, lc, "r"); g.Join != "waiting_children" || g.UnfinishedChildIDs[0] != "r__m" {
		t.Fatalf("outer join must still wait for the middle level: %+v", g)
	}
	m.Status = "verified"
	if g := lcGet(t, observer.ProjectLifecycles(s), "r"); g.Join != "satisfied" {
		t.Fatalf("all levels verified: %+v", g)
	}
	// Revision: the same task ID gets a new generation and cannot inherit verification.
	s.ArchivedTasks = []model.TaskState{*b}
	nt := s.NewTask(model.Task{ID: "r__m__b", Parent: "r__m"})
	s.Tasks["r__m__b"] = nt
	m.Status = "waiting"
	lc = observer.ProjectLifecycles(s)
	nb := lcGet(t, lc, "r__m__b")
	if nb.Generation != 2 || nb.OperationID == b.OperationID || nb.ValidationLevel != "none" || nb.ExecutionStatus != "pending" {
		t.Fatalf("revised generation: %+v", nb)
	}
	if mm := lcGet(t, lc, "r__m"); mm.Join != "waiting_children" || mm.UnfinishedChildIDs[0] != "r__m__b" {
		t.Fatalf("revised child must block join again: %+v", mm)
	}
}

func TestLifecycleDependencyGateIsNotAdmission(t *testing.T) {
	a := runfixture.LcRow("a", "", "verified", "structural", nil)
	b := runfixture.LcRow("b", "", "pending", "", []string{"a"})
	c := runfixture.LcRow("c", "", "pending", "", []string{"a", "b", "b"})
	d := runfixture.LcRow("d", "", "failed", "", []string{"ghost"})
	e := runfixture.LcRow("e", "", "pending", "", nil)
	e.CancelRequested = true
	f := runfixture.LcRow("f", "", "weird", "bogus", nil)
	s := runfixture.LcState(a, b, c, d, e, f)
	s.Control = "pause"
	lc := observer.ProjectLifecycles(s)
	gb := lcGet(t, lc, "b")
	if gb.DependencyGate != "satisfied" || gb.ExecutionStatus != "pending" || gb.Admission != "not_evaluated" || gb.Join != "not_applicable" {
		t.Fatalf("dependency-ready must not claim admission or execution: %+v", gb)
	}
	gc := lcGet(t, lc, "c")
	if gc.DependencyGate != "blocked" || gc.BlockingDependenciesTotal != 1 || gc.BlockingDependencyIDs[0] != "b" || gc.Join != "not_applicable" {
		t.Fatalf("same-depth dependency is not ownership: %+v", gc)
	}
	if ga := lcGet(t, lc, "a"); ga.UnfinishedChildrenTotal != 0 || ga.Join != "not_applicable" {
		t.Fatalf("independent roots must not become children: %+v", ga)
	}
	gd := lcGet(t, lc, "d")
	if gd.DependencyGate != "unknown" || gd.MissingDependenciesTotal != 1 || gd.BlockingDependenciesTotal != 0 || gd.ExecutionStatus != "failed" {
		t.Fatalf("missing dependency must fail closed and stay distinct: %+v", gd)
	}
	ge := lcGet(t, lc, "e")
	if !ge.CancelRequested || ge.DependencyGate != "satisfied" {
		t.Fatalf("cancel is separate from the gate: %+v", ge)
	}
	if gf := lcGet(t, lc, "f"); gf.ExecutionStatus != "unknown" || gf.ValidationLevel != "none" && gf.ValidationLevel != "unknown" {
		t.Fatalf("unknown status: %+v", gf)
	}
	raw, _ := json.Marshal(lc)
	for _, banned := range []string{"lease", "busy"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("lifecycle must not invent %s state", banned)
		}
	}
}

func TestLifecycleMissingChildAndCaps(t *testing.T) {
	p := runfixture.LcRow("p", "", "waiting", "structural", nil, "p__gone")
	if g := lcGet(t, observer.ProjectLifecycles(runfixture.LcState(p)), "p"); g.Join != "unknown" {
		t.Fatalf("missing child must fail closed: %+v", g)
	}
	kids := []string{}
	rows := []*model.TaskState{}
	for i := 0; i < 12; i++ {
		id := "w__k" + string(rune('a'+i))
		kids = append(kids, id)
		rows = append(rows, runfixture.LcRow(id, "w", "pending", "", nil))
	}
	rows = append(rows, runfixture.LcRow("w", "", "waiting", "structural", nil, kids...))
	w := lcGet(t, observer.ProjectLifecycles(runfixture.LcState(rows...)), "w")
	if w.UnfinishedChildrenTotal != 12 || len(w.UnfinishedChildIDs) != model.LifecycleMaxIDs || w.UnfinishedChildrenOmitted != 12-model.LifecycleMaxIDs {
		t.Fatalf("bounded ids with counts: %+v", w)
	}
}

func TestLifecycleOnSnapshotNodesAndTopology(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	plan := model.Plan{Name: "n", Tasks: []model.Task{{ID: "a", Prompt: "p", Requires: []string{"x"}}}}
	if err := checkpoint.Initialize(plan, root, "mock"); err != nil {
		t.Fatal(err)
	}
	raw, err := livePanelSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Nodes []struct {
			ID        string
			Lifecycle *observer.PanelLifecycle
		}
		Topology struct {
			Nodes []struct {
				ID        string
				Lifecycle *observer.PanelLifecycle
			}
		}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 1 || len(out.Topology.Nodes) != 1 {
		t.Fatalf("nodes=%d topology=%d", len(out.Nodes), len(out.Topology.Nodes))
	}
	for _, l := range []*observer.PanelLifecycle{out.Nodes[0].Lifecycle, out.Topology.Nodes[0].Lifecycle} {
		if l == nil || l.Generation != 1 || l.DependencyGate != "satisfied" || l.Join != "not_applicable" || l.ExecutionStatus != "pending" || l.ValidationLevel != "none" || l.OperationID == "" {
			t.Fatalf("lifecycle must be on selected nodes and topology nodes: %+v", l)
		}
	}
}
