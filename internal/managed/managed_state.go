package managed

import (
	"errors"

	"github.com/grainulation/irrigator/internal/checkpoint"
	costpkg "github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func ManagedCanBegin(s *model.State, row *model.TaskState) error {
	if err := checkpoint.ValidateBillingAuthority(s); err != nil {
		return err
	}
	if !checkpoint.ManagedRun(s) {
		return nil
	}
	b, err := checkpoint.ManagedBinding(s, row)
	if err != nil {
		return err
	}
	if !checkpoint.ManagedTracksCost(s) && s.Execution.Frozen {
		return errors.New("managed admission frozen after cost overrun")
	}
	if prior, ok := s.Execution.Reservations[row.OperationID]; ok {
		if prior.ReservedMicros == 0 && prior.ReportedMicros == 0 && b.Adapter.Kind == "mock" && prior.Status != "settled" {
			return nil
		}
		return errors.New("managed operation already reserved; reconcile before retry")
	}
	if checkpoint.ManagedTracksCost(s) {
		return nil
	}
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil {
		return err
	}
	if used > s.Execution.CeilingMicros || b.OperationBudgetMicros > s.Execution.CeilingMicros-used {
		return errors.New("managed run budget exhausted by spent or held reservations")
	}
	return nil
}
func ManagedReserve(s *model.State, row *model.TaskState) error {
	if !checkpoint.ManagedRun(s) {
		return nil
	}
	if err := ManagedCanBegin(s, row); err != nil {
		return launch.ErrNoWork
	}
	b, _ := checkpoint.ManagedBinding(s, row)
	s.Execution.Reservations[row.OperationID] = model.ManagedReservation{TaskID: row.Spec.ID, OperationID: row.OperationID, Attempt: row.Attempt + 1, BindingDigest: b.Digest, ReservedMicros: checkpoint.ManagedReservedAmount(s, b), Status: "held"}
	return nil
}
func managedCostMicros(cost float64) (int64, error) {
	micros, ok := costpkg.MicrosFromFloat(cost)
	if !ok {
		return 0, errors.New("reported managed cost exceeds numeric bound")
	}
	// The cost entered as an exact micro-dollar amount (half-even at ingest).
	return micros, nil
}
func ManagedSettle(s *model.State, row *model.TaskState, o CellOutcome) error {
	if !checkpoint.ManagedRun(s) {
		return nil
	}
	r, ok := s.Execution.Reservations[row.OperationID]
	if !ok || r.Attempt != o.Fence.Attempt || r.BindingDigest != row.BindingDigest || r.Status == "settled" {
		return errors.New("stale or missing managed settlement")
	}
	amount, err := managedCostMicros(o.Cost)
	if err != nil {
		return err
	}
	r.ReportedMicros = amount
	if row.Status == "uncertain" || o.CleanupUnresolved || o.CleanupError != nil {
		r.Status = "uncertain"
	} else if o.CostUnknown {
		r.Status = "unknown_cost"
	} else {
		r.Status = "settled"
	}
	s.Execution.Reservations[row.OperationID] = r
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil {
		return err
	}
	if !checkpoint.ManagedTracksCost(s) && (amount > r.ReservedMicros || used > s.Execution.CeilingMicros) {
		s.Execution.Frozen = true
		s.Execution.FreezeReason = "reported cost exceeded managed admission reservation"
	}
	return nil
}
func ManagedHoldUncertain(s *model.State, row *model.TaskState) error {
	if !checkpoint.ManagedRun(s) || s.Execution == nil {
		return nil
	}
	r, ok := s.Execution.Reservations[row.OperationID]
	if !ok {
		return errors.New("uncertain cell has no managed reservation")
	}
	// Later validation/cleanup uncertainty can follow a settled model result.
	// Restore admission exposure, retaining the reported cost as evidence.
	r.Status = "uncertain"
	s.Execution.Reservations[row.OperationID] = r
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil {
		return err
	}
	if !checkpoint.ManagedTracksCost(s) && used > s.Execution.CeilingMicros {
		s.Execution.Frozen = true
		s.Execution.FreezeReason = "restored uncertain exposure exceeds managed ceiling"
	}
	return nil
}

func ManagedSettledError(err error) bool {
	var outcome *model.OutcomeError
	return errors.As(err, &outcome) && outcome.Outcome == model.OutcomeSettled
}
