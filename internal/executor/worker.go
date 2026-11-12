package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	costpkg "github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/providerenv"
	"github.com/grainulation/irrigator/internal/terminalview"
)

var errStopped = errors.New("swarm stopped")

// workerProgressEnv turns on short progress lines on a worker's stdout. The
// herdr launcher sets it so `hc attach` and `hc read` show something; the local
// executor never does, so the dashboard is unaffected.
const workerProgressEnv = "IRRIGATOR_WORKER_PROGRESS"

// workerProgressOut is where progress lines go. Tests replace it.
var workerProgressOut io.Writer = os.Stdout

// workerProgress prints one line when progress is enabled, and sets the
// terminal title (OSC 2) to the same text so `hc ls` and tab bars show the
// attempt's state. Callers pass only ids, counters and states: never prompts,
// outputs, errors or credentials.
func workerProgress(format string, args ...any) {
	if os.Getenv(workerProgressEnv) != "1" {
		return
	}
	line := "irrigator: " + fmt.Sprintf(format, args...)
	fmt.Fprint(workerProgressOut, terminalview.TitleOSC(line)+line+"\n")
}

// workerMark writes an OSC 133 semantic mark when progress is enabled, so a
// terminal can jump between attempts and select one attempt's output.
func workerMark(mark string) {
	if os.Getenv(workerProgressEnv) != "1" {
		return
	}
	fmt.Fprint(workerProgressOut, mark)
}

func workerProgressCost(cost float64, unknown bool, usage ...*model.TaskUsage) string {
	var u *model.TaskUsage
	if len(usage) > 0 {
		u = usage[0]
	}
	if unknown {
		if u != nil && u.TotalTokens > 0 {
			return fmt.Sprintf("cost unknown (%s)", model.FormatTokenSummary(u))
		}
		return "cost unknown"
	}
	if u != nil && u.TotalTokens > 0 {
		return fmt.Sprintf("cost $%.4f (%s)", cost, model.FormatTokenSummary(u))
	}
	return fmt.Sprintf("cost $%.4f", cost)
}

// A queued subprocess may start after a graph revision has superseded its task.
// Fence admission as well as result commit so that it cannot run the new task
// using the old controller's scheduling decision.
func WorkerOperation(root, id string, opts model.Options, expectedOperation string) error {
	return workerOperationWithExecutor(root, id, opts, expectedOperation, nil)
}

func workerOperationWithExecutor(root, id string, opts model.Options, expectedOperation string, factory attemptExecutorFactory) error {
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return err
	}
	// Look up the task before deriving a filesystem path from its ID.
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	if s.Tasks[id] == nil {
		if expectedOperation != "" {
			return nil
		}
		return errors.New("unknown task")
	}
	if expectedOperation != "" && s.Tasks[id].OperationID != expectedOperation {
		return nil
	}
	cell := filepath.Join(root, "cells", id)
	lease, err := checkpoint.FileLock(filepath.Join(cell, "lease"), true)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil
	}
	if err != nil {
		return err
	}
	defer checkpoint.Unlock(lease)
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return err
	}
	cells := CheckpointCells{Root: root}
	admitted, err := cells.BeginAttempt(id, expectedOperation, os.Getpid())
	if errors.Is(err, launch.ErrNoWork) {
		return nil
	}
	if err != nil {
		return err
	}
	attempt, operation := admitted.Fence.Attempt, admitted.Fence.OperationID
	workerMark(terminalview.MarkPromptStart + terminalview.MarkPromptEnd)
	workerProgress("%s attempt %d admitted", id, attempt)
	s = admitted.snapshot
	// Profiles pin their timeout with the immutable binding. Apply it before
	// creating the worker deadline, not only in the serialized provider request.
	if checkpoint.ManagedRun(s) {
		if b, bindingErr := checkpoint.ManagedBinding(s, s.Tasks[id]); bindingErr == nil && b.Profile.TimeoutSeconds > 0 {
			opts.Timeout = time.Duration(b.Profile.TimeoutSeconds) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), launch.ProcessLeaseKey{}, lease), opts.Timeout)
	defer cancel()
	// A normal signal or down request cancels the complete model process group.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	watchDone := make(chan struct{})
	commandflags.GoLabeled(func() {
		defer close(watchDone)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				cancel()
				return
			case <-tick.C:
				current, e := checkpoint.ReadState(root)
				if e != nil || current.Control == "down" || current.Tasks[id] == nil || current.Tasks[id].OperationID != operation || current.Tasks[id].CancelRequested {
					cancel()
					return
				}
			}
		}
	}, "irrigator.role", "worker-watch", "run.id", s.RunID, "task.id", id, "attempt", strconv.Itoa(attempt))
	workerMark(terminalview.MarkOutputStart)
	workerProgress("%s attempt %d provider started", id, attempt)
	receipt, executionErr := executeCellAttempt(ctx, s, admitted, cell, opts, factory)
	result, cost, costUnknown, diag := receipt.Output, receipt.CostUSD, receipt.CostUnknown, receipt.Diagnostic
	err = executionErr
	cancel()
	<-watchDone
	unresolved, markerErr := launch.UnresolvedProcess(cell, attempt)
	commitErr := cells.RecordOutcome(managed.CellOutcome{
		Fence: admitted.Fence, Result: result, Cost: cost, CostUnknown: costUnknown,
		CostBasis: receipt.CostBasis, Usage: receipt.Usage, Model: receipt.Model,
		Err: err, Diagnostic: diag,
		CleanupUnresolved: unresolved, CleanupError: markerErr,
	})
	if commitErr != nil {
		return commitErr
	}
	state := "settled"
	switch {
	case unresolved || markerErr != nil || (err != nil && receipt.Outcome == model.OutcomeUncertain):
		state = "uncertain"
	case err != nil:
		state = "failed"
	}
	workerProgress("%s attempt %d %s, %s", id, attempt, state, workerProgressCost(cost, costUnknown, receipt.Usage))
	exit := 0
	if state != "settled" {
		exit = 1
	}
	workerMark(terminalview.MarkCommandEnd(exit))
	if err == nil && !unresolved && markerErr == nil {
		return Finalize(root, id, lease)
	}
	return nil
}

func callHaikuBounded(ctx context.Context, cell string, attempt int, task model.Task, inputs map[string]map[string]string, budget float64, boundaries *model.WorkerBoundaries, contexts ...map[string]string) (map[string]string, float64, error) {
	schema := artifactstore.ModelSchemaForRequest(model.WorkRequest{Task: task, Boundaries: boundaries})
	schemaJSON, _ := json.Marshal(schema)
	var selectedContext map[string]string
	if len(contexts) > 0 {
		selectedContext = contexts[0]
	}
	payload := map[string]any{"task": task.Prompt, "context": selectedContext, "dependency_artifacts": inputs, "acceptance": task.Equals, "checks": task.Checks, "output_schema": schema}
	if boundaries != nil {
		payload["execution_boundaries"] = boundaries
	}
	if publication := model.DisplayPublicationInstructions(task); publication != nil {
		payload["display_publication"] = publication
	}
	prompt, _ := json.Marshal(payload)
	prefix := filepath.Join(cell, strconv.Itoa(attempt))
	effort := task.Effort
	if effort == "" {
		effort = "low"
	}
	cmd := exec.Command("claude", "-p", "--model", "haiku", "--safe-mode", "--tools", "",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--disable-slash-commands",
		"--no-session-persistence", "--output-format", "json", "--json-schema", string(schemaJSON),
		"--max-budget-usd", strconv.FormatFloat(budget, 'f', -1, 64), "--effort", effort, "--system-prompt",
		"Complete the supplied task using only the supplied inputs. Return the requested structured output. When turn_request is declared, return a JSON object for it; use {} to stop, never an empty or omitted value or a JSON-encoded string. Be concise.")
	cmd.Dir = cell
	cmd.Env = providerenv.Claude()
	raw, err := launch.RunProcess(ctx, cmd, prefix, prompt)
	if model.KnownNotStarted(err) {
		return nil, 0, err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// runProcess returns the cancellation rather than stdout, but the
		// supervisor has already drained the process and preserved its report.
		raw, _ = model.ReadProcessEvidence(prefix+".stdout.json", model.MaxOutputBytes)
	}
	cost, costKnown := provider.HaikuReportedCost(raw)
	if !costKnown {
		cost = 0
	}
	uncertain := func(cause error) (map[string]string, float64, error) {
		return nil, cost, model.ClaudeOutcomeError(model.OutcomeUncertain, cause)
	}
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !model.ProviderProcessStopped(prefix)) {
		return uncertain(err)
	}
	var reply struct {
		Type       string          `json:"type"`
		Subtype    string          `json:"subtype"`
		Structured json.RawMessage `json:"structured_output"`
		Result     string          `json:"result"`
		IsError    bool            `json:"is_error"`
	}
	if parseErr := json.Unmarshal(raw, &reply); parseErr != nil {
		return uncertain(fmt.Errorf("Claude returned invalid JSON (%v); see %s.stderr.log", parseErr, prefix))
	}
	object, valid := provider.StrictCostObject(raw, "type", "is_error", "subtype", "result", "structured_output", "total_cost_usd")
	if !valid || (reply.Type != "" && reply.Type != "result") {
		return uncertain(errors.New("Claude returned no unambiguous terminal result"))
	}
	if value, present := object["is_error"]; present {
		if _, ok := value.(bool); !ok {
			return uncertain(errors.New("Claude is_error must be boolean"))
		}
	}
	if model.ProviderResultFailure(reply.IsError, reply.Subtype) {
		// Only a complete terminal result can settle a nonzero CLI exit.
		if err != nil && reply.Type != "result" {
			return uncertain(err)
		}
		_, message := model.ClaudeResultDiagnostic(reply.Subtype, reply.Result)
		return nil, cost, model.WithOutcome(model.OutcomeSettled, errors.New(message))
	}
	if err != nil {
		return uncertain(err)
	}
	if costpkg.CostExceeds(cost, budget) {
		return nil, cost, model.WithOutcome(model.OutcomeSettled, errors.New("Claude exceeded per-call budget"))
	}
	if len(reply.Structured) == 0 || string(reply.Structured) == "null" {
		reply.Structured = json.RawMessage(reply.Result)
	}
	result, normalizeErr := artifactstore.NormalizeModelOutput(task, reply.Structured)
	return result, cost, model.WithOutcome(model.OutcomeSettled, normalizeErr)
}
