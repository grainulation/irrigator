package dashboard

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestPlainDisplayBoundsTenThousandTaskOutput(t *testing.T) {
	s := &model.State{Backend: "mock", Control: "run", Tasks: map[string]*model.TaskState{}}
	for i := 0; i < model.MaxGraphTasks; i++ {
		task := model.Task{ID: fmt.Sprintf("task_%05d", i)}
		s.Plan.Tasks = append(s.Plan.Tasks, task)
		s.Tasks[task.ID] = &model.TaskState{Spec: task, Status: "pending"}
	}
	var output bytes.Buffer
	displayTo(&output, s, "runs/scale", 20)
	text := output.String()
	if strings.Count(text, "attempts=") != 20 || !strings.Contains(text, "9980 more cells") || !strings.Contains(text, "0/10000 verified") {
		t.Fatalf("unbounded or misleading display: %s", text)
	}
	if strings.Contains(text, "task_09999") || output.Len() > 4096 {
		t.Fatalf("plain output exceeded display bound: bytes=%d", output.Len())
	}
}

func TestPlainDisplayPreservesSmallRunAndUnknownCost(t *testing.T) {
	task := model.Task{ID: "a"}
	s := &model.State{Backend: "mock", Control: "complete", Plan: model.Plan{Tasks: []model.Task{task}}, Tasks: map[string]*model.TaskState{"a": {Spec: task, Status: "verified", Attempt: 1, CostBasis: "unknown", Error: "line\nnext"}}}
	var output bytes.Buffer
	displayTo(&output, s, "runs/small", 20)
	if text := output.String(); !strings.Contains(text, "1/1 verified") || !strings.Contains(text, "cost unknown") || strings.Contains(text, "more cells") {
		t.Fatal(text)
	}
}
