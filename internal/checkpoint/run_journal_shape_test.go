package checkpoint

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func runShapeRootTasks(n int) []model.Task {
	tasks := make([]model.Task, 0, n)
	for i := 1; i <= n; i++ {
		tasks = append(tasks, task(fmt.Sprintf("root%02d", i), 0))
	}
	return tasks
}

// 992 is a declared ceiling (32 roots, 5 total levels, 2 children). Only the 32
// roots exist at initialization; metadata grants no spawn capability.
func TestRunShapeDeclaredShape992IsCeilingNotActualCount(t *testing.T) {
	p := model.Plan{Name: "shape992", Tasks: runShapeRootTasks(32), MaxTasks: 992, MaxDepth: 4, ObserverShape: &model.ObserverShape{Roots: 32, Levels: 5, MaxChildren: 2}}
	for i := range p.Tasks {
		p.Tasks[i].Requires = append(p.Tasks[i].Requires, "children")
		p.Tasks[i].Spawn = &model.SpawnPolicy{Field: "children", MaxChildren: 2}
	}
	if err := ValidateObserverShape(p); err != nil {
		t.Fatal(err)
	}
	if n, err := ObsShapeCeiling(*p.ObserverShape); err != nil || n != 992 {
		t.Fatalf("ceiling %d %v, want 992", n, err)
	}
	root := filepath.Join(t.TempDir(), "run")
	if err := Initialize(p, root, "mock"); err != nil {
		t.Fatal(err)
	}
	s := read(t, root)
	if len(s.Tasks) != 32 || len(s.Dynamic) != 0 {
		t.Fatalf("actual tasks %d dynamic %d; declared ceiling must not materialize tasks", len(s.Tasks), len(s.Dynamic))
	}
	wrongRoots, tooDeep := p, p
	shape := *p.ObserverShape
	shape.Roots = 31
	wrongRoots.ObserverShape = &shape
	if ValidateObserverShape(wrongRoots) == nil {
		t.Fatal("accepted shape whose roots differ from plan roots")
	}
	deep := *p.ObserverShape
	deep.Levels = 7
	tooDeep.ObserverShape = &deep
	if ValidateObserverShape(tooDeep) == nil {
		t.Fatal("accepted shape deeper than max_depth")
	}
}

// The 2x1x2 fixture is a different shape and must never be conflated with 992.
func TestRunShapeTwoByOneByTwoShapeHasOnlyTwoRoots(t *testing.T) {
	p := model.Plan{Name: "shape2", Tasks: runShapeRootTasks(2), ObserverShape: &model.ObserverShape{Roots: 2, Levels: 1, MaxChildren: 2}}
	if err := ValidateObserverShape(p); err != nil {
		t.Fatal(err)
	}
	if n, err := ObsShapeCeiling(*p.ObserverShape); err != nil || n != 2 {
		t.Fatalf("ceiling %d %v, want 2", n, err)
	}
	root := filepath.Join(t.TempDir(), "run")
	if err := Initialize(p, root, "mock"); err != nil {
		t.Fatal(err)
	}
	for id, row := range read(t, root).Tasks {
		if row.Spec.Parent != "" {
			t.Fatalf("%s has owner %s in a no-children shape", id, row.Spec.Parent)
		}
	}
}
