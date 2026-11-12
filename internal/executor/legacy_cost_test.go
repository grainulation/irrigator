package executor

import (
	"errors"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestLegacyExternalUnknownInvoiceSurvivesAllUncertainReasons(t *testing.T) {
	for _, reason := range []string{"missing_cost", "malformed_response", "invalid_outcome", "deadline", "cancelled", "provider_reported"} {
		t.Run(reason, func(t *testing.T) {
			s := runfixture.Read(t, runfixture.New(t, runfixture.Task("a", 0)))
			s.Backend = "external"
			s.Plan.Providers = map[string]model.CommandSpec{"external": {Replay: "safe"}}
			row := s.Tasks["a"]
			row.Status, row.Attempt = "running", 1
			outcome := managed.CellOutcome{Fence: graph.FenceForCell(s, row), Cost: .07, Err: &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: reason, Err: errors.New("external outcome unconfirmed")}}
			if err := recordCellOutcome(s, outcome, time.Now()); err != nil {
				t.Fatal(err)
			}
			if row.Status != "uncertain" || row.Cost != .07 || provider.LegacyUnknownOperations(s) != 1 {
				t.Fatalf("lost uncertain invoice: %+v events=%+v", row, s.Events)
			}
		})
	}
}

func TestHaikuBillingEvidenceRejectsAmbiguousCost(t *testing.T) {
	for _, raw := range []string{
		`{"total_cost_usd":0.3,"total_cost_usd":0}`,
		`{"total_cost_usd":0.3,"TOTAL_COST_USD":0}`,
		`{"TOTAL_COST_USD":0}`,
		`{"total_cost_usd":null}`,
		`{"total_cost_usd":-1}`,
		`{"total_cost_usd":0} {}`,
	} {
		if _, ok := provider.HaikuReportedCost([]byte(raw)); ok {
			t.Fatalf("accepted ambiguous invoice %s", raw)
		}
	}
	for _, raw := range []string{`{"total_cost_usd":0}`, `{"total_cost_usd":0.03}`} {
		if _, ok := provider.HaikuReportedCost([]byte(raw)); !ok {
			t.Fatalf("rejected explicit cost %s", raw)
		}
	}
}
