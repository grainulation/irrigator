package cli

import (
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestOfflineBenchmarkHeldOutRejectsIncorrectVerifiedOutput(t *testing.T) {
	s := &model.State{Control: "complete", Tasks: map[string]*model.TaskState{}}
	for _, task := range benchmarkFixture().Tasks {
		s.Tasks[task.ID] = &model.TaskState{Spec: task, Status: "verified", Result: task.MockResult}
	}
	if failures := gradeBenchmark(s); len(failures) != 0 {
		t.Fatal(failures)
	}
	s.Tasks["square_3"].Result = map[string]string{"answer": "8"}
	if failures := gradeBenchmark(s); len(failures) == 0 {
		t.Fatal("verified status bypassed held-out correctness")
	}
	s.Tasks["square_3"].Result = map[string]string{"answer": "9"}
	s.Tasks["join"].Status = "uncertain"
	if failures := gradeBenchmark(s); len(failures) == 0 {
		t.Fatal("uncertain result accepted")
	}
}
