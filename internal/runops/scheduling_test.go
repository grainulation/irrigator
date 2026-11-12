package runops

import (
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestReadyPrioritizesDependencyChain(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("leaf", 0), runfixture.Task("root", 0), runfixture.Task("middle", 0, "root"), runfixture.Task("end", 0, "middle"), runfixture.Task("tie", 0))
	s := runfixture.Read(t, root)
	ready := []string{"leaf", "root", "tie"}
	prioritizeReady(s, ready)
	if !reflect.DeepEqual(ready, []string{"root", "leaf", "tie"}) {
		t.Fatal(ready)
	}
}

func TestReadyRanksThroughParentCompletionBarrier(t *testing.T) {
	child := runfixture.Task("parent__child", 0)
	child.Parent = "parent"
	s := &model.State{Plan: model.Plan{Tasks: []model.Task{runfixture.Task("leaf", 0), runfixture.Task("parent", 0), runfixture.Task("consumer", 0, "parent")}}, Dynamic: []model.Task{child}, Tasks: map[string]*model.TaskState{}}
	for _, spec := range s.AllTasks() {
		s.Tasks[spec.ID] = &model.TaskState{Spec: spec, Status: "pending"}
	}
	s.Tasks["parent"].Status = "waiting"
	ready := []string{"leaf", child.ID}
	prioritizeReady(s, ready)
	if ready[0] != child.ID {
		t.Fatal(ready)
	}
}
