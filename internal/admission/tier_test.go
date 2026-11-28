package admission

import (
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func tierEval(clk *fakeClock, s *fakeSensor, tiers func(*model.State, string) (tier, bool)) *Evaluator {
	return newEvaluator(options{clock: clk, workers: 4, exitDwell: time.Second, rampThrottle: time.Second, sync: true, locality: byName, tier: tiers},
		registration{sensor: s, policy: failOpen, ttl: time.Hour})
}

func TestTierFiltersOnlyTasksThatDeclareOne(t *testing.T) {
	declared := map[string]tier{"big": tierFull, "mid": tierReduced, "small": tierMinimal}
	lookup := func(_ *model.State, id string) (tier, bool) { t, ok := declared[id]; return t, ok }
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.modelTier, r.Reason = tierReduced, "thermal serious"
	s.set(r)
	e := tierEval(clk, s, lookup)
	st := tasks("big", "mid", "small", "untagged")
	got, env := e.Admit(st, []string{"big", "mid", "small", "untagged"}, map[string]bool{}, 4)
	want := []string{"mid", "small", "untagged"}
	if len(got) != len(want) {
		t.Fatalf("admitted %v under a reduced ceiling, want %v (env %+v)", got, want, env)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("admitted %v, want %v", got, want)
		}
	}
}

func TestTierWithNoLookupFiltersNothing(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.modelTier = tierMinimal
	s.set(r)
	got, _ := tierEval(clk, s, nil).Admit(tasks("a", "b"), []string{"a", "b"}, map[string]bool{}, 4)
	if len(got) != 2 {
		t.Fatalf("with no declared tiers the ceiling must not filter anything, admitted %v", got)
	}
}

func TestTierNeverRebindsATask(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.modelTier = tierMinimal
	s.set(r)
	st := tasks("big")
	st.Tasks["big"].BindingDigest, st.Tasks["big"].OperationID = "digest", "op"
	tierEval(clk, s, func(*model.State, string) (tier, bool) { return tierFull, true }).Admit(st, []string{"big"}, map[string]bool{}, 4)
	if st.Tasks["big"].BindingDigest != "digest" || st.Tasks["big"].OperationID != "op" {
		t.Fatal("tier filtering changed a task's binding")
	}
}

func TestProfileTiersReadsTheDeclaredTierOfATasksManagedProfile(t *testing.T) {
	plan := model.Plan{Execution: &model.ManagedExecutionPlan{Catalog: model.ExecutionCatalog{DefaultProfile: "heavy"}}}
	st := &model.State{Plan: plan, Tasks: map[string]*model.TaskState{
		"a": {Spec: model.Task{ID: "a"}},                    // default profile: heavy
		"b": {Spec: model.Task{ID: "b", Profile: "light"}},  // declared light
		"c": {Spec: model.Task{ID: "c", Profile: "custom"}}, // not declared
	}}
	lookup := profileTiers(map[string]tier{"heavy": tierFull, "light": tierMinimal})
	for id, want := range map[string]struct {
		tier     tier
		declared bool
	}{"a": {tierFull, true}, "b": {tierMinimal, true}, "c": {0, false}} {
		if got, ok := lookup(st, id); ok != want.declared || (ok && got != want.tier) {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", id, got, ok, want.tier, want.declared)
		}
	}
	if profileTiers(nil) != nil {
		t.Error("no declared tiers should mean no filter at all")
	}
	if _, err := parseTier("giant"); err == nil {
		t.Error("an unknown tier name was accepted")
	}
}
