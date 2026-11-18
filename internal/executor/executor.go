package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clock"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
)

const attemptExecutorVersion = "irrigator.attempt-executor/v1"

// Namespace is local identity, not tenant authentication. No network endpoint is
// exposed here. A hosted transport must bind its authenticated principal itself.
type attemptExecutionIdentity struct {
	Namespace     string `json:"namespace"`
	RunID         string `json:"run_id"`
	TaskID        string `json:"task_id"`
	OperationID   string `json:"operation_id"`
	Attempt       int    `json:"attempt"`
	RequestID     string `json:"request_id"`
	BindingDigest string `json:"binding_digest,omitempty"`
	Route         string `json:"route"`
}

// Work contains the actual selected task/context/dependencies, not a local
// lookup handle. LaunchContext must be nil: host capsules are prepared only by
// the local adapter. Commands/credentials/cell paths are adapter configuration.
type attemptExecutionRequest struct {
	Version          string                   `json:"version"`
	Identity         attemptExecutionIdentity `json:"identity"`
	Work             model.WorkRequest        `json:"work"`
	History          *managed.ManagedHistory  `json:"history,omitempty"`
	Digest           string                   `json:"sha256"`
	AllowUnknownCost bool                     `json:"allow_unknown_cost"`
	OutputLimitBytes int64                    `json:"output_limit_bytes"`
}

type attemptExecutionReceipt struct {
	Version       string                   `json:"version"`
	Identity      attemptExecutionIdentity `json:"identity"`
	RequestDigest string                   `json:"request_sha256"`
	Output        map[string]string        `json:"output,omitempty"`
	CostUSD       float64                  `json:"cost_usd"`
	CostUnknown   bool                     `json:"cost_unknown"`
	CostBasis     string                   `json:"cost_basis,omitempty"`
	Usage         *model.TaskUsage         `json:"usage,omitempty"`
	Model         string                   `json:"model,omitempty"`
	Outcome       string                   `json:"outcome"`
	OutcomeReason string                   `json:"outcome_reason,omitempty"`
	Error         string                   `json:"error,omitempty"`
	ErrorKind     string                   `json:"error_kind,omitempty"`
	Diagnostic    string                   `json:"diagnostic,omitempty"`
}

// Execute errors mean no trustworthy receipt was delivered. Adapters with
// attributable cost or usage evidence must return a structured uncertain receipt
// and nil transport error, even when the output itself was lost.
type attemptExecutor interface {
	execute(context.Context, attemptExecutionRequest) (attemptExecutionReceipt, error)
}

// Injection is per attempt invocation, never a process-global mutable registry.
type attemptExecutorFactory func(localAttemptExecutor) attemptExecutor

type localAttemptExecutor struct {
	cell     string
	provider string
	command  model.CommandSpec
	binding  *model.ExecutionBinding
}

func attemptRequestDigest(r attemptExecutionRequest) (string, error) {
	r.Digest = ""
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	if len(raw) > model.MaxInputBytes {
		return "", errors.New("executor request exceeds input limit")
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

// MaxCallTimeout is the longest single provider call a worker accepts.
const MaxCallTimeout = time.Hour

func validateAttemptRequest(r attemptExecutionRequest) error {
	if r.Version != attemptExecutorVersion || r.Identity.Namespace != "local" || r.Identity.RunID == "" || r.Identity.TaskID == "" || r.Identity.RequestID == "" || r.Identity.OperationID == "" || r.Identity.Attempt < 1 || r.Identity.Route == "" || r.Work.LaunchContext != nil || r.Work.Protocol != model.ProtocolVersion || r.Work.Kind != "task" || r.Work.RunID != r.Identity.RunID || r.Work.Task.ID != r.Identity.TaskID || r.Work.OperationID != r.Identity.OperationID || r.Work.Attempt != r.Identity.Attempt || r.Work.RequestID != r.Identity.RequestID || r.Work.TimeoutMS <= 0 || r.Work.TimeoutMS > MaxCallTimeout.Milliseconds() || r.OutputLimitBytes < 1 || r.OutputLimitBytes > model.MaxOutputBytes || r.Work.MaxCostUSD < 0 || math.IsNaN(r.Work.MaxCostUSD) || math.IsInf(r.Work.MaxCostUSD, 0) {
		return errors.New("invalid executor request identity or limits")
	}
	digest, err := attemptRequestDigest(r)
	if err != nil {
		return err
	}
	if digest != r.Digest {
		return errors.New("executor request digest mismatch")
	}
	return nil
}

// Only fixed diagnostic classes cross the executor boundary. Never project
// provider error text into the public uncertainty reason.
func executorOutcomeReason(reason string) string {
	switch reason {
	case "deadline", "cancelled", "malformed_response", "invalid_outcome", "provider_reported", "missing_cost":
		return reason
	default:
		return ""
	}
}

func executionReceipt(r attemptExecutionRequest, response model.WorkResponse, err error) attemptExecutionReceipt {
	basis := response.CostBasis
	if basis == "" {
		if response.CostUnknown {
			basis = "unknown"
		} else {
			basis = "reported"
		}
	}
	modelName := response.Model
	if modelName == "" && response.Usage != nil {
		modelName = response.Usage.Model
	}
	receipt := attemptExecutionReceipt{
		Version:       attemptExecutorVersion,
		Identity:      r.Identity,
		RequestDigest: r.Digest,
		Output:        response.Output,
		CostUSD:       response.CostUSD,
		CostUnknown:   response.CostUnknown,
		CostBasis:     basis,
		Usage:         response.Usage,
		Model:         modelName,
		Outcome:       model.OutcomeSettled,
		Diagnostic:    model.KnownDiagnosticCode(response.DiagnosticCode),
	}
	if err != nil {
		receipt.Error = err.Error()
		switch {
		case model.KnownNotStarted(err):
			receipt.Outcome = model.OutcomeNotStarted
		case model.IsUncertain(err):
			receipt.Outcome = model.OutcomeUncertain
			receipt.OutcomeReason = executorOutcomeReason(model.OutcomeReason(err))
		case managed.ManagedSettledError(err):
			receipt.Outcome = model.OutcomeSettled
		default:
			receipt.Outcome = "legacy"
		}
		switch {
		case errors.Is(err, context.Canceled):
			receipt.ErrorKind = "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			receipt.ErrorKind = "deadline"
		case errors.Is(err, errStopped):
			receipt.ErrorKind = "stopped"
		}
	}
	return receipt
}
func validateAttemptReceipt(req attemptExecutionRequest, r attemptExecutionReceipt) error {
	output, outputErr := json.Marshal(r.Output)
	if outputErr != nil || int64(len(output)) > req.OutputLimitBytes {
		return errors.New("executor output exceeds admitted limit")
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > model.MaxOutputBytes+65536 {
		return errors.New("invalid or oversized executor receipt")
	}
	if r.Version != attemptExecutorVersion || r.Identity != req.Identity || r.RequestDigest != req.Digest {
		return errors.New("executor receipt identity mismatch")
	}
	if r.CostUSD < 0 || math.IsNaN(r.CostUSD) || math.IsInf(r.CostUSD, 0) || (r.CostUnknown && (!req.AllowUnknownCost || r.CostUSD != 0 || r.Outcome == model.OutcomeNotStarted)) {
		return errors.New("invalid executor cost evidence")
	}
	switch r.Outcome {
	case model.OutcomeSettled:
	case model.OutcomeUncertain, model.OutcomeNotStarted, "legacy":
		if r.Error == "" {
			return errors.New("executor failure outcome lacks error")
		}
	default:
		return errors.New("invalid executor outcome")
	}
	if r.OutcomeReason != executorOutcomeReason(r.OutcomeReason) || (r.OutcomeReason != "" && r.Outcome != model.OutcomeUncertain) {
		return errors.New("invalid executor outcome reason")
	}
	switch r.ErrorKind {
	case "", "cancelled", "deadline", "stopped":
	default:
		return errors.New("invalid executor error kind")
	}
	if r.Diagnostic != model.KnownDiagnosticCode(r.Diagnostic) {
		return errors.New("invalid executor diagnostic")
	}
	if r.Usage != nil && (r.Usage.InputTokens < 0 || r.Usage.CacheReadTokens < 0 || r.Usage.CacheWriteTokens < 0 || r.Usage.OutputTokens < 0 || r.Usage.ThinkingTokens < 0 || r.Usage.TotalTokens < 0) {
		return errors.New("invalid executor usage")
	}
	return nil
}
func receiptError(r attemptExecutionReceipt) error {
	if r.Error == "" {
		return nil
	}
	err := errors.New(r.Error)
	switch r.ErrorKind {
	case "cancelled":
		err = fmt.Errorf("%s: %w", r.Error, context.Canceled)
	case "deadline":
		err = fmt.Errorf("%s: %w", r.Error, context.DeadlineExceeded)
	case "stopped":
		err = fmt.Errorf("%s: %w", r.Error, errStopped)
	}
	if r.Outcome == "legacy" {
		return err
	}
	if r.Outcome == model.OutcomeUncertain {
		return &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: executorOutcomeReason(r.OutcomeReason), Err: err}
	}
	return model.WithOutcome(r.Outcome, err)
}

func (e localAttemptExecutor) execute(ctx context.Context, r attemptExecutionRequest) (attemptExecutionReceipt, error) {
	if err := validateAttemptRequest(r); err != nil {
		return executionReceipt(r, model.WorkResponse{}, model.MarkNotStarted(err)), nil
	}
	route, binding := e.provider, ""
	if e.binding != nil {
		route = e.binding.Adapter.Kind + ":" + e.binding.ProfileName
		binding = e.binding.Digest
	}
	if r.Identity.Route != route || r.Identity.BindingDigest != binding {
		return executionReceipt(r, model.WorkResponse{}, model.MarkNotStarted(errors.New("executor route differs from local adapter configuration"))), nil
	}
	req := r.Work
	prefix := filepath.Join(e.cell, strconv.Itoa(req.Attempt))
	var response model.WorkResponse
	var err error
	if e.binding != nil {
		response, err = managed.ManagedWorkWithHistory(ctx, e.cell, prefix, req, *e.binding, r.History)
	} else if e.provider == "mock" {
		delay := time.Duration(req.Task.MockDelay * float64(time.Second))
		if delay <= 0 {
			delay = time.Millisecond
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-clock.ActiveClock.After(delay):
			err = clock.WaitMockGate(ctx, req.Task.ID)
		}
		if err == nil {
			response.Output = req.Task.MockResult
			if response.Output == nil {
				response.Output = map[string]string{}
				for _, key := range req.Task.Requires {
					response.Output[key] = "mock " + key
				}
			}
		}
	} else if e.provider == "haiku" {
		response.Output, response.CostUSD, err = callHaikuBounded(ctx, e.cell, req.Attempt, req.Task, req.Dependencies, req.MaxCostUSD, req.Boundaries, req.Context)
		if !model.KnownNotStarted(err) {
			raw, _ := model.ReadProcessEvidence(prefix+".stdout.json", model.MaxOutputBytes)
			_, known := provider.HaikuReportedCost(raw)
			response.CostUnknown = !known
		}
		if err != nil {
			raw, _ := model.ReadProcessEvidence(prefix+".stdout.json", model.MaxOutputBytes)
			var reply struct {
				IsError bool   `json:"is_error"`
				Subtype string `json:"subtype"`
				Result  string `json:"result"`
			}
			if json.Unmarshal(raw, &reply) == nil && model.ProviderResultFailure(reply.IsError, reply.Subtype) {
				response.DiagnosticCode, _ = model.ClaudeResultDiagnostic(reply.Subtype, reply.Result)
			}
		}
	} else {
		response, err = External(ctx, e.cell, prefix, e.command, req)
		if response.Protocol != model.ProtocolVersion || response.RequestID != req.RequestID || model.OutcomeReason(err) == "malformed_response" || model.OutcomeReason(err) == "invalid_outcome" {
			response.DiagnosticCode = ""
		}
	}
	return executionReceipt(r, response, err), nil
}

func executeCellAttempt(ctx context.Context, s *model.State, a AdmittedCellAttempt, cell string, opts model.Options, factory attemptExecutorFactory) (attemptExecutionReceipt, error) {
	local := localAttemptExecutor{cell: cell, provider: checkpoint.ProviderName(s, a.task)}
	var history *managed.ManagedHistory
	if checkpoint.ManagedRun(s) {
		b, err := checkpoint.ManagedBinding(s, s.Tasks[a.task.ID])
		if err != nil {
			return attemptExecutionReceipt{}, model.MarkNotStarted(err)
		}
		local.binding = &b
		opts.Budget = float64(b.OperationBudgetMicros) / 1e6
		if b.Profile.TimeoutSeconds > 0 {
			opts.Timeout = time.Duration(b.Profile.TimeoutSeconds) * time.Second
		}
	} else {
		local.command = s.Plan.Providers[local.provider]
	}
	work := request(s, a.task, a.Fence.Attempt, a.inputs, opts)
	// Model guidance is not needed by mock adapters; keep their
	// request size unchanged, including profiles with a small input-byte allowance.
	if local.binding != nil && local.binding.Adapter.Kind != "mock" {
		work.Boundaries = workerBoundaries(s, a.task)
	}
	work.TrackCost = checkpoint.ManagedRun(s) && checkpoint.PlanTracksCost(s.Plan)
	if local.binding != nil {
		var err error
		history, err = managed.BuildManagedHistory(s, work, *local.binding)
		if err != nil {
			return attemptExecutionReceipt{}, model.MarkNotStarted(err)
		}
	}
	route := local.provider
	if local.binding != nil {
		route = local.binding.Adapter.Kind + ":" + local.binding.ProfileName
	}
	req := attemptExecutionRequest{Version: attemptExecutorVersion, Identity: attemptExecutionIdentity{Namespace: "local", RunID: a.Fence.RunID, TaskID: a.Fence.TaskID, OperationID: a.Fence.OperationID, Attempt: a.Fence.Attempt, RequestID: work.RequestID, BindingDigest: a.Fence.BindingDigest, Route: route}, Work: work, History: history}
	req.AllowUnknownCost = !checkpoint.ManagedRun(s) || launch.SupportsUnknownCost(s.Plan.APIVersion)
	req.OutputLimitBytes = model.MaxOutputBytes
	if local.binding != nil && local.binding.Profile.Limits.OutputBytes < req.OutputLimitBytes {
		req.OutputLimitBytes = local.binding.Profile.Limits.OutputBytes
	}
	var err error
	req.Digest, err = attemptRequestDigest(req)
	if err != nil {
		return attemptExecutionReceipt{}, model.MarkNotStarted(err)
	}
	if err = validateAttemptRequest(req); err != nil {
		return attemptExecutionReceipt{}, model.MarkNotStarted(err)
	}
	var executor attemptExecutor = local
	if factory != nil {
		executor = factory(local)
	}
	if executor == nil {
		return attemptExecutionReceipt{}, model.MarkNotStarted(errors.New("executor unavailable"))
	}
	// The controller does not guess or lock worker worktrees. Worktree creation,
	// location and lifetime belong to the worker; a controller-held lock cannot
	// prove anything about a surviving worker or its descendants.
	receipt, transportErr := executor.execute(ctx, req)
	if transportErr != nil {
		return attemptExecutionReceipt{CostUnknown: req.AllowUnknownCost, CostBasis: "unknown"}, model.WithOutcome(model.OutcomeUncertain, errors.New("executor response unavailable after handoff"))
	}
	if err = validateAttemptReceipt(req, receipt); err != nil {
		// A malformed output does not erase a matching attempt's observed charge.
		// Identity mismatches cannot attribute accounting to this attempt.
		retained := attemptExecutionReceipt{CostUnknown: req.AllowUnknownCost, CostBasis: "unknown"}
		if receipt.Version == attemptExecutorVersion && receipt.Identity == req.Identity && receipt.RequestDigest == req.Digest && receipt.CostUSD >= 0 && !math.IsNaN(receipt.CostUSD) && !math.IsInf(receipt.CostUSD, 0) {
			retained.CostUSD = receipt.CostUSD
			retained.CostUnknown = req.AllowUnknownCost && receipt.CostUnknown && receipt.CostUSD == 0 && receipt.Outcome != model.OutcomeNotStarted
			if retained.CostUnknown {
				retained.CostBasis = "unknown"
			} else {
				retained.CostBasis = "reported"
			}
			retained.Usage = receipt.Usage
			retained.Model = receipt.Model
		}
		return retained, model.WithOutcome(model.OutcomeUncertain, err)
	}
	// Compatibility error semantics (including cancellation-to-pending) apply only to
	// the built-in local adapter. Alternate executors must not authorize replay
	// using compatibility outcomes or local error-kind hints.
	if factory != nil {
		receipt.ErrorKind = ""
		if receipt.Outcome == "legacy" {
			receipt.Outcome = model.OutcomeUncertain
			if req.AllowUnknownCost && receipt.CostUSD == 0 {
				receipt.CostUnknown = true
				receipt.CostBasis = "unknown"
			}
		}
	}
	return receipt, receiptError(receipt)
}
