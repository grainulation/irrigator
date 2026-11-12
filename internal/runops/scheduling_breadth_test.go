package runops

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// ownedState: roots r1,r2,r4; r3 depends on r1; r1__c1 owned by r1; r1__c1__g1 owned by r1__c1.
func ownedState(order string) *model.State {
	c1 := runfixture.Task("r1__c1", 0)
	c1.Parent = "r1"
	g1 := runfixture.Task("r1__c1__g1", 0)
	g1.Parent = "r1__c1"
	s := &model.State{Scheduling: order, Plan: model.Plan{Tasks: []model.Task{runfixture.Task("r1", 0), runfixture.Task("r2", 0), runfixture.Task("r3", 0, "r1"), runfixture.Task("r4", 0)}}, Dynamic: []model.Task{c1, g1}, Tasks: map[string]*model.TaskState{}}
	for _, spec := range s.AllTasks() {
		s.Tasks[spec.ID] = &model.TaskState{Spec: spec, Status: "pending"}
	}
	s.Tasks["r1"].Status = "waiting"
	return s
}

func TestBreadthStartsRootsBeforeDescendants(t *testing.T) {
	ready := []string{"r1__c1__g1", "r1__c1", "r2", "r4"}
	orderReady(ownedState(ScheduleBreadth), ready)
	if want := []string{"r2", "r4", "r1__c1", "r1__c1__g1"}; !reflect.DeepEqual(ready, want) {
		t.Fatalf("breadth order %v, want %v", ready, want)
	}
}

func TestDefaultOrderUnchangedByBreadthPolicy(t *testing.T) {
	for _, order := range []string{"", scheduleDepth} {
		got := []string{"r1__c1__g1", "r1__c1", "r2", "r4"}
		orderReady(ownedState(order), got)
		want := []string{"r1__c1__g1", "r1__c1", "r2", "r4"}
		prioritizeReady(ownedState(""), want)
		if !reflect.DeepEqual(got, want) || got[0] != "r1__c1__g1" {
			t.Fatalf("default order changed for %q: %v want %v", order, got, want)
		}
	}
}

func TestBreadthOnlyPermutesCallerReadySet(t *testing.T) {
	// r3 depends on unverified r1, so the controller never lists it; ordering must not add it.
	ready := []string{"r1__c1__g1", "r2", "r4"}
	before := append([]string{}, ready...)
	orderReady(ownedState(ScheduleBreadth), ready)
	sort.Strings(ready)
	sort.Strings(before)
	if !reflect.DeepEqual(ready, before) {
		t.Fatalf("membership changed: %v vs %v", ready, before)
	}
}

func TestSchedulingOrderRequestIsExplicitAndJournaled(t *testing.T) {
	s := ownedState("")
	applySchedulingOrder(s, "")
	applySchedulingOrder(s, scheduleDepth)
	if s.Scheduling != "" || len(s.Events) != 0 {
		t.Fatalf("omitted/depth request changed default: %q %v", s.Scheduling, s.Events)
	}
	applySchedulingOrder(s, ScheduleBreadth)
	applySchedulingOrder(s, ScheduleBreadth)
	if s.Scheduling != ScheduleBreadth || len(s.Events) != 1 || s.Events[0].Kind != "scheduling_order" {
		t.Fatalf("breadth request: %q %v", s.Scheduling, s.Events)
	}
	applySchedulingOrder(s, "")
	if s.Scheduling != ScheduleBreadth {
		t.Fatal("empty request must keep the persisted order")
	}
	applySchedulingOrder(s, scheduleDepth)
	if s.Scheduling != "" || len(s.Events) != 2 {
		t.Fatalf("depth request: %q %v", s.Scheduling, s.Events)
	}
	data, _ := json.Marshal(s)
	if strings.Contains(string(data), "\"scheduling\"") {
		t.Fatal("default state must omit the scheduling field")
	}
}
