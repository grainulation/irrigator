package provider

import (
	"fmt"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// costSummary preserves known provider reports separately from reservation
// exposure. Exposure is not an invoice or an estimate of the missing cost.
type CostSummary struct {
	Currency                 string           `json:"currency"`
	Mode                     string           `json:"mode"`
	WarningThresholdExceeded bool             `json:"warning_threshold_exceeded,omitempty"`
	ExternallyReconciledUSD  float64          `json:"externally_reconciled_usd,omitempty"`
	KnownReportedUSD         float64          `json:"known_reported_usd"`
	UnknownHeldUSD           float64          `json:"unknown_held_exposure_usd"`
	UnknownOperations        int              `json:"unknown_cost_operations"`
	InFlightReservedUSD      float64          `json:"in_flight_reserved_usd"`
	Basis                    string           `json:"basis"`
	Tokens                   *model.TaskUsage `json:"tokens,omitempty"`
}

func ProjectCost(s *model.State) CostSummary {
	c := CostSummary{KnownReportedUSD: s.ReportedCost(), Basis: "provider_reported_subtotal", Currency: "USD", Mode: "enforce"}
	var totalTokens *model.TaskUsage
	for _, row := range s.Tasks {
		if row != nil && row.Usage != nil {
			totalTokens = model.AddUsage(totalTokens, row.Usage)
		}
	}
	for _, row := range s.ArchivedTasks {
		if row.Usage != nil {
			totalTokens = model.AddUsage(totalTokens, row.Usage)
		}
	}
	c.Tokens = totalTokens

	tracking := !checkpoint.ManagedRun(s) || checkpoint.ManagedTracksCost(s)
	if tracking {
		c.Mode = "track"
	}
	if !checkpoint.ManagedRun(s) || s.Execution == nil {
		c.UnknownOperations = LegacyUnknownOperations(s)
		return c
	}
	var known, reconciled, heldUnknown, inFlight int64
	for _, r := range s.Execution.Reservations {
		known += r.ReportedMicros
		if receipt, ok := s.Execution.BillingReceipts[r.OperationID]; ok {
			reconciled += receipt.AmountMicros
			if r.Status == "unknown_cost" {
				continue
			}
		}
		exposure := r.ReservedMicros
		if r.ReportedMicros > exposure {
			exposure = r.ReportedMicros
		}
		switch r.Status {
		case "unknown_cost", "uncertain":
			if !tracking {
				heldUnknown += exposure
			}
			c.UnknownOperations++
		case "held":
			if !tracking {
				inFlight += exposure
			}
		}
	}
	c.KnownReportedUSD, c.ExternallyReconciledUSD = cost.MicrosToFloat(known), cost.MicrosToFloat(reconciled)
	c.UnknownHeldUSD, c.InFlightReservedUSD = cost.MicrosToFloat(heldUnknown), cost.MicrosToFloat(inFlight)
	if tracking && s.Plan.Execution != nil {
		threshold, err := launch.ExecutionBudgetMicros(s.Plan.Execution.RunBudgetUSD)
		c.WarningThresholdExceeded = err == nil && threshold > 0 && known > threshold
	}
	return c
}
func CostStatusText(s *model.State) string {
	c := ProjectCost(s)
	text := fmt.Sprintf("reported estimate USD %.4f", c.KnownReportedUSD)
	if c.ExternallyReconciledUSD > 0 {
		text += fmt.Sprintf(" | external billing USD %.4f", c.ExternallyReconciledUSD)
	}
	if c.Tokens != nil && c.Tokens.TotalTokens > 0 {
		text += fmt.Sprintf(" | %s", model.FormatTokenSummary(c.Tokens))
	}
	if c.UnknownOperations > 0 {
		if checkpoint.ManagedRun(s) && !checkpoint.ManagedTracksCost(s) {
			text += fmt.Sprintf(" | USD unknown: %d operations, USD %.4f held exposure", c.UnknownOperations, c.UnknownHeldUSD)
		} else {
			text += fmt.Sprintf(" | cost unavailable: %d operations (tracking only; no funds held)", c.UnknownOperations)
		}
		if checkpoint.ManagedRun(s) && !checkpoint.ManagedTracksCost(s) {
			text += " | reconcile verified charges with: irrigator billing reconcile RUN RECEIPT.json --attest-external-billing"
		}
	}
	if c.WarningThresholdExceeded {
		text += " | warning: reported USD estimate exceeds tracking threshold; execution continues"
	}
	return text
}
