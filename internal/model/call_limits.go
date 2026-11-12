package model

import (
	"errors"
	"time"

	"github.com/grainulation/irrigator/internal/cost"
)

// SavedCallLimits are controller defaults, not immutable plan provenance.
// Older checkpoints omit them and retain the CLI defaults.
type SavedCallLimits struct {
	Timeout time.Duration `json:"timeout_ns"`
	Budget  float64       `json:"call_budget_usd"`
}

func ValidateCallLimits(v *SavedCallLimits) error {
	if v != nil && (v.Timeout <= 0 || !ValidCallBudget(v.Budget)) {
		return errors.New("invalid saved call limits")
	}
	return nil
}
func SaveCallLimits(s *State, opts Options) {
	s.CallLimits = &SavedCallLimits{Timeout: opts.Timeout, Budget: opts.Budget}
}
func SavedCallOptions(s *State, opts Options, timeoutSet, budgetSet bool) Options {
	if s.CallLimits != nil {
		if !timeoutSet {
			opts.Timeout = s.CallLimits.Timeout
		}
		if !budgetSet {
			opts.Budget = s.CallLimits.Budget
		}
	}
	return opts
}

// validCallBudget accepts a positive per-call budget that is exactly
// representable in micro-dollars.
func ValidCallBudget(b float64) bool {
	m, ok := cost.MicrosFromFloat(b)
	return ok && m > 0
}
