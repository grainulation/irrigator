package sizing

import (
	"fmt"
	"math"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestSizingTenThousandIndependentTasks(t *testing.T) {
	tasks := make([]model.Task, model.MaxGraphTasks)
	for i := range tasks {
		tasks[i] = model.Task{ID: fmt.Sprintf("task_%05d", i)}
	}
	c := szCfg(1)
	c.Capacity = 6
	report := sizeReport(tasks, func(string) string { return "pending" }, 0, c, 6)
	if !report.Feasible || report.Tasks != model.MaxGraphTasks || len(report.Rows) != 6 {
		t.Fatalf("large sizing refused: %+v", report)
	}
	for _, row := range report.Rows {
		if row.MakespanS != math.Ceil(float64(model.MaxGraphTasks)/float64(row.N)) {
			t.Fatalf("incorrect scale makespan: %+v", row)
		}
	}
}

func TestSizingHeapRetainsConflictAndEpsilonFallbacks(t *testing.T) {
	if _, ok := scheduleWithoutConflicts(2, []float64{1, 1}, [][]int{{}, {}}, [][]int{{}, {}}, [][]int{{0}, {0}}, []float64{1, 1}, 2); ok {
		t.Fatal("output conflicts bypassed original schedule")
	}
	if _, ok := scheduleWithoutConflicts(2, []float64{1, 1}, [][]int{{}, {}}, [][]int{{}, {}}, [][]int{{}, {}}, []float64{1, 1 + 5e-13}, 2); ok {
		t.Fatal("epsilon ordering changed")
	}
	// With one worker and unequal priorities, the later high-priority root runs
	// before its dependent, while ties remain in original plan order.
	if got := szSchedule(3, []float64{2, 3, 5}, [][]int{{}, {}, {1}}, [][]int{{}, {2}, {}}, [][]int{{}, {}, {}}, []float64{2, 8, 5}, 2); got != 8 {
		t.Fatalf("dependency priority schedule: %v", got)
	}
}
