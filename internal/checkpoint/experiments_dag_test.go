package checkpoint

import (
	"fmt"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

// Validation must not recompute the same suffix per parallel edge (2^32 paths
// for the permitted 64 edges), even though this plan is rejected for hop count.
func TestMessagingValidationBoundsParallelDAG(t *testing.T) {
	p := model.Plan{Name: "parallel", Experiments: &model.PlanExperiments{Messaging: &model.MessagingExperiment{Enabled: true}}}
	for i := 0; i < 33; i++ {
		id := fmt.Sprintf("task-%d", i)
		p.Tasks = append(p.Tasks, model.Task{ID: id, Prompt: "work", Requires: []string{"result"}})
		if i > 0 {
			for j := 0; j < 2; j++ {
				p.Experiments.Messaging.Edges = append(p.Experiments.Messaging.Edges, model.MessageEdge{From: fmt.Sprintf("task-%d", i-1), To: id, Field: "result"})
			}
		}
	}
	if err := validateExperiments(p); err == nil {
		t.Fatal("accepted chain beyond hop limit")
	}
	// Memoized depths must still detect a cycle rather than hiding it.
	p.Experiments.Messaging.Edges = []model.MessageEdge{{From: "task-0", To: "task-1", Field: "result"}, {From: "task-1", To: "task-2", Field: "result"}, {From: "task-2", To: "task-1", Field: "result"}}
	if err := validateExperiments(p); err == nil {
		t.Fatal("accepted cycle")
	}
}
