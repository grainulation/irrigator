//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

func TestTurnRuntimeExecutionIndependentLanes(t *testing.T) {
	// beta and the helper are held until the continuation has started, so a
	// continuation that waited for either would never start.
	next := task("ignored", 0)
	rootTask := runfixture.TurnRequestTask(t, "alpha", checkpoint.TurnRequest{Continue: &next, Helpers: []model.Task{task("h", 0)}})
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 2, MainTurns: 2, HelpersPerTurn: 1}, rootTask, task("beta", 0))
	release := runfixture.HoldMock(t, "beta", "alpha__turn1__helper_h")
	_, done := start(t, root, "--workers", "4")
	waitFor(t, root, func(s *model.State) bool {
		r := s.Tasks["alpha__turn2"]
		return r != nil && (r.Status == "running" || r.Status == "verified")
	})
	release("beta", "alpha__turn1__helper_h")
	finished(t, done, true)
	s := read(t, root)
	if len(s.Tasks) != 4 {
		t.Fatalf("expected optional four executions, got%d", len(s.Tasks))
	}
	cont := s.Tasks["alpha__turn2"]
	helper := s.Tasks["alpha__turn1__helper_h"]
	if cont == nil || helper == nil || cont.Spec.Turn.AgentID != "alpha" || helper.Spec.Turn.Role != "helper" {
		t.Fatal("lost identities")
	}
	seq := map[string]int{}
	for i, e := range s.Events {
		seq[e.Task+"/"+e.Kind] = i
	}
	if seq["alpha__turn2/started"] > seq["beta/turn_verified"] || seq["alpha__turn2/started"] > seq["alpha__turn1__helper_h/turn_verified"] {
		t.Fatal("unnecessary root/helper barrier")
	}
	if seq["alpha__turn2/started"] < seq["alpha/turn_verified"] {
		t.Fatal("continuation before accepted predecessor")
	}
	_, done = start(t, root)
	finished(t, done, true)
	for id, r := range read(t, root).Tasks {
		if r.Attempt != 1 {
			t.Fatalf("%s replayed", id)
		}
	}
}
func TestTurnRuntimeWaitForHelpers(t *testing.T) {
	next := task("next", 0)
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next, Helpers: []model.Task{task("h", .08)}, WaitForHelpers: true}))
	_, done := start(t, root, "--workers", "3")
	finished(t, done, true)
	s := read(t, root)
	seq := map[string]int{}
	for i, e := range s.Events {
		seq[e.Task+"/"+e.Kind] = i
	}
	if seq["a__turn2/started"] < seq["a__turn1__helper_h/turn_verified"] {
		t.Fatal("helper dependency not enforced")
	}
	inputs, e := managed.InputsForTask(s, s.Tasks["a__turn2"].Spec)
	if e != nil || inputs["a__turn1__helper_h"] == nil {
		t.Fatal("helper result not consumed")
	}
}
func TestTurnRuntimeRejectsTerminalExpansionAndLimits(t *testing.T) {
	cases := []struct {
		name  string
		req   checkpoint.TurnRequest
		shape model.TurnShape
	}{
		{"helpers", checkpoint.TurnRequest{Helpers: []model.Task{task("one", 0), task("two", 0)}}, model.TurnShape{Roots: 1, MainTurns: 1, HelpersPerTurn: 1}},
		{"turns", checkpoint.TurnRequest{Continue: func() *model.Task { x := task("next", 0); return &x }()}, model.TurnShape{Roots: 1, MainTurns: 1, HelpersPerTurn: 0}},
		{"identity", checkpoint.TurnRequest{Helpers: []model.Task{{ID: "bad", Prompt: "bad", Requires: []string{"a"}, Turn: &model.TurnIdentity{Role: "main"}}}}, model.TurnShape{Roots: 1, MainTurns: 1, HelpersPerTurn: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := runfixture.TurnFixture(t, c.shape, runfixture.TurnRequestTask(t, "a", c.req))
			_, done := start(t, root)
			finished(t, done, false)
			s := read(t, root)
			if len(s.Tasks) != 1 || s.Tasks["a"].Status != "failed" {
				t.Fatal("invalid proposal admitted")
			}
		})
	}
	badHelper := runfixture.TurnRequestTask(t, "h", checkpoint.TurnRequest{Helpers: []model.Task{task("child", 0)}})
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Helpers: []model.Task{badHelper}}))
	_, done := start(t, root)
	finished(t, done, false)
	s := read(t, root)
	if len(s.Tasks) != 2 || s.Tasks["a__turn1__helper_h"].Status != "failed" {
		t.Fatal("helper expanded")
	}
}
func TestTurnRuntimeAtomicAdmissionRecovery(t *testing.T) {
	next := task("next", 0)
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next, Helpers: []model.Task{task("h", 0)}}))
	runfixture.StageExpansion(t, root, "a")
	if e := os.MkdirAll(filepath.Join(root, "cells", "a__turn2"), 0700); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- commitTurnExpansion(root, "a", "structural") }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	s := read(t, root)
	if len(s.Tasks) != 3 {
		t.Fatal("duplicate admission")
	}
	events := 0
	for _, e := range s.Events {
		if e.Kind == "turn_verified" {
			events++
		}
	}
	if events != 1 {
		t.Fatal("duplicate receipt")
	}
	_, done := start(t, root)
	finished(t, done, true)
	if read(t, root).Tasks["a"].Attempt != 0 {
		t.Fatal("produced predecessor replayed")
	}
}

func TestTurnRuntimeUncertainNeverAutomaticallyReplays(t *testing.T) {
	next := task("next", 0)
	main := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next})
	main.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
	shape := model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 0}
	p := model.Plan{APIVersion: checkpoint.TurnPlanVersion, Name: "manual outcome", TurnShape: &shape, MaxTasks: 2, Tasks: []model.Task{main}, Providers: map[string]model.CommandSpec{"manual": {Command: []string{"never-invoked"}, Replay: "manual"}}}
	root := filepath.Join(t.TempDir(), "run")
	if e := checkpoint.Initialize(p, root, "manual"); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "running"; s.Tasks["a"].Attempt = 1; return nil }); e != nil {
		t.Fatal(e)
	}
	if e := runops.Reconcile(root); e != nil {
		t.Fatal(e)
	}
	s := read(t, root)
	if s.Tasks["a"].Status != "uncertain" || len(s.Tasks) != 1 {
		t.Fatal("lost worker replayed or admitted continuation")
	}
	_, done := start(t, root)
	finished(t, done, false)
	s = read(t, root)
	if s.Tasks["a"].Attempt != 1 || len(s.Tasks) != 1 {
		t.Fatal("uncertain execution replayed")
	}
}
