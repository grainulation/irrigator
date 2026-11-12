package executor

import (
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// New runs created with explicit call options, or by current code without them,
// never need the legacy gate; truly free runs carry no new field at all.
func TestUnbudgetedResumeInitialRunsRemainCompatible(t *testing.T) {
	plan := model.Plan{Name: "p", Tasks: []model.Task{runfixture.Task("a", 0)}}

	noOptions := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(plan, noOptions, "haiku"); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, noOptions)
	if s.UnbudgetedDecision != checkpoint.UnbudgetedInitial || UnbudgetedGateApplies(s) || AdmitUnbudgetedExecution(s) != nil {
		t.Fatalf("new run without options: decision %q, gated %t", s.UnbudgetedDecision, UnbudgetedGateApplies(s))
	}

	configured := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(plan, configured, "haiku", runfixture.UnbudgetedTestOptions()); err != nil {
		t.Fatal(err)
	}
	s = runfixture.Read(t, configured)
	if s.CallLimits == nil || s.UnbudgetedDecision != "" || UnbudgetedGateApplies(s) {
		t.Fatalf("explicitly configured run: %+v gated=%t", s.CallLimits, UnbudgetedGateApplies(s))
	}

	free := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(plan, free, "mock"); err != nil {
		t.Fatal(err)
	}
	s = runfixture.Read(t, free)
	if s.UnbudgetedDecision != "" || UnbudgetedGateApplies(s) {
		t.Fatalf("free mock run: decision %q gated=%t", s.UnbudgetedDecision, UnbudgetedGateApplies(s))
	}
}
