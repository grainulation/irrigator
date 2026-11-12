package executor

import (
	"context"
	"errors"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
)

// checkpointCells is the first CellStore implementation. It deliberately retains
// the existing whole-run atomic transaction and lock hierarchy. There is no second
// cell state file, persisted mailbox, or new checkpoint/wire version.
type CheckpointCells struct{ Root string }

// transactionalCells consumes checkpoint transactions, not process ownership.
// Callers must still hold the cell lease before admitting or settling work.
type transactionalCells struct{ store checkpoint.CheckpointStore }

func (c CheckpointCells) BeginAttempt(id, operation string, pid int) (AdmittedCellAttempt, error) {
	return (transactionalCells{checkpoint.FileCheckpointStore{Root: c.Root}}).beginAttempt(id, operation, pid)
}
func (c CheckpointCells) RecordOutcome(o managed.CellOutcome) error {
	return (transactionalCells{checkpoint.FileCheckpointStore{Root: c.Root}}).recordOutcome(o)
}
func (c CheckpointCells) RecordValidation(v CellValidation) error {
	return (transactionalCells{checkpoint.FileCheckpointStore{Root: c.Root}}).recordValidation(v)
}

type AdmittedCellAttempt struct {
	Fence  managed.CellFence
	task   model.Task
	inputs map[string]map[string]string
	// snapshot is a private read snapshot from admission, never a live store view.
	snapshot *model.State
}

// beginAttempt requires the caller to hold this cell's process lease. A
// recovered running row is eligible only after the previous lease owner is gone
// and its provider is replay-safe. Other running rows require controller
// reconciliation; a free lease alone does not authorize repeating external work.
func (c transactionalCells) beginAttempt(id, expectedOperation string, pid int) (AdmittedCellAttempt, error) {
	var admitted AdmittedCellAttempt
	// Channel items live outside the checkpoint; read them before the transaction.
	var items []messaging.ChannelItem
	if pre, err := c.store.Read(context.Background()); err == nil {
		items = messaging.ChannelInbox(pre, id)
	}
	err := c.store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error {
		var err error
		admitted, err = beginCellAttempt(s, id, expectedOperation, pid, time.Now().UTC(), items...)
		return err
	})
	if err != nil {
		return AdmittedCellAttempt{}, err
	}
	return admitted, nil
}

// Reducers below perform no I/O and take their event time explicitly. They mutate
// only the supplied transaction snapshot; update publishes only successful results.
func beginCellAttempt(s *model.State, id, expectedOperation string, pid int, now time.Time, items ...messaging.ChannelItem) (AdmittedCellAttempt, error) {
	row := s.Tasks[id]
	if row == nil || s.Control != "run" || row.CancelRequested || (row.Status != "pending" && row.Status != "running") {
		return AdmittedCellAttempt{}, launch.ErrNoWork
	}
	// The controller normally reconciles lost workers before dispatch. Enforce
	// this boundary here too so direct/internal worker entry cannot replay a
	// paid or otherwise non-replay-safe call merely because its lease is free.
	if row.Status == "running" && !graph.ReplaySafe(s, row.Spec) {
		return AdmittedCellAttempt{}, launch.ErrNoWork
	}
	if expectedOperation != "" && row.OperationID != expectedOperation {
		return AdmittedCellAttempt{}, launch.ErrNoWork
	}
	// This reducer is shared by the controller and by direct/internal worker entry
	// that never passes through run(); refuse paid work without recorded consent
	// before anything (guardrails, reservations, attempts) is mutated.
	if err := AdmitUnbudgetedExecution(s); err != nil {
		return AdmittedCellAttempt{}, err
	}
	if err := launch.AdmitRuntimeGuardrails(s, id); err != nil {
		return AdmittedCellAttempt{}, err
	}
	inputs, err := managed.InputsForTask(s, row.Spec)
	if err != nil {
		return AdmittedCellAttempt{}, err
	}
	if err := managed.ManagedReserve(s, row); err != nil {
		return AdmittedCellAttempt{}, err
	}
	// A new attempt freezes its notes; a recovered running attempt keeps them.
	if row.Status == "pending" {
		row.Notes = messaging.CollectNotes(s, id, items)
		if len(row.Notes) > 0 {
			digests := map[string]any{}
			for k, v := range row.Notes {
				digests[k] = messaging.NoteDigest(v)[:12]
			}
			s.Events = append(s.Events, model.Event{Time: now, Kind: "notes_delivered", Task: id, Detail: map[string]any{"attempt": row.Attempt + 1, "notes": digests}})
		}
	}
	row.Attempt++
	row.Status, row.Error, row.DiagnosticCode = "running", "", ""
	s.Events = append(s.Events, model.Event{Time: now, Kind: "started", Task: id, Detail: map[string]any{"attempt": row.Attempt, "pid": pid, "operation_id": row.OperationID}})
	return AdmittedCellAttempt{graph.FenceForCell(s, row), row.Spec, inputs, s}, nil
}

func (c transactionalCells) recordOutcome(outcome managed.CellOutcome) error {
	return c.store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error {
		if err := recordCellOutcome(s, outcome, time.Now().UTC()); err != nil {
			return err
		}
		if err := managed.ManagedSettle(s, s.Tasks[outcome.Fence.TaskID], outcome); err != nil {
			return err
		}
		return checkpoint.ValidateManagedState(s)
	})
}

func cleanupUncertainMessage(err error) string {
	message := "provider process cleanup is unresolved; reconcile before retry"
	if err != nil {
		message += ": " + err.Error()
	}
	return message
}

func recordCellOutcome(s *model.State, o managed.CellOutcome, now time.Time) error {
	row, err := o.Fence.Current(s)
	if err != nil {
		return err
	}
	// A response is applied once while this attempt is running. An accidental
	// duplicate cannot charge twice or move an accepted/cancelled cell backwards.
	if row.Status != "running" {
		return launch.ErrNoWork
	}
	if _, ok := cost.MicrosFromFloat(o.Cost); !ok {
		return errors.New("invalid reported cost")
	}
	if o.CostUnknown && ((checkpoint.ManagedRun(s) && !launch.SupportsUnknownCost(s.Plan.APIVersion)) || o.Cost != 0 || model.KnownNotStarted(o.Err)) {
		return errors.New("invalid unknown cost outcome")
	}
	basis := o.CostBasis
	if basis == "" {
		if o.CostUnknown {
			basis = model.CostBasisUnknown
		} else {
			basis = model.CostBasisReported
		}
	}
	row.CostBasis = basis
	if o.Usage != nil {
		row.Usage = o.Usage
	}
	if o.Model != "" {
		row.Model = o.Model
	}
	row.CostUnknown = o.CostUnknown && checkpoint.ManagedRun(s)
	if err := row.AddCost(o.Cost); err != nil {
		return err
	}
	id := o.Fence.TaskID
	emit := func(kind string, detail any) {
		s.Events = append(s.Events, model.Event{Time: now, Kind: kind, Task: id, Detail: detail})
	}
	if o.CostUnknown {
		if checkpoint.ManagedRun(s) {
			policy := "retain_unknown"
			if checkpoint.PlanTracksCost(s.Plan) {
				policy = "track"
			}
			emit("cost_unknown", map[string]any{"operation_id": row.OperationID, "reported_cost_usd": nil, "policy": policy, "cost_basis": model.CostBasisUnknown})
		} else {
			provider.RecordLegacyCostUnknown(s, row, now)
		}
	}
	// An uncertain external attempt may have incurred additional charges, even
	// when a partial cost report survived. Keep that invoice uncertainty beside
	// the known subtotal for custom providers as well as the built-in CLI.
	if !checkpoint.ManagedRun(s) && checkpoint.ProviderName(s, row.Spec) != "mock" && !model.KnownNotStarted(o.Err) &&
		(o.CleanupUnresolved || o.CleanupError != nil || model.IsUncertain(o.Err) ||
			(o.Err != nil && !managed.ManagedSettledError(o.Err) && !graph.ReplaySafe(s, row.Spec))) {
		provider.RecordLegacyCostUnknown(s, row, now)
	}
	if o.CleanupUnresolved || o.CleanupError != nil {
		row.Status, row.Error = "uncertain", cleanupUncertainMessage(o.CleanupError)
		emit("outcome_uncertain", row.Error)
		return nil
	}
	if o.Err != nil {
		if model.IsUncertain(o.Err) {
			row.Status, row.Error, row.DiagnosticCode = "uncertain", model.UncertainOutcomeMessage(o.Err), o.Diagnostic
			emit("outcome_uncertain", row.Error)
			return nil
		}
		if checkpoint.ManagedRun(s) && model.KnownNotStarted(o.Err) && o.Cost > 0 {
			row.Status, row.Error = "uncertain", "not-started outcome contradicts positive reported cost"
			emit("outcome_uncertain", row.Error)
			return nil
		}
		if model.KnownNotStarted(o.Err) {
			row.Status, row.Error, row.DiagnosticCode = "failed", o.Err.Error(), o.Diagnostic
			emit("failed", row.Error)
			return nil
		}
		row.Status, row.Error = "failed", o.Err.Error()
		kind := "failed"
		if errors.Is(o.Err, context.Canceled) || errors.Is(o.Err, errStopped) {
			row.Status, row.Error, kind = "pending", "swarm stopped", "interrupted"
		}
		if row.CancelRequested {
			row.Status, kind = "cancelled", "cancelled"
		}
		if !graph.ReplaySafe(s, row.Spec) && !managed.ManagedSettledError(o.Err) {
			row.Status, kind = "uncertain", "outcome_uncertain"
		}
		if row.Status == "failed" || row.Status == "uncertain" {
			row.DiagnosticCode = o.Diagnostic
		}
		emit(kind, row.Error)
	} else {
		// Persist candidate before acceptance so recovery never needs a second model
		// call merely because validation was interrupted.
		row.Status, row.Result, row.Error, row.DiagnosticCode = "produced", o.Result, "", ""
		producedDetail := map[string]any{
			"attempt": o.Fence.Attempt,
			"reported_cost_usd": func() any {
				if o.CostUnknown {
					return nil
				}
				return o.Cost
			}(),
			"cost_unknown": o.CostUnknown,
			"cost_basis":   basis,
		}
		if row.Usage != nil {
			producedDetail["usage"] = row.Usage
		}
		if row.Model != "" {
			producedDetail["model"] = row.Model
		}
		emit("produced", producedDetail)
	}
	return nil
}

type CellValidation struct {
	Fence             managed.CellFence
	Level             string
	Err               error
	CleanupUnresolved bool
	cleanupError      error
}

func (c transactionalCells) recordValidation(v CellValidation) error {
	return c.store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error {
		if err := recordCellValidation(s, v, time.Now().UTC()); err != nil {
			return err
		}
		if row := s.Tasks[v.Fence.TaskID]; checkpoint.ManagedRun(s) && row.Status == "uncertain" {
			if err := managed.ManagedHoldUncertain(s, row); err != nil {
				return err
			}
		}
		return checkpoint.ValidateManagedState(s)
	})
}

func recordCellValidation(s *model.State, v CellValidation, now time.Time) error {
	row, err := v.Fence.Current(s)
	if err != nil {
		return err
	}
	if row.Status != "produced" && !(row.Status == "cancelled" && row.CancelRequested) {
		return launch.ErrNoWork
	}
	id := v.Fence.TaskID
	emit := func(kind string, detail any) {
		s.Events = append(s.Events, model.Event{Time: now, Kind: kind, Task: id, Detail: detail})
	}
	if v.CleanupUnresolved || v.cleanupError != nil {
		row.Status, row.Error = "uncertain", cleanupUncertainMessage(v.cleanupError)
		emit("outcome_uncertain", row.Error)
		return nil
	}
	if model.IsUncertain(v.Err) {
		row.Status, row.Error = "uncertain", "validator outcome uncertain; reconcile external outcome before explicit replay"
		emit("outcome_uncertain", row.Error)
		return nil
	}
	if row.CancelRequested {
		row.Status = "cancelled"
		emit("cancelled", nil)
		return nil
	}
	if errors.Is(v.Err, context.Canceled) {
		emit("validation_interrupted", nil)
		return nil
	}
	row.Status, row.Error, row.Validation = "verified", "", v.Level
	if v.Err != nil {
		row.Status, row.Error = "failed", v.Err.Error()
	}
	emit(row.Status, map[string]string{"validation": v.Level, "error": row.Error})
	return nil
}
