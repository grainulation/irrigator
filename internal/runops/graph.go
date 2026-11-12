package runops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

func CancelNode(root, id string) error {
	return checkpoint.Update(root, func(s *model.State) error {
		if s.Tasks[id] == nil {
			return errors.New("unknown node")
		}
		for _, node := range managed.Descendants(s, id) {
			r := s.Tasks[node]
			if r.Status == "verified" {
				continue
			}
			r.CancelRequested = true
			if r.Status != "running" && r.Status != "uncertain" {
				r.Status = "cancelled"
			}
			s.Event("cancel_requested", node, nil)
		}
		return nil
	})
}

type retryReport struct {
	Retried []string
	Waiting []string
	Skipped []string
}

func RetryNodeWithReport(root, id string, recursive, allowUncertain bool, confirmStopped ...bool) (retryReport, error) {
	var report retryReport
	err := retryNodeCollect(root, id, recursive, allowUncertain, &report, confirmStopped...)
	if err != nil {
		return retryReport{}, err
	}
	return report, nil
}

func retryNodeCollect(root, id string, recursive, allowUncertain bool, report *retryReport, confirmStopped ...bool) error {
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return err
	}
	lock, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		return errors.New("stop the controller before retrying a node")
	}
	defer checkpoint.Unlock(lock)
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	if s.Tasks[id] == nil {
		return errors.New("unknown node")
	}
	nodes := []string{id}
	if recursive {
		nodes = managed.Descendants(s, id)
	}
	// Keep every selected lease until the state transition is committed.
	var leases []*os.File
	defer func() {
		for _, f := range leases {
			checkpoint.Unlock(f)
		}
	}()
	for _, node := range nodes {
		f, e := checkpoint.FileLock(filepath.Join(root, "cells", node, "lease"), true)
		if e != nil {
			return errors.New("node still has an active worker")
		}
		leases = append(leases, f)
	}
	// A dead worker can leave its provider alive. A free lease alone is not
	// cleanup evidence; retain the leases while inspecting and acknowledging
	// process markers, just as resolution does. Never probe or kill saved PIDs.
	s, err = checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	var cleanup []string
	missingCleanup := map[string]bool{}
	for _, node := range nodes {
		r := s.Tasks[node]
		if r.Status == "verified" {
			continue
		}
		if checkpoint.HasTurnSemantics(s.Plan.APIVersion) && r.ExpansionHash != "" {
			return fmt.Errorf("turn %s already committed its result and descendant admissions; retry the failed descendants instead, or inspect/recover inconsistent parent state before changing the accepted turn", node)
		}
		if !allowUncertain && (r.Status == "uncertain" || (r.Status == "running" && r.Attempt > 0 && !graph.ReplaySafe(s, r.Spec))) {
			return errors.New("outcome uncertain: reconcile with --result, or explicitly retry with --allow-uncertain")
		}
		cell := filepath.Join(root, "cells", node)
		unresolved, markerErr := launch.UnresolvedProcess(cell, r.Attempt)
		mock := checkpoint.ProviderName(s, r.Spec) == "mock"
		if checkpoint.ManagedRun(s) {
			binding, err := checkpoint.ManagedBinding(s, r)
			mock = err == nil && binding.Adapter.Kind == "mock"
		}
		// Older runs may have no process marker. Absence is not evidence that
		// an external call stopped. Include lost-running rows so callers cannot
		// bypass this gate by retrying before reconciliation marks uncertainty.
		if !mock && r.Attempt > 0 && (r.Status == "uncertain" || r.Status == "running") {
			_, statErr := os.Stat(filepath.Join(cell, fmt.Sprintf("%d.process.json", r.Attempt)))
			if os.IsNotExist(statErr) {
				missingCleanup[node] = true
				unresolved = true
			} else if statErr != nil {
				markerErr = statErr
			}
		}
		if unresolved || markerErr != nil {
			if len(confirmStopped) == 0 || !confirmStopped[0] {
				return fmt.Errorf("node %s process cleanup is unconfirmed: verify the provider has stopped, then retry with --confirm-stopped (and --allow-uncertain for an uncertain outcome)", node)
			}
			cleanup = append(cleanup, node)
		}
	}
	for _, node := range cleanup {
		if err := launch.AcknowledgeProcessCleanup(filepath.Join(root, "cells", node), s.Tasks[node].Attempt); err != nil {
			return err
		}
	}
	return checkpoint.Update(root, func(s *model.State) error {
		for _, node := range nodes {
			r := s.Tasks[node]
			if checkpoint.ManagedRun(s) {
				if hold, ok := s.Execution.Reservations[r.OperationID]; ok && hold.Status == "held" && hold.ReservedMicros > 0 {
					if !allowUncertain {
						return errors.New("managed dispatch has an unresolved hold: reconcile or explicitly retry with --allow-uncertain")
					}
					if err := managed.ManagedHoldUncertain(s, r); err != nil {
						return err
					}
				}
			}
			if !allowUncertain && (r.Status == "uncertain" || (r.Status == "running" && r.Attempt > 0 && !graph.ReplaySafe(s, r.Spec))) {
				return errors.New("outcome uncertain: reconcile with --result, or explicitly retry with --allow-uncertain")
			}
		}
		for _, node := range nodes {
			r := s.Tasks[node]
			if r.Status == "verified" {
				report.Skipped = append(report.Skipped, node)
				continue
			}
			if missingCleanup[node] {
				// Record the user's attestation, not fabricated supervisor evidence.
				s.Event("process_cleanup_confirmed", node, map[string]any{"attempt": r.Attempt, "operation_id": r.OperationID, "confirmed_by": "explicit-user", "missing_marker": true})
			}
			r.CancelRequested = false
			r.Error = ""
			if !checkpoint.HasTurnSemantics(s.Plan.APIVersion) && len(r.Children) > 0 {
				r.Status = "waiting"
				report.Waiting = append(report.Waiting, node)
			} else {
				report.Retried = append(report.Retried, node)
				// Explicit retry creates a new logical operation; crash recovery does not.
				r.CostUnknown = false
				r.Generation++
				r.OperationID = model.OperationID(s.RunID, node, r.Generation)
				r.Status = "pending"
				if checkpoint.ManagedRun(s) {
					r.BindingDigest = model.ManagedCurrentBinding(s, model.ManagedProfile(s.Plan, r.Spec)).Digest
				}
				r.Result = nil
				r.Validation = ""
				if checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
					r.ExpansionHash = ""
				}
			}
			s.Event("retry_requested", node, map[string]any{"operation_id": r.OperationID})
		}
		if checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
			return checkpoint.ValidateTurnState(s)
		}
		return nil
	})
}
func ResolveNode(root, id string, result map[string]string, confirmStopped ...bool) error {
	lock, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		return errors.New("stop controller before reconciliation")
	}
	defer checkpoint.Unlock(lock)
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	if s.Tasks[id] == nil {
		return errors.New("unknown node")
	}
	lease, err := checkpoint.FileLock(filepath.Join(root, "cells", id, "lease"), true)
	if err != nil {
		return errors.New("node has a live worker")
	}
	defer checkpoint.Unlock(lease)
	// Re-read after taking the lease: a finishing worker may have changed state.
	s, err = checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	row := s.Tasks[id]
	if row.Status != "uncertain" {
		return errors.New("resolve requires an uncertain node")
	}
	cell := filepath.Join(root, "cells", id)
	unresolved, markerErr := launch.UnresolvedProcess(cell, row.Attempt)
	if unresolved || markerErr != nil {
		if len(confirmStopped) == 0 || !confirmStopped[0] {
			return errors.New("process cleanup is unconfirmed: verify the provider has stopped, then resolve with --confirm-stopped")
		}
		if err := launch.AcknowledgeProcessCleanup(cell, row.Attempt); err != nil {
			return err
		}
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		r := s.Tasks[id]
		if r.Status != "uncertain" {
			return errors.New("resolve requires an uncertain node")
		}
		r.Result = result
		r.Status = "produced"
		r.CancelRequested = false
		s.Event("outcome_reconciled", id, map[string]string{"operation_id": r.OperationID})
		return nil
	}); err != nil {
		return err
	}
	if s.Control == "down" {
		return nil
	} // Saved outcome is validated on resume.
	return executor.Finalize(root, id, lease)
}
