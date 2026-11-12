package executor

import (
	"errors"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

var ErrLegacyUnbudgeted = errors.New("legacy unbudgeted run requires explicit --legacy-unbudgeted=allow or --call-budget to proceed without budget enforcement")

// unbudgetedGateApplies reports whether s is a legacy unbudgeted run that has no
// durable consent. The storage schema version is deliberately ignored: every
// checkpoint update stamps it, including pause/down on an old paid run, so it
// proves nothing about consent. A run is exempt when it is managed (profile
// operation budgets), truly free (mock only), carries a durable explicit
// decision, or has a persisted positive call budget.
func UnbudgetedGateApplies(s *model.State) bool {
	if checkpoint.ManagedRun(s) {
		return false
	}
	switch s.UnbudgetedDecision {
	case checkpoint.UnbudgetedAllow, checkpoint.UnbudgetedInitial:
		return false
	}
	if s.CallLimits != nil && model.ValidCallBudget(s.CallLimits.Budget) {
		return false
	}
	return checkpoint.PotentiallyPaid(s.Backend, checkpoint.HistoricalCellTasks(s))
}

// admitUnbudgetedExecution is the execution admission guard. Call it inside a
// checkpoint Update callback (state lock held) before any state that could lead
// to paid work is published, so the check and the update cannot race.
func AdmitUnbudgetedExecution(s *model.State) error {
	if UnbudgetedGateApplies(s) {
		return ErrLegacyUnbudgeted
	}
	return nil
}
