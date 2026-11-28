package admission

import (
	"context"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func tasks(ids ...string) *model.State {
	s := &model.State{Tasks: map[string]*model.TaskState{}}
	for _, id := range ids {
		s.Tasks[id] = &model.TaskState{}
	}
	return s
}

// locality by name: ids starting with "r" are remote, everything else local.
func byName(_ *model.State, id string) locality {
	if id[0] == 'r' {
		return remote
	}
	return local
}

func filterEval(clk *fakeClock, workers int, s *fakeSensor) *Evaluator {
	return newEvaluator(options{clock: clk, workers: workers, exitDwell: time.Second, rampThrottle: time.Second, sync: true, locality: byName},
		registration{sensor: s, policy: failOpen, ttl: time.Hour})
}

func TestAdmitOpenEnvelopeAdmitsReadyTasksInOrderUpToTheSlots(t *testing.T) {
	e := filterEval(newFakeClock(), 4, newFake("s"))
	got, env := e.Admit(tasks("l1", "r1", "l2", "r2", "l3"), []string{"l1", "r1", "l2", "r2", "l3"}, map[string]bool{}, 3)
	if env.restricted() || len(got) != 3 || got[0] != "l1" || got[1] != "r1" || got[2] != "l2" {
		t.Fatalf("admitted %v under %+v", got, env)
	}
}

func TestAdmitSeparatesLocalAndRemoteSlots(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.MaxLocalSlots, r.Reason = 1, "thermal"
	s.set(r)
	e := filterEval(clk, 6, s)
	st := tasks("l1", "l2", "l3", "r1", "r2")
	got, _ := e.Admit(st, []string{"l1", "l2", "l3", "r1", "r2"}, map[string]bool{}, 6)
	// Only one local task, but remote tasks are not starved by the hot local limit.
	want := []string{"l1", "r1", "r2"}
	if len(got) != len(want) {
		t.Fatalf("admitted %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("admitted %v, want %v", got, want)
		}
	}
}

func TestAdmitCountsWorkAlreadyRunningAgainstItsLocality(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.MaxLocalSlots = 2
	s.set(r)
	e := filterEval(clk, 6, s)
	st := tasks("l1", "l2", "l3", "r1")
	got, _ := e.Admit(st, []string{"l3", "r1"}, map[string]bool{"l1": true, "l2": true}, 4)
	if len(got) != 1 || got[0] != "r1" {
		t.Fatalf("two local tasks already run against a local limit of 2, admitted %v", got)
	}
}

func TestAdmitRemoteOffStopsRemoteOnlyAndLetsLocalWorkProceed(t *testing.T) {
	clk := newFakeClock()
	s := newFake("net")
	r := OpenReading("net")
	r.allowRemote, r.severity, r.Reason = false, hard, "offline"
	s.set(r)
	e := filterEval(clk, 4, s)
	got, env := e.Admit(tasks("r1", "l1", "r2", "l2"), []string{"r1", "l1", "r2", "l2"}, map[string]bool{}, 4)
	if len(got) != 2 || got[0] != "l1" || got[1] != "l2" {
		t.Fatalf("admitted %v, want only the local tasks", got)
	}
	if env.AllowRemote {
		t.Fatal("the envelope should say remote is off")
	}
}

func TestAdmitHoldAdmitsNothingAndNeverRebindsAnything(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.hold, r.Reason = HoldTransient, "battery 9%"
	s.set(r)
	e := filterEval(clk, 4, s)
	st := tasks("l1", "l2")
	st.Tasks["l1"].BindingDigest, st.Tasks["l1"].OperationID = "digest-1", "op-1"
	got, env := e.Admit(st, []string{"l1", "l2"}, map[string]bool{}, 4)
	if len(got) != 0 || env.Hold != HoldTransient {
		t.Fatalf("a hold admitted %v", got)
	}
	if st.Tasks["l1"].BindingDigest != "digest-1" || st.Tasks["l1"].OperationID != "op-1" {
		t.Fatal("admission changed a task's binding")
	}
}

func TestAdmitRecoversByRampingAfterAHold(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	hold := OpenReading("power")
	hold.hold = HoldTransient
	s.set(hold)
	e := newEvaluator(options{clock: clk, workers: 3, exitDwell: 5 * time.Second, rampThrottle: 2 * time.Second, sync: true, locality: byName},
		registration{sensor: s, policy: failOpen, ttl: time.Second})
	st := tasks("l1", "l2", "l3")
	ready := []string{"l1", "l2", "l3"}
	if got, _ := e.Admit(st, ready, map[string]bool{}, 3); len(got) != 0 {
		t.Fatalf("admitted %v during a hold", got)
	}
	s.set(OpenReading("power"))
	var counts []int
	for i := 0; i < 12; i++ {
		clk.Advance(time.Second)
		got, _ := e.Admit(st, ready, map[string]bool{}, 3)
		counts = append(counts, len(got))
	}
	if counts[0] != 0 || counts[len(counts)-1] != 3 {
		t.Fatalf("recovery went %v, want from 0 up to 3", counts)
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[i-1] || counts[i]-counts[i-1] > 1 {
			t.Fatalf("recovery must grow by at most one slot at a time: %v", counts)
		}
	}
}

func TestClassifierLegacyBackendsAndMissingRows(t *testing.T) {
	c := newClassifier()
	mock := &model.State{Backend: "mock", Tasks: map[string]*model.TaskState{"a": {}}}
	haiku := &model.State{Backend: "haiku", Tasks: map[string]*model.TaskState{"a": {}}}
	if c.locality(mock, "a") != local {
		t.Error("a mock run is local")
	}
	if c.locality(haiku, "a") != remote {
		t.Error("the paid haiku backend is remote")
	}
	if c.locality(mock, "missing") != remote {
		t.Error("an unknown task is classified conservatively as remote")
	}
}

func TestEvaluateReturnsWithinTheCallerContext(t *testing.T) {
	e := filterEval(newFakeClock(), 2, newFake("s"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if env := e.evaluate(ctx, runSnapshot{}); env.restricted() {
		t.Fatalf("a cancelled context restricted the run: %+v", env)
	}
}
