package observer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

type legacyPlanForHash struct {
	APIVersion string                       `json:"api_version,omitempty"`
	Name       string                       `json:"name"`
	Tasks      []model.Task                 `json:"tasks"`
	Context    map[string]string            `json:"context,omitempty"`
	Providers  map[string]model.CommandSpec `json:"providers,omitempty"`
	MaxTasks   int                          `json:"max_tasks,omitempty"`
	MaxDepth   int                          `json:"max_depth,omitempty"`
	Display    *model.PlanDisplay           `json:"display,omitempty"`
}

func shapePlan(roots int, spawnCap int, shape *model.ObserverShape) model.Plan {
	p := model.Plan{Name: "n", ObserverShape: shape}
	for i := 0; i < roots; i++ {
		t := model.Task{ID: fmt.Sprintf("r%d", i), Prompt: "x", Requires: []string{"out"}}
		if spawnCap >= 0 {
			t.Spawn = &model.SpawnPolicy{Field: "out", MaxChildren: spawnCap}
		}
		p.Tasks = append(p.Tasks, t)
	}
	return p
}

// initialCheck is the initial load/initialize validation path.
func initialCheck(p model.Plan) error {
	if err := checkpoint.Validate(p); err != nil {
		return err
	}
	return checkpoint.ValidateObserverShape(p)
}

func wantErr(t *testing.T, p model.Plan, sub string) {
	t.Helper()
	err := initialCheck(p)
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want error containing %q, got %v", sub, err)
	}
}

func actualState(p model.Plan, extra ...model.Task) *model.State {
	s := &model.State{Plan: p, Tasks: map[string]*model.TaskState{}}
	all := append(append([]model.Task{}, p.Tasks...), extra...)
	for _, tk := range all {
		s.ActiveTasks = append(s.ActiveTasks, tk)
		s.Tasks[tk.ID] = &model.TaskState{Spec: tk, Status: "pending"}
	}
	return s
}

func child(parent string, n int) model.Task {
	return model.Task{ID: fmt.Sprintf("%s__c%d", parent, n), Parent: parent, Prompt: "x", Requires: []string{"out"}}
}

func TestLegacyPlanBytesUnchanged(t *testing.T) {
	p := shapePlan(2, -1, nil)
	p.MaxTasks = 5
	got, _ := json.Marshal(p)
	want, _ := json.Marshal(legacyPlanForHash{Name: p.Name, Tasks: p.Tasks, MaxTasks: 5})
	if string(got) != string(want) || strings.Contains(string(got), "observer_shape") {
		t.Fatalf("legacy bytes changed: %s vs %s", got, want)
	}
}

func TestCanonicalD1AndD17(t *testing.T) {
	for _, s := range []int{1, 2} {
		p := shapePlan(2, -1, &model.ObserverShape{Roots: 2, Levels: 1, MaxChildren: s})
		if err := initialCheck(p); err != nil {
			t.Fatalf("D1 S%d: %v", s, err)
		}
		if c := ProjectCapacity(actualState(p)); !c.Known || c.ShapeCeilingNodes != 2 {
			t.Fatalf("%+v", c)
		}
	}
	p := shapePlan(1, 1, &model.ObserverShape{Roots: 1, Levels: 17, MaxChildren: 1})
	p.MaxDepth = 16
	wantErr(t, p, "levels")
}

func TestShape775AndDefaults(t *testing.T) {
	p := shapePlan(25, 2, &model.ObserverShape{Roots: 25, Levels: 5, MaxChildren: 2})
	if err := initialCheck(p); err != nil { // omitted max_depth => 4
		t.Fatal(err)
	}
	c := ProjectCapacity(actualState(p))
	if !c.Known || c.ShapeCeilingNodes != 775 || c.AllocationCapNodes != 128 || c.EffectiveCeilingNodes != 128 || !c.CeilingExceedsAllocationCap {
		t.Fatalf("%+v", c)
	}
	p = shapePlan(1, 2, &model.ObserverShape{Roots: 1, Levels: 6, MaxChildren: 2})
	wantErr(t, p, "effective max_depth")
	p.MaxDepth = 5
	if err := initialCheck(p); err != nil {
		t.Fatal(err)
	}
}

func TestObserverShapeRejections(t *testing.T) {
	wantErr(t, shapePlan(3, 2, &model.ObserverShape{Roots: 4, Levels: 2, MaxChildren: 2}), "root task count")
	wantErr(t, shapePlan(1, 64, &model.ObserverShape{Roots: 1, Levels: 4, MaxChildren: 64}), "1024")
	wantErr(t, shapePlan(2, -1, &model.ObserverShape{Roots: 2, Levels: 2, MaxChildren: 2}), "without spawn")
	wantErr(t, shapePlan(2, 3, &model.ObserverShape{Roots: 2, Levels: 2, MaxChildren: 2}), "spawn cap")
	wantErr(t, shapePlan(2, 0, &model.ObserverShape{Roots: 2, Levels: 2, MaxChildren: 4}), "spawn cap") // default 8 > 4
	wantErr(t, shapePlan(1, 2, &model.ObserverShape{Roots: 0, Levels: 1, MaxChildren: 1}), "positive")
	for _, ok := range []int{8, 9} { // actual cap 8 <= declared
		if err := initialCheck(shapePlan(2, 0, &model.ObserverShape{Roots: 2, Levels: 2, MaxChildren: ok})); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRevisionNotVetoedByObserverMetadata(t *testing.T) {
	// validate() is the generic path used by revisions/expansion.
	p := shapePlan(3, 2, &model.ObserverShape{Roots: 2, Levels: 2, MaxChildren: 2})
	if err := checkpoint.Validate(p); err != nil {
		t.Fatalf("generic validate vetoed by observer metadata: %v", err)
	}
	bad := shapePlan(3, -1, &model.ObserverShape{Roots: -9, Levels: 99, MaxChildren: 0})
	if err := checkpoint.Validate(bad); err != nil {
		t.Fatalf("generic validate must ignore shape: %v", err)
	}
}

func TestCapacityActualStateMismatches(t *testing.T) {
	p := shapePlan(2, 2, &model.ObserverShape{Roots: 2, Levels: 3, MaxChildren: 2})
	if err := initialCheck(p); err != nil {
		t.Fatal(err)
	}
	c := ProjectCapacity(actualState(p, child("r0", 0), child("r1", 0)))
	if !c.Known || c.ShapeCeilingNodes != 14 {
		t.Fatalf("%+v", c)
	}
	extraRoot := model.Task{ID: "r9", Prompt: "x", Requires: []string{"out"}}
	grand := model.Task{ID: "r0__c0__g", Parent: "r0__c0", Prompt: "x", Requires: []string{"out"}}
	great := model.Task{ID: "r0__c0__g__h", Parent: "r0__c0__g", Prompt: "x", Requires: []string{"out"}}
	cases := []struct {
		name, reason string
		s            *model.State
	}{
		{"root added", "root_count_mismatch", actualState(p, extraRoot)},
		{"depth", "depth_exceeds_declared", actualState(p, child("r0", 0), grand, great)},
		{"children", "children_exceed_declared", actualState(p, child("r0", 0), child("r0", 1), child("r0", 2))},
	}
	capState := actualState(p)
	capState.Tasks["r0"].Spec.Spawn = &model.SpawnPolicy{Field: "out", MaxChildren: 5}
	capState.ActiveTasks[0] = capState.Tasks["r0"].Spec
	cases = append(cases, struct {
		name, reason string
		s            *model.State
	}{"cap", "spawn_cap_exceeds_declared", capState})
	for _, tc := range cases {
		c := ProjectCapacity(tc.s)
		if c.Known || c.Reason != tc.reason || c.ShapeCeilingNodes != 0 || c.EffectiveCeilingNodes != 0 || c.Shape == nil {
			t.Fatalf("%s: %+v", tc.name, c)
		}
	}
}

func TestCapacityTaskCountExceeds(t *testing.T) {
	p := shapePlan(1, 2, &model.ObserverShape{Roots: 1, Levels: 2, MaxChildren: 2}) // ceiling 3
	s := actualState(p, child("r0", 0), child("r0", 1))
	if c := ProjectCapacity(s); !c.Known {
		t.Fatalf("%+v", c)
	}
	// duplicate-free over-ceiling total cannot exist with S enforced; force via extra root-free orphan chain is invalid:
	s = actualState(p, child("r0", 0), child("r0", 1), model.Task{ID: "orph", Parent: "nowhere"})
	if c := ProjectCapacity(s); c.Known || c.Reason != "invalid_actual_topology" {
		t.Fatalf("%+v", c)
	}
}

func TestCapacityUnknownCases(t *testing.T) {
	c := ProjectCapacity(actualState(shapePlan(1, -1, nil)))
	if c.Known || c.Shape != nil || c.Reason != "no_explicit_observer_shape" {
		t.Fatalf("%+v", c)
	}
	bad := model.Plan{ObserverShape: &model.ObserverShape{Roots: -1, Levels: 2, MaxChildren: 2}}
	c = ProjectCapacity(&model.State{Plan: bad, Tasks: map[string]*model.TaskState{}}) // must not panic or fabricate
	if c.Known || c.Shape != nil || c.Reason != "invalid_observer_shape" || c.ShapeCeilingNodes != 0 {
		t.Fatalf("%+v", c)
	}
	p := shapePlan(1, 2, &model.ObserverShape{Roots: 1, Levels: 2, MaxChildren: 2})
	s := actualState(p)
	delete(s.Tasks, "r0") // missing row
	if c := ProjectCapacity(s); c.Known || c.Reason != "invalid_actual_topology" {
		t.Fatalf("%+v", c)
	}
	s = actualState(p, model.Task{ID: "a", Parent: "b"}, model.Task{ID: "b", Parent: "a"})
	if c := ProjectCapacity(s); c.Known || c.Reason != "invalid_actual_topology" {
		t.Fatalf("%+v", c)
	}
}

func topoState(roots, kids int) *model.State {
	s := &model.State{Tasks: map[string]*model.TaskState{}}
	add := func(id, parent string) {
		tk := model.Task{ID: id, Parent: parent, Prompt: "SECRET_PROMPT", Context: []string{"SECRET_CTX"}}
		s.ActiveTasks = append(s.ActiveTasks, tk)
		s.Tasks[id] = &model.TaskState{Spec: tk, Status: "pending", Result: map[string]string{"out": "SECRET_RESULT"}}
	}
	for r := 0; r < roots; r++ {
		add(fmt.Sprintf("r%d", r), "")
	}
	for r := 0; r < roots; r++ {
		for k := 0; k < kids; k++ {
			add(fmt.Sprintf("r%d__c%02d", r, k), fmt.Sprintf("r%d", r))
		}
	}
	return s
}

func TestTopologyBounds(t *testing.T) {
	tp := ProjectTopology(topoState(7, 40)) // 7 + 280 = 287 actual nodes
	if tp.NodesTotal != 287 || tp.RootCount != 7 || tp.Shown != 200 || tp.Omitted != 87 {
		t.Fatalf("%+v", tp)
	}
	if tp.StatusCounts["pending"] != 287 || len(tp.StatusCounts) != 1 {
		t.Fatalf("%v", tp.StatusCounts)
	}
	if len(tp.Groups) != 8 || tp.Groups[0].Parent != "" || tp.Groups[0].Depth != 0 || tp.Groups[0].Total != 7 || tp.Groups[0].Omitted != 0 {
		t.Fatalf("%+v", tp.Groups)
	}
	if g := tp.Groups[1]; g.Parent != "r0" || g.Depth != 1 || g.Total != 40 || g.Shown != 32 || g.Omitted != 8 {
		t.Fatalf("%+v", g)
	}
	if g := tp.Groups[7]; g.Shown != 1 || g.Omitted != 39 {
		t.Fatalf("%+v", g)
	}
	if len(tp.DepthCounts) != 2 || tp.DepthCounts[0] != (panelDepthCount{Depth: 0, Count: 7}) || tp.DepthCounts[1] != (panelDepthCount{Depth: 1, Count: 280}) {
		t.Fatalf("%+v", tp.DepthCounts)
	}
	for _, n := range tp.Nodes {
		if n.Parent != "" && n.Depth != 1 {
			t.Fatalf("bad depth %+v", n)
		}
	}
	data, _ := json.Marshal(tp)
	for _, secret := range []string{"SECRET_PROMPT", "SECRET_CTX", "SECRET_RESULT", "planned"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
}

func TestTopologyCycleAndUnknownStatus(t *testing.T) {
	s := &model.State{Tasks: map[string]*model.TaskState{}}
	for _, tk := range []model.Task{{ID: "a", Parent: "b"}, {ID: "b", Parent: "a"}, {ID: "c", Parent: "missing"}, {ID: "ok"}} {
		s.ActiveTasks = append(s.ActiveTasks, tk)
		s.Tasks[tk.ID] = &model.TaskState{Spec: tk, Status: "weird"}
	}
	tp := ProjectTopology(s)
	if tp.NodesTotal != 4 || tp.InvalidNodes != 3 || tp.Shown != 1 || tp.Omitted != 0 || tp.StatusCounts["unknown"] != 4 {
		t.Fatalf("%+v", tp)
	}
}
