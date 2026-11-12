package managed

import (
	"context"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestManagedTrackingCostIsIndependentOfCompletedOutcome(t *testing.T) {
	for _, cost := range []string{"", `,"total_cost_usd":null`, `,"total_cost_usd":-1`, `,"total_cost_usd":"unavailable"`} {
		t.Run(cost, func(t *testing.T) {
			b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"}`+cost+`}'`)
			req.TrackCost = true
			response, err := managedWork(context.Background(), dir, prefix, req, b)
			if err != nil || response.Output["answer"] != "ok" || !response.CostUnknown || response.CostUSD != 0 || response.Outcome != model.OutcomeSettled {
				t.Fatalf("cost telemetry rejected output: %+v %v", response, err)
			}
		})
	}
}

func TestManagedTrackingStillRejectsUncertainOutcome(t *testing.T) {
	for _, script := range []string{
		`printf '%s' '{"type":"result","structured_output":{"answer":"ok"}}'; exit 1`,
		`printf '%s' '{"type":"result","total_cost_usd":0,"total_cost_usd":1,"structured_output":{"answer":"ok"}}'`,
		`printf '%s' '{"type":"result","IS_ERROR":true,"structured_output":{"answer":"ok"}}'`,
	} {
		b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, script)
		req.TrackCost = true
		_, err := managedWork(context.Background(), dir, prefix, req, b)
		if !model.IsUncertain(err) {
			t.Fatalf("accepted ambiguous outcome: %v", err)
		}
	}
}

func TestManagedTrackingCompleteErrorWithoutCostSettles(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","is_error":true,"result":"Not logged in · Please run /login"}'; exit 1`)
	req.TrackCost = true
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if !ManagedSettledError(err) || !response.CostUnknown || response.DiagnosticCode != model.DiagProviderAuth {
		t.Fatalf("complete failure treated as unknown execution: %+v %v", response, err)
	}
}
