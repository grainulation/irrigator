package managed

import (
	"errors"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

// SnapshotAdmissionCheck is a scheduling hint over one owned, immutable state
// snapshot. It shares repeated authority/binding/budget work within that pass.
// Discard it before changing the snapshot. Actual BeginAttempt/ManagedReserve
// still runs ManagedCanBegin freshly under the checkpoint transaction lock.
func SnapshotAdmissionCheck(s *model.State) func(*model.TaskState) error {
	authorityErr := checkpoint.ValidateBillingAuthority(s)
	managed, tracking := checkpoint.ManagedRun(s), checkpoint.ManagedTracksCost(s)
	var used int64
	var budgetErr error
	if managed && s.Execution != nil && !tracking {
		used, budgetErr = checkpoint.ManagedBudgetUsed(s.Execution)
	}
	type key struct{ profile, digest string }
	type result struct {
		binding model.ExecutionBinding
		err     error
	}
	bindings := map[key]result{}
	return func(row *model.TaskState) error {
		if authorityErr != nil {
			return authorityErr
		}
		if !managed {
			return nil
		}
		if row == nil {
			_, err := checkpoint.ManagedBinding(s, row)
			return err
		}
		identity := key{model.ManagedProfile(s.Plan, row.Spec), row.BindingDigest}
		resolved, ok := bindings[identity]
		if !ok {
			resolved.binding, resolved.err = checkpoint.ManagedBinding(s, row)
			bindings[identity] = resolved
		}
		if resolved.err != nil {
			return resolved.err
		}
		b := resolved.binding
		if !tracking && s.Execution.Frozen {
			return errors.New("managed admission frozen after cost overrun")
		}
		if prior, ok := s.Execution.Reservations[row.OperationID]; ok {
			if prior.ReservedMicros == 0 && prior.ReportedMicros == 0 && b.Adapter.Kind == "mock" && prior.Status != "settled" {
				return nil
			}
			return errors.New("managed operation already reserved; reconcile before retry")
		}
		if tracking {
			return nil
		}
		if budgetErr != nil {
			return budgetErr
		}
		if used > s.Execution.CeilingMicros || b.OperationBudgetMicros > s.Execution.CeilingMicros-used {
			return errors.New("managed run budget exhausted by spent or held reservations")
		}
		return nil
	}
}
