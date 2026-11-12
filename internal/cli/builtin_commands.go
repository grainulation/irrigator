package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/dashboard"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runops"
	"github.com/grainulation/irrigator/internal/session"
)

func cmdInit(out, initProfile string, managedInit bool) (int, error) {
	if managedInit {
		return 0, managed.WriteManagedInitialPlan(out, initProfile)
	}
	if initProfile != "" {
		return 2, errors.New("--profile requires --managed")
	}
	p := model.Plan{APIVersion: "swarm/v1", Name: "first Irrigator graph", Tasks: []model.Task{
		{ID: "draft", Prompt: "Write a concise implementation brief for a durable task runner.", Requires: []string{"brief"}, Outputs: map[string]string{"brief": "brief.txt"}},
		{ID: "review", Prompt: "Review the dependency brief. Identify the main recovery risk and a concrete test.", DependsOn: []string{"draft"}, Requires: []string{"review"}, Outputs: map[string]string{"review": "review.txt"}},
	}}
	data, _ := json.MarshalIndent(p, "", "  ")
	return 0, localfs.WriteNew(out, append(data, '\n'))
}

func cmdPlan(target string, contextFiles checkpoint.ContextImportFlags, out string, asJSON bool) (int, error) {
	if len(contextFiles) > 0 && out == "" {
		return 2, errors.New("--context-file requires --out NEWPLAN; the source plan is never changed")
	}
	p, err := checkpoint.LoadPlanWithContext(target, contextFiles)
	if err != nil {
		return 1, err
	}
	if out != "" {
		if err := checkpoint.WriteContextPlan(out, p); err != nil {
			return 1, err
		}
		if !asJSON {
			fmt.Println(out)
			return 0, nil
		}
	}
	if asJSON {
		return 0, jsonio.PrintJSON(p)
	}
	for _, t := range p.Tasks {
		fmt.Printf("%s <- %v | fields: %v | checks: %d", t.ID, t.DependsOn, t.Requires, len(t.Checks))
		if t.Spawn != nil {
			fmt.Printf(" | spawn from %s", t.Spawn.Field)
		}
		fmt.Println()
	}
	return 0, nil
}

func cmdApply(command, target, root, backend, executorFlag, sessionRegistry, schedule string, opts model.Options, callBudgetSet, explicitBackend bool) (int, error) {
	if root == "" {
		return 2, errors.New("--run is required")
	}
	p, err := checkpoint.LoadPlan(target)
	if err != nil {
		return 1, err
	}
	if launch.IsManagedPlanVersion(p.APIVersion) {
		if callBudgetSet {
			return 2, errors.New("managed plans use profile operation budgets; --call-budget/--budget are unsupported")
		}
		if explicitBackend && backend != "managed" {
			return 2, errors.New("managed plans require backend managed")
		}
		backend = "managed"
	}
	if backend == "" {
		return 2, errors.New("--backend is required: use mock for a free run, or explicitly select haiku or a named provider")
	}
	if err = checkpoint.Initialize(p, root, backend, opts); err != nil {
		return 1, err
	}
	if err = executor.RecordExecutorChoice(root, executorFlag); err != nil {
		return 1, err
	}
	if command == "prepare" {
		fmt.Println(root)
		return 0, nil
	}
	if session.ActiveSessionRegistry, err = session.ResolveRegistryPath(sessionRegistry); err != nil {
		return 1, err
	}
	runops.ActiveSchedulingOrder = schedule
	return runops.Run(root, opts)
}

// admitLegacyResume atomically decides and records consent for a resume. Under the
// state lock it either leaves a non-gated run untouched, persists an explicit
// positive call budget and/or the explicit allow decision (idempotently: nothing
// is appended once a decision exists), or refuses without writing anything. It
// never modifies the plan, plan hash, task results or existing history.
func admitLegacyResume(root string, allow, explicitBudget bool, opts model.Options) error {
	return checkpoint.Update(root, func(s *model.State) error {
		if !executor.UnbudgetedGateApplies(s) {
			return nil
		}
		if !allow && !explicitBudget {
			return executor.ErrLegacyUnbudgeted
		}
		if explicitBudget {
			if !model.ValidCallBudget(opts.Budget) || opts.Timeout <= 0 {
				return errors.New("explicit call budget must be positive and finite")
			}
			model.SaveCallLimits(s, opts)
			s.Event("unbudgeted_call_budget_set", "", map[string]float64{"call_budget_usd": opts.Budget})
		}
		if allow {
			s.UnbudgetedDecision = checkpoint.UnbudgetedAllow
			s.Event("legacy_unbudgeted_allowed", "", nil)
		}
		return nil
	})
}

func cmdResume(target, sessionRegistry, schedule, legacyUnbudgeted string, retry bool, opts model.Options, timeoutSet, callBudgetSet bool) (int, error) {
	if legacyUnbudgeted != "" && legacyUnbudgeted != checkpoint.UnbudgetedAllow {
		return 2, fmt.Errorf("invalid --legacy-unbudgeted value %q: the only accepted value is --legacy-unbudgeted=allow", legacyUnbudgeted)
	}
	saved, err := checkpoint.ReadState(target)
	if err != nil {
		return 1, err
	}
	if callBudgetSet && checkpoint.ManagedRun(saved) {
		return 2, errors.New("managed runs use profile operation budgets; --call-budget/--budget are unsupported")
	}
	opts = model.SavedCallOptions(saved, opts, timeoutSet, callBudgetSet)
	// Only a gated run needs this transaction; the snapshot decides whether to
	// pay for it, while the transaction itself re-evaluates under the state lock.
	// Non-gated runs are untouched here (no write, no migration side effect).
	if executor.UnbudgetedGateApplies(saved) {
		if err := admitLegacyResume(target, legacyUnbudgeted == checkpoint.UnbudgetedAllow, callBudgetSet, opts); err != nil {
			if errors.Is(err, executor.ErrLegacyUnbudgeted) {
				return 2, err
			}
			return 1, err
		}
	}
	var rerr error
	if session.ActiveSessionRegistry, rerr = session.ResolveRegistryPath(sessionRegistry); rerr != nil {
		return 1, rerr
	}
	runops.ActiveSchedulingOrder = schedule
	return runops.Run(target, opts)
}

func cmdRetire(target, retireReason, retireSuperseded, sessionRegistry string) (int, error) {
	reason, verr := session.ValidateRetireInputs(target, retireReason, retireSuperseded)
	if verr != nil {
		return 2, verr
	}
	regPath, perr := session.ResolveRegistryPath(sessionRegistry)
	if perr != nil {
		return 1, errors.New("session registry path could not be resolved")
	}
	if _, rerr := session.RetireSession(regPath, target, reason, retireSuperseded); rerr != nil {
		return 1, rerr
	}
	fmt.Println("retired " + target + " in the session registry (run state and checkpoints unchanged; resume reactivates it)")
	return 0, nil
}

func cmdCancel(target, task string) (int, error) {
	if err := runops.CancelNode(target, task); err != nil {
		return 1, err
	}
	s, err := checkpoint.ReadState(target)
	if err != nil {
		return 1, err
	}
	if s.Tasks[task].Status == "verified" {
		fmt.Printf("Task %s is already verified; its accepted result was preserved.\n", task)
	}
	return 0, nil
}

func cmdRetry(target, task string, recursive, allowUncertain, confirmStopped bool) (int, error) {
	report, err := runops.RetryNodeWithReport(target, task, recursive, allowUncertain, confirmStopped)
	if err != nil {
		return 1, err
	}
	if len(report.Retried) > 0 {
		fmt.Printf("Prepared retry for: %s. Resume the run to execute.\n", strings.Join(report.Retried, ", "))
	}
	if len(report.Waiting) > 0 {
		fmt.Printf("Preserved accepted parent results; waiting for children: %s.\n", strings.Join(report.Waiting, ", "))
	}
	if len(report.Skipped) > 0 {
		fmt.Printf("Skipped unchanged tasks: %s. Accepted results were preserved.\n", strings.Join(report.Skipped, ", "))
	}
	if len(report.Retried)+len(report.Waiting)+len(report.Skipped) == 0 {
		fmt.Println("Nothing to retry; no eligible failed or cancelled tasks were changed.")
	}
	return 0, nil
}

func cmdResolve(target, task, resultPath string, confirmStopped bool) (int, error) {
	if resultPath == "" {
		return 2, errors.New("--result is required")
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return 1, err
	}
	var result map[string]string
	if err = json.Unmarshal(data, &result); err != nil {
		return 1, err
	}
	if err := runops.ResolveNode(target, task, result, confirmStopped); err != nil {
		return 1, err
	}
	row, err := runops.InspectNode(target, task)
	if err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(row)
}

func cmdPropose(target, proposalPath string) (int, error) {
	f, err := os.Open(proposalPath)
	if err != nil {
		return 1, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8*1024*1024+1))
	if err != nil {
		return 1, err
	}
	if len(data) > 8*1024*1024 {
		return 1, errors.New("proposal exceeds 8 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var proposal model.RevisionProposal
	if err := decoder.Decode(&proposal); err != nil {
		return 1, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return 1, errors.New("proposal must contain exactly one JSON object")
	}
	if err := graph.QueueRevision(target, proposal); err != nil {
		return 1, err
	}
	fmt.Printf("Proposal %s queued. A running controller evaluates it automatically; otherwise run irrigator resume %q.\n", proposal.ID, target)
	return 0, nil
}

func cmdPolicy(target, policyPath string) (int, error) {
	f, err := os.Open(policyPath)
	if err != nil {
		return 1, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return 1, err
	}
	if len(data) > 1024*1024 {
		return 1, errors.New("policy exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy model.AdaptivePolicy
	if err := decoder.Decode(&policy); err != nil {
		return 1, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return 1, errors.New("policy must contain exactly one JSON object")
	}
	if err := checkpoint.ValidateAdaptivePolicy(policy); err != nil {
		return 1, err
	}
	return 0, checkpoint.Update(target, func(s *model.State) error {
		s.Policy = &policy
		s.Event("adaptive_policy_updated", "", policy)
		return nil
	})
}

func cmdDecisions(target string) (int, error) {
	s, err := checkpoint.ReadState(target)
	if err != nil {
		return 1, err
	}
	policy := graph.DefaultAdaptivePolicy()
	if s.Policy != nil {
		policy = *s.Policy
	}
	return 0, jsonio.PrintJSON(map[string]any{"graph_revision": s.GraphRevision, "policy": policy, "proposals": s.Proposals, "decisions": s.Decisions, "revisions": s.Revisions, "reported_cost_usd": s.ReportedCost()})
}

func cmdExport(target, out, dir string) (int, error) {
	if out != "" && dir != "" {
		return 2, errors.New("choose --out or --dir")
	}
	return 0, runops.ExportArtifacts(target, out, dir)
}

func cmdGC(target string, keep int, apply, dryRun bool) (int, error) {
	if apply && dryRun {
		return 2, errors.New("choose --apply or --dry-run")
	}
	report, err := runops.GarbageCollect(target, keep, apply)
	if err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(report)
}

func cmdLogs(root string, since int, task string, follow, asJSON bool) (int, error) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	for {
		s, err := checkpoint.ReadState(root)
		if err != nil {
			return 1, err
		}
		if task != "" && s.Tasks[task] == nil {
			return 1, errors.New("unknown node filter")
		}
		for i, e := range s.Events {
			if i+1 <= since {
				continue
			}
			if task != "" && e.Task != task {
				continue
			}
			if asJSON {
				if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"seq": i + 1, "event": e}); err != nil {
					return 1, err
				}
			} else {
				fmt.Printf("%d %s %-20s %s\n", i+1, e.Time.Format(time.RFC3339), e.Kind, e.Task)
			}
		}
		since = len(s.Events)
		if !follow {
			return 0, nil
		}
		select {
		case <-signals:
			return 0, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func cmdStatus(command, target string, watch, asJSON bool, interval time.Duration) (int, error) {
	var ui *dashboard.TerminalUI
	if command == "status" && watch && !asJSON {
		ui = dashboard.NewTerminalUI(os.Stdout, target)
		if ui != nil {
			ui.WatchOnly = true
			defer ui.Close()
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	for {
		s, err := checkpoint.ReadState(target)
		if err != nil {
			return 1, err
		}
		if command == "events" {
			return 0, jsonio.PrintJSON(s.Events)
		}
		if command == "artifacts" {
			results := map[string]map[string]string{}
			for id, t := range s.Tasks {
				if t.Status == "verified" {
					results[id] = t.Result
				}
			}
			return 0, jsonio.PrintJSON(results)
		}
		if watch && s.Control == "run" {
			if err := dashboard.StatusWatchController(target); err != nil {
				// A controller can finish between the snapshot and lock probe.
				// Refresh before diagnosing its absence; never rewrite durable state.
				latest, readErr := checkpoint.ReadState(target)
				if readErr != nil {
					return 1, readErr
				}
				if latest.Control == "run" {
					return 1, err
				}
				s = latest
			}
		}
		if asJSON {
			if err := jsonio.PrintJSON(dashboard.NewStatusReport(s, target)); err != nil {
				return 1, err
			}
		} else if ui != nil {
			ui.Render(s, time.Now())
		} else {
			dashboard.Display(s, target)
			dashboard.PrintStatusExecutor(target)
		}
		if !watch || s.Control == "complete" || s.Control == "blocked" || (s.Control != "run" && !liveWorkers(target, s)) {
			return 0, nil
		}
		select {
		case <-signals:
			return 0, nil
		case <-time.After(interval):
		}
	}
}

func liveWorkers(root string, s *model.State) bool {
	for id := range s.Tasks {
		lease, err := checkpoint.FileLock(filepath.Join(root, "cells", id, "lease"), true)
		if err != nil {
			return true
		}
		checkpoint.Unlock(lease)
	}
	return false
}
