package checkpoint

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func capacityTasks(n int, reverseChain bool) []model.Task {
	tasks := make([]model.Task, n)
	for i := range tasks {
		id := fmt.Sprintf("task_%05d", i)
		tasks[i] = task(id, 0)
		if reverseChain && i+1 < n {
			tasks[i].DependsOn = []string{fmt.Sprintf("task_%05d", i+1)}
		}
	}
	return tasks
}

func TestGraphCapacityAdmissionAndReverseOrderedDependencies(t *testing.T) {
	for _, n := range []int{900, 2000, model.MaxGraphTasks} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			p := model.Plan{MaxTasks: n, Tasks: capacityTasks(n, true)}
			if err := Validate(p); err != nil {
				t.Fatal(err)
			}
			p.Tasks[n-1].DependsOn = []string{p.Tasks[0].ID}
			if err := Validate(p); err == nil || !strings.Contains(err.Error(), "dependency cycle") {
				t.Fatalf("large dependency cycle accepted: %v", err)
			}
		})
	}
}

func TestGraphCapacityPreservesDefaultAndRejectsOverflow(t *testing.T) {
	p := model.Plan{Tasks: capacityTasks(model.DefaultGraphTasks+1, false)}
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "exceeds max_tasks") {
		t.Fatalf("omitted ceiling changed: %v", err)
	}
	p.MaxTasks = model.MaxGraphTasks
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{model.MaxGraphTasks + 1, math.MaxInt, -1} {
		p.MaxTasks = n
		if err := Validate(p); err == nil {
			t.Fatalf("max_tasks %d accepted", n)
		}
	}
	for _, sh := range []Shape{{Roots: math.MaxInt, Levels: 16, MaxChildren: math.MaxInt}, {Roots: 1, Levels: 2, MaxChildren: math.MaxInt}} {
		if _, err := ShapeMaxNodes(sh); err == nil {
			t.Fatalf("overflow shape accepted: %+v", sh)
		}
	}
}

func TestGraphCapacityObserverAndRevisionShareCeiling(t *testing.T) {
	p := model.Plan{MaxTasks: model.MaxGraphTasks, Tasks: capacityTasks(model.MaxGraphTasks, false), ObserverShape: &model.ObserverShape{Roots: model.MaxGraphTasks, Levels: 1, MaxChildren: 1}}
	if err := ValidateObserverShape(p); err != nil {
		t.Fatal(err)
	}
	if n, err := ObsShapeCeiling(*p.ObserverShape); err != nil || n != model.MaxGraphTasks || MaxRevTasks != model.MaxGraphTasks {
		t.Fatalf("inconsistent graph ceilings: n=%d revision=%d error=%v", n, MaxRevTasks, err)
	}
	p.MaxTasks++
	if err := ValidateObserverShape(p); err == nil {
		t.Fatal("observer accepted out-of-range allocation")
	}
}
