package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestCostProjectionHistoricalUnknownExposure(t *testing.T) {
	s := &model.State{Plan: model.Plan{APIVersion: launch.UnknownCostPlanVersion}, Execution: &model.ManagedExecutionState{Reservations: map[string]model.ManagedReservation{
		"old":       {Status: "unknown_cost", ReservedMicros: 250000},
		"uncertain": {Status: "uncertain", ReservedMicros: 250000, ReportedMicros: 10000},
		"new":       {Status: "settled", ReservedMicros: 250000, ReportedMicros: 20000},
		"active":    {Status: "held", ReservedMicros: 250000},
	}}}
	c := provider.ProjectCost(s)
	if c.UnknownOperations != 2 || c.UnknownHeldUSD != .5 || c.KnownReportedUSD != .03 || c.InFlightReservedUSD != .25 {
		t.Fatalf("wrong accounting %+v", c)
	}
	text := provider.CostStatusText(s)
	if !strings.Contains(text, "reported estimate USD 0.0300") || !strings.Contains(text, "USD unknown: 2 operations, USD 0.5000 held exposure") {
		t.Fatal(text)
	}
}
func TestCostProjectionVerifiedUnknownPanel(t *testing.T) {
	root := runfixture.UnknownCostFixture(t)
	store := executor.CheckpointCells{Root: root}
	a, e := store.BeginAttempt("a", "", 123)
	if e != nil {
		t.Fatal(e)
	}
	if e = store.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); e != nil {
		t.Fatal(e)
	}
	if e = store.RecordValidation(executor.CellValidation{Fence: a.Fence, Level: "validated"}); e != nil {
		t.Fatal(e)
	}
	raw, e := livePanelSnapshot(root)
	if e != nil {
		t.Fatal(e)
	}
	var p panelSnapshot
	if e = json.Unmarshal(raw, &p); e != nil {
		t.Fatal(e)
	}
	if p.CostAccounting == nil || p.CostAccounting.UnknownOperations != 1 || p.CostAccounting.UnknownHeldUSD != .25 || p.CostAccounting.Basis != "provider_reported_subtotal" {
		t.Fatalf("missing cost uncertainty: %s", raw)
	}
}

func TestCostProjectionTrackNoHoldAndWarning(t *testing.T) {
	s := &model.State{Plan: model.Plan{APIVersion: launch.UnknownCostPlanVersion, Execution: &model.ManagedExecutionPlan{CostPolicy: "track", RunBudgetUSD: "0.01"}}, Execution: &model.ManagedExecutionState{Reservations: map[string]model.ManagedReservation{
		"missing": {Status: "unknown_cost", ReportedMicros: 20000},
		"active":  {Status: "held"},
	}}}
	c := provider.ProjectCost(s)
	if c.Currency != "USD" || c.Mode != "track" || c.UnknownOperations != 1 || c.UnknownHeldUSD != 0 || c.InFlightReservedUSD != 0 || !c.WarningThresholdExceeded {
		t.Fatalf("wrong tracking telemetry: %+v", c)
	}
	text := provider.CostStatusText(s)
	if strings.Contains(text, "reconcile") || strings.Contains(text, "held exposure") || !strings.Contains(text, "execution continues") {
		t.Fatal(text)
	}
}
