package runops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/approvals"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clock"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/dashboard"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/session"
	"github.com/grainulation/irrigator/internal/sizing"
)

func Reconcile(root string) error {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	return reconcileSnapshot(root, s)
}

func reconcileSnapshot(root string, s *model.State) error {
	for _, spec := range s.AllTasks() {
		id := spec.ID
		if st := s.Tasks[id].Status; st != "running" && st != "produced" {
			continue
		}
		lease, e := checkpoint.FileLock(filepath.Join(root, "cells", id, "lease"), true)
		if errors.Is(e, syscall.EWOULDBLOCK) {
			continue
		}
		if e != nil {
			return e
		}
		// A worker may commit and exit after the initial scan. Read the state
		// again under its lease; all produced results must use full finalization.
		fresh, e := checkpoint.ReadState(root)
		if e != nil {
			checkpoint.Unlock(lease)
			return e
		}
		if fresh.Tasks[id].Status == "produced" {
			if fresh.Control == "down" {
				checkpoint.Unlock(lease)
				continue
			}
			e = executor.Finalize(root, id, lease)
			checkpoint.Unlock(lease)
			if e != nil {
				return e
			}
			continue
		}
		e = checkpoint.Update(root, func(current *model.State) error {
			row := current.Tasks[id]
			switch row.Status {
			case "running":
				row.Status = "pending"
				unresolved, processErr := launch.UnresolvedProcess(filepath.Join(root, "cells", id), row.Attempt)
				if !graph.ReplaySafe(current, row.Spec) || unresolved || processErr != nil {
					row.Status = "uncertain"
					row.Error = "worker lost; reconcile external outcome before retry"
					if unresolved || processErr != nil {
						row.Error = "provider cleanup unconfirmed; inspect processes and reconcile before explicit retry"
					}
				}
				if row.CancelRequested && row.Status != "uncertain" {
					row.Status = "cancelled"
				}
				if checkpoint.ManagedRun(current) {
					if r, ok := current.Execution.Reservations[row.OperationID]; ok && r.ReservedMicros > 0 && r.Status != "settled" {
						row.Status = "uncertain"
						row.Error = "managed dispatch interrupted; reservation retained until reconciliation"
						if err := managed.ManagedHoldUncertain(current, row); err != nil {
							return err
						}
					}
				}
				if !checkpoint.ManagedRun(current) && checkpoint.ProviderName(current, row.Spec) == "haiku" {
					// The worker may die after receiving a cost report but before its
					// checkpoint commit. Reconcile once under the cell lease.
					// Cleanup certainty and observed billing are independent. A complete
					// bounded report from this exact attempt remains evidence even
					// when the supervisor could not confirm process termination.
					// Keep the unknown-billing event and uncertain status: this is
					// a reported subtotal, never proof of final cost or safe replay.
					raw, readErr := model.ReadProcessEvidence(filepath.Join(root, "cells", id, strconv.Itoa(row.Attempt)+".stdout.json"), model.MaxOutputBytes)
					if readErr == nil {
						if cost, ok := provider.HaikuReportedCost(raw); ok {
							// An unusable report (invalid or out of range) is ignored: the
							// unknown-billing event recorded below still marks the cost unknown.
							_ = row.AddCost(cost)
						}
					}
					provider.RecordLegacyCostUnknown(current, row, clock.ActiveClock.Now().UTC())
				}
				current.Event("recovered", id, nil)
			}
			return nil
		})
		checkpoint.Unlock(lease)
		if e != nil {
			return e
		}
	}
	return graph.ReconcileParents(root)
}

func Run(root string, opts model.Options) (code int, runErr error) {
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return 1, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return 1, err
	}
	controller, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return 1, errors.New("a controller is already running")
	}
	if err != nil {
		return 1, err
	}
	defer checkpoint.Unlock(controller)
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return 1, err
	}
	sizing, serr := sizing.LoadRunSizing(root, sizing.ActiveSizingPath)
	if serr != nil {
		return 1, serr
	}
	previous, err := checkpoint.ReadState(root)
	if err != nil {
		return 1, err
	}
	if previous.Control == "down" {
		for id := range previous.Tasks {
			lease, e := checkpoint.FileLock(filepath.Join(root, "cells", id, "lease"), true)
			if e != nil {
				return 1, errors.New("down is still draining workers; resume after they stop")
			}
			checkpoint.Unlock(lease)
		}
	}
	if err = checkpoint.Update(root, func(s *model.State) error {
		if err := executor.AdmitUnbudgetedExecution(s); err != nil {
			return err
		}
		s.Control = "run"
		model.SaveCallLimits(s, opts)
		applySchedulingOrder(s, ActiveSchedulingOrder)
		if opts.Retry {
			for id, row := range s.Tasks {
				if row.Status == "failed" {
					if checkpoint.HasTurnSemantics(s.Plan.APIVersion) && row.ExpansionHash != "" {
						fmt.Fprintf(os.Stderr, "Skipped failed turn %s: its accepted result and descendant admissions are already committed. Retry failed descendants or inspect/recover the inconsistent parent state.\n", id)
						continue
					}
					if !graph.ReplaySafe(s, row.Spec) {
						fmt.Fprintf(os.Stderr, "Skipped failed node %s: provider %s is not safe for automatic replay. Inspect its outcome, then explicitly retry with: irrigator retry %q %q\n", id, checkpoint.ProviderName(s, row.Spec), root, id)
						continue
					}
					row.CostUnknown = false
					row.CostBasis = ""
					row.Usage = nil
					row.Model = ""
					row.Generation++
					row.OperationID = model.OperationID(s.RunID, id, row.Generation)
					row.Result = nil
					row.Validation = ""
					row.Status, row.Error = "pending", ""
					if checkpoint.ManagedRun(s) {
						row.BindingDigest = model.ManagedCurrentBinding(s, model.ManagedProfile(s.Plan, row.Spec)).Digest
					}
					if checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
						row.ExpansionHash = ""
					}
				}
			}
		}
		s.Event("resumed", "", map[string]int{"workers": opts.Workers})
		if checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
			return checkpoint.ValidateTurnState(s)
		}
		return nil
	}); err != nil {
		return 1, err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	ui := dashboard.NewTerminalUI(os.Stdout, root)
	if ui != nil {
		defer ui.Close()
	}
	if perr := session.PublishSession(session.ActiveSessionRegistry, root, previous.RunID); perr != nil {
		fmt.Fprintln(os.Stderr, "warning: run NOT registered in the session registry; it may be unavailable in the session registry:", perr)
	}
	executor, err := executor.ResolveWorkerExecutor(root, executor.ActiveExecutor)
	if err != nil {
		return 1, err
	}
	notifier, err := approvals.NewRunNotifier(root, approvals.ActiveNotify, os.Stderr)
	if err != nil {
		return 1, err
	}
	defer notifier.Flush()
	children := map[string]*controllerChild{}
	ownedChildren := map[string]*controllerChild{}
	defer func() {
		if runErr != nil && len(ownedChildren) > 0 {
			if drainErr := drainControllerChildren(root, ownedChildren, 3*time.Second, 8*time.Second); drainErr != nil {
				runErr = errors.Join(runErr, drainErr)
			}
		}
	}()
	wakeup := make(chan struct{}, 1)
	last := ""
	for {
		select {
		case <-signals:
			if err := checkpoint.SetControl(root, "pause"); err != nil {
				return 1, err
			}
		default:
		}
		// Experimental channels: post newly verified outputs (idempotent).
		if err = messaging.PublishChannels(root); err != nil {
			return 1, err
		}
		if err = messaging.ProcessMailbox(root, 32); err != nil {
			return 1, err
		}
		if err = Reconcile(root); err != nil {
			return 1, err
		}
		if err = graph.ProcessRevisionProposals(root); err != nil {
			return 1, err
		}
		s, err := checkpoint.ReadState(root)
		if err != nil {
			return 1, err
		}
		notifier.Observe(s)
		var snapshot strings.Builder
		snapshot.WriteString(s.Control)
		for _, spec := range s.AllTasks() {
			fmt.Fprintf(&snapshot, "%s:%s;", spec.ID, s.Tasks[spec.ID].Status)
		}
		if ui != nil {
			ui.Render(s, clock.ActiveClock.Now())
		} else if snapshot.String() != last {
			dashboard.Display(s, root)
			last = snapshot.String()
		}
		for id, child := range children {
			select {
			case <-child.done:
				childErr := child.err
				delete(children, id)
				if childErr != nil {
					return 1, fmt.Errorf("worker %s exited: %w; resume to recover", id, childErr)
				}
			default:
			}
		}
		busy := map[string]bool{}
		for id := range children {
			busy[id] = true
		}
		for _, spec := range s.AllTasks() {
			lease, e := checkpoint.FileLock(filepath.Join(root, "cells", spec.ID, "lease"), true)
			if errors.Is(e, syscall.EWOULDBLOCK) {
				busy[spec.ID] = true
				continue
			}
			if e != nil {
				return 1, e
			}
			checkpoint.Unlock(lease)
		}
		// A worker can commit and release its lease between reconcile and this
		// scan. Reconcile that response before declaring the run drained/blocked.
		needsReconcile := false
		for id, row := range s.Tasks {
			if !busy[id] && ((row.Status == "produced" && s.Control != "down") || row.Status == "running") {
				needsReconcile = true
			}
		}
		if needsReconcile {
			continue
		}
		if s.Control != "run" {
			if len(busy) == 0 {
				return 0, nil
			}
		} else {
			var ready []string
			for _, spec := range s.AllTasks() {
				if s.Tasks[spec.ID].Status != "pending" || s.Tasks[spec.ID].CancelRequested || busy[spec.ID] {
					continue
				}
				ok := true
				for _, d := range spec.DependsOn {
					ok = ok && s.Tasks[d].Status == "verified"
				}
				if ok && managed.ManagedCanBegin(s, s.Tasks[spec.ID]) == nil {
					ready = append(ready, spec.ID)
				}
			}
			orderReady(s, ready)
			slots := opts.Workers - len(busy)
			if sizing != nil {
				ready, slots = sizing.Admit(s, ready, busy, slots, opts.Budget)
			}
			for _, id := range ready {
				if slots <= 0 {
					break
				}
				slots--
				binary, e := os.Executable()
				if e != nil {
					return 1, e
				}
				attempt := s.Tasks[id].Attempt + 1
				workerArgs := []string{"_worker", root, id, "--operation", s.Tasks[id].OperationID, "--timeout", opts.Timeout.String(), "--budget", strconv.FormatFloat(opts.Budget, 'f', -1, 64)}
				cmd, process, release, e := executor.Start(binary, root, s.RunID, id, attempt, workerArgs)
				if e != nil {
					return 1, e
				}
				child := &controllerChild{process: process, done: make(chan struct{}), attempt: attempt}
				children[id] = child
				ownedChildren[id] = child
				commandflags.GoLabeled(func() {
					child.err = cmd.Wait()
					release()
					close(child.done)
					select {
					case wakeup <- struct{}{}:
					default:
					}
				}, "irrigator.role", "worker-wait", "run.id", s.RunID, "task.id", id, "attempt", strconv.Itoa(attempt))
			}
			if len(busy) == 0 && len(ready) == 0 {
				complete := true
				for _, row := range s.Tasks {
					complete = complete && row.Status == "verified"
				}
				control, code := "blocked", 1
				if complete {
					control, code = "complete", 0
				}
				if err = checkpoint.Update(root, func(s *model.State) error {
					s.Control = control
					s.Event(control, "", nil)
					if checkpoint.ManagedRun(s) && control == "blocked" {
						for id, row := range s.Tasks {
							if row.Status == "pending" {
								if err := managed.ManagedCanBegin(s, row); err != nil {
									s.Event("budget_blocked", id, err.Error())
								}
							}
						}
					}
					graph.RecordAdaptiveOutcome(s, control, clock.ActiveClock.Now().UTC())
					return nil
				}); err != nil {
					return 1, err
				}
				s, err = checkpoint.ReadState(root)
				if err != nil {
					return 1, err
				}
				notifier.Observe(s)
				if ui != nil {
					ui.Render(s, clock.ActiveClock.Now())
				} else {
					dashboard.Display(s, root)
				}
				return code, nil
			}
		}
		// Local completions wake the scheduler immediately. Polling remains a
		// recovery fallback for adopted workers and external control requests.
		select {
		case <-wakeup:
		case <-signals:
			if err := checkpoint.SetControl(root, "pause"); err != nil {
				return 1, err
			}
		case <-clock.ActiveClock.After(50 * time.Millisecond):
		}
	}
}

// A controller only signals process handles it created. Persisted worker and
// provider PIDs remain diagnostic evidence, never cleanup targets.
type controllerChild struct {
	process executor.ChildProcess
	attempt int
	done    chan struct{}
	err     error // published by closing done; never read before completion
}

func controllerFailureDown(root string) error {
	return (checkpoint.FileCheckpointStore{Root: root}).Update(context.Background(), checkpoint.CheckpointUpdateOptions{Nonblocking: true}, func(s *model.State) error {
		if s.Control == "down" {
			return nil
		}
		s.Control = "down"
		s.Event("controller_failed", "", map[string]string{"action": "drain_owned_workers"})
		return nil
	})
}

func drainControllerChildren(root string, children map[string]*controllerChild, grace, limit time.Duration) error {
	return drainControllerChildrenWithClock(clock.ActiveClock, root, children, grace, limit)
}

// drainControllerChildrenWithClock measures the grace and limit deadlines and
// paces its polling with the same clock, so neither can drift from the other.
// Nothing here blocks except clock.Sleep, which returns after one 20ms step; the
// loop always ends once every child is confirmed gone or limit has elapsed.
func drainControllerChildrenWithClock(clock clock.Clock, root string, children map[string]*controllerChild, grace, limit time.Duration) error {
	started := clock.Now()
	var cleanupErrors []error
	if err := controllerFailureDown(root); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("cannot persist controller shutdown: %w", err))
	}
	for _, child := range children {
		select {
		case <-child.done:
			continue
		default:
		}
		if err := child.process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("signal owned worker: %w", err))
		}
	}
	escalated := false
	for {
		pending := []string{}
		escalateNow := !escalated && clock.Now().Sub(started) >= grace
		for id, child := range children {
			select {
			case <-child.done:
			default:
				pending = append(pending, id)
				if escalateNow {
					if err := child.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
						cleanupErrors = append(cleanupErrors, fmt.Errorf("stop owned worker: %w", err))
					}
				}
				continue
			}
			// The supervisor retains the same cell lease after a worker dies, until
			// its own provider group has drained. Do not return merely after Wait.
			lease, err := checkpoint.FileLock(filepath.Join(root, "cells", id, "lease"), true)
			if err != nil {
				pending = append(pending, id)
				continue
			}
			unresolved, markerErr := launch.UnresolvedProcess(filepath.Join(root, "cells", id), child.attempt)
			checkpoint.Unlock(lease)
			if unresolved || markerErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("provider cleanup unconfirmed for %s; inspect process evidence before resuming", id))
			}
		}
		if escalateNow {
			escalated = true
		}
		if len(pending) == 0 {
			return errors.Join(cleanupErrors...)
		}
		if clock.Now().Sub(started) >= limit {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("owned worker cleanup unconfirmed for %s; inspect the run before resuming", strings.Join(pending, ",")))
			return errors.Join(cleanupErrors...)
		}
		clock.Sleep(20 * time.Millisecond)
	}
}
