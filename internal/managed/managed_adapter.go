package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clock"
	costpkg "github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/providerenv"
)

// claudeModelUsageEntry is one per-model value of the claude CLI's
// `modelUsage` object (keyed by model name, camelCase fields).
type claudeModelUsageEntry struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
	ThinkingTokens           int64 `json:"thinkingTokens"`
}

// applyClaudeModelUsage fills response.Usage from the claude CLI's
// `modelUsage` object when the adapter reported no usage of its own. Token
// counts are summed across every model entry; entries that do not decode are
// skipped, and any negative count or int64 overflow makes the whole usage unavailable. Usage stays nil when no input/output/cache tokens were reported.
func applyClaudeModelUsage(response *model.WorkResponse, usage map[string]json.RawMessage, modelName string) {
	if response.Usage != nil {
		return
	}
	var sum claudeModelUsageEntry
	ok := true
	add := func(dst *int64, v int64) {
		if v > math.MaxInt64-*dst {
			ok = false
			return
		}
		*dst += v
	}
	for _, raw := range usage {
		var e claudeModelUsageEntry
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		if e.InputTokens < 0 || e.OutputTokens < 0 || e.CacheReadInputTokens < 0 || e.CacheCreationInputTokens < 0 || e.ThinkingTokens < 0 {
			return // invalid provider accounting: leave usage unavailable rather than record it
		}
		add(&sum.InputTokens, e.InputTokens)
		add(&sum.OutputTokens, e.OutputTokens)
		add(&sum.CacheReadInputTokens, e.CacheReadInputTokens)
		add(&sum.CacheCreationInputTokens, e.CacheCreationInputTokens)
		add(&sum.ThinkingTokens, e.ThinkingTokens)
	}
	tot := sum.InputTokens
	add(&tot, sum.OutputTokens)
	add(&tot, sum.CacheReadInputTokens)
	add(&tot, sum.CacheCreationInputTokens)
	if !ok || tot <= 0 {
		return // overflowing accounting is invalid: leave usage unavailable
	}
	response.Usage = &model.TaskUsage{
		InputTokens:      sum.InputTokens,
		OutputTokens:     sum.OutputTokens,
		ThinkingTokens:   sum.ThinkingTokens,
		CacheReadTokens:  sum.CacheReadInputTokens,
		CacheWriteTokens: sum.CacheCreationInputTokens,
		TotalTokens:      tot,
		Model:            modelName,
	}
}

// Managed receipts attest adapter delivery, never model comprehension or vendor identity.
func ManagedWorkWithHistory(ctx context.Context, cell, prefix string, req model.WorkRequest, b model.ExecutionBinding, history *ManagedHistory) (model.WorkResponse, error) {
	if err := launch.PrepareWorkContext(&req, cell); err != nil {
		return model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID, Outcome: model.OutcomeNotStarted, Error: err.Error()}, model.MarkNotStarted(err)
	}
	response, callErr := managedWorkCallWithHistory(ctx, cell, prefix, req, b, history)
	if response.Outcome == model.OutcomeNotStarted {
		return response, callErr
	}
	receipt := ManagedDispatchReceipt{Schema: "irrigator.managed-dispatch/v1", RunID: req.RunID, TaskID: req.Task.ID, OperationID: req.OperationID, Attempt: req.Attempt, RequestID: req.RequestID, BindingDigest: b.Digest, RequestedModel: b.Profile.Model, Outcome: response.Outcome, Evidence: "adapter_reported_delivery_not_model_comprehension"}
	if launch.ManagedHistoryEnabled(b.Profile) {
		receipt.Schema = "irrigator.managed-dispatch/v2"
		receipt.HistoryBoundary = "helper_isolation"
		if history != nil {
			receipt.HistorySHA256 = history.Digest
			receipt.HistoryBoundary = history.Boundary
		}
		input, inputErr := ManagedInput(req, b, history)
		if inputErr != nil {
			response.Outcome = model.OutcomeUncertain
			response.Output = nil
			response.Error = "managed delivery attestation failed"
			return response, model.WithOutcome(model.OutcomeUncertain, inputErr)
		}
		receipt.InputSHA256 = ManagedBytesDigest(input)
		if b.Adapter.Kind != "mock" {
			schema, _ := json.Marshal(artifactstore.ModelSchemaForRequest(req))
			receipt.OutputSchemaSHA256 = ManagedBytesDigest(schema)
		}
		if b.Adapter.Kind == "claude_cli" {
			receipt.SystemPromptSHA256 = ManagedBytesDigest([]byte(ManagedSystemPrompt))
		}
	}
	if response.Outcome == model.OutcomeSettled && !response.CostUnknown {
		cost := response.CostUSD
		receipt.ReportedCostUSD = &cost
	}
	if b.Adapter.Kind == "claude_cli" && response.Outcome == model.OutcomeSettled {
		raw, err := os.ReadFile(prefix + ".stdout.json")
		if err == nil && len(raw) <= model.MaxOutputBytes {
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) == nil {
				var model string
				if json.Unmarshal(fields["model"], &model) == nil && launch.ContextText(model, 128) {
					receipt.ReportedModel = &model
					response.Model = model
				}
				var usage map[string]json.RawMessage
				if len(fields["modelUsage"]) <= 65536 && json.Unmarshal(fields["modelUsage"], &usage) == nil && usage != nil {
					receipt.ReportedModelUsage = usage
					applyClaudeModelUsage(&response, usage, model)
				}
			}
		}
	}
	response.ContextReceipt = &model.ContextReceipt{Schema: launch.ContextReceiptSchema, Digest: req.LaunchContext.Digest, Consumer: "irrigator-managed-adapter"}
	if response.Usage != nil {
		receipt.TokenUsage = &codexTokenUsage{
			InputTokens:           response.Usage.InputTokens,
			OutputTokens:          response.Usage.OutputTokens,
			CachedInputTokens:     response.Usage.CacheReadTokens,
			CacheWriteInputTokens: response.Usage.CacheWriteTokens,
			ReasoningOutputTokens: response.Usage.ThinkingTokens,
		}
	}
	receipt.CostUnknown = response.CostUnknown
	receiptErr := launch.RecordContextReceipt(prefix, req, response.ContextReceipt)
	if receiptErr == nil {
		receiptErr = jsonio.AtomicJSON(prefix+".managed.json", receipt)
	}
	if receiptErr != nil {
		response.Outcome = model.OutcomeUncertain
		response.Error = "managed dispatch receipt persistence failed"
		response.Output = nil
		return response, model.WithOutcome(model.OutcomeUncertain, errors.New(response.Error))
	}
	return response, callErr
}

type ManagedDispatchReceipt struct {
	HistorySHA256      string                     `json:"history_sha256,omitempty"`
	HistoryBoundary    string                     `json:"history_boundary,omitempty"`
	InputSHA256        string                     `json:"input_sha256,omitempty"`
	OutputSchemaSHA256 string                     `json:"output_schema_sha256,omitempty"`
	SystemPromptSHA256 string                     `json:"system_prompt_sha256,omitempty"`
	TokenUsage         *codexTokenUsage           `json:"provider_reported_token_usage,omitempty"`
	CostUnknown        bool                       `json:"cost_unknown"`
	Schema             string                     `json:"schema"`
	RunID              string                     `json:"run_id"`
	TaskID             string                     `json:"task_id"`
	OperationID        string                     `json:"operation_id"`
	Attempt            int                        `json:"attempt"`
	RequestID          string                     `json:"request_id"`
	BindingDigest      string                     `json:"binding_digest"`
	RequestedModel     string                     `json:"requested_model"`
	ReportedModel      *string                    `json:"provider_reported_model"`
	ReportedModelUsage map[string]json.RawMessage `json:"provider_reported_model_usage,omitempty"`
	ReportedCostUSD    *float64                   `json:"reported_cost_usd"`
	Outcome            string                     `json:"outcome"`
	Evidence           string                     `json:"evidence"`
}

const ManagedSystemPrompt = "Execute only the supplied WorkRequest. Use its current run/task/operation/attempt identity and explicit context. Tools are disabled. Return the requested structured output. When turn_request is declared, return a JSON object for it; use {} to stop, never an empty or omitted value or a JSON-encoded string."

func managedWorkCallWithHistory(ctx context.Context, cell, prefix string, req model.WorkRequest, b model.ExecutionBinding, history *ManagedHistory) (model.WorkResponse, error) {
	response := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID}
	fail := func(certainty string, err error) (model.WorkResponse, error) {
		response.Error = err.Error()
		response.Outcome = certainty
		return response, model.ClaudeOutcomeError(certainty, err)
	}
	if err := checkpoint.ValidateManagedBinding(b); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	if req.Protocol != model.ProtocolVersion || req.Kind != "task" || req.RunID == "" || req.OperationID == "" || req.RequestID == "" || req.Attempt < 1 || req.TimeoutMS <= 0 || req.TimeoutMS > 3600000 {
		return fail(model.OutcomeNotStarted, errors.New("invalid managed request identity"))
	}
	if b.Profile.TimeoutSeconds > 0 && req.TimeoutMS != int64(b.Profile.TimeoutSeconds)*1000 {
		return fail(model.OutcomeNotStarted, errors.New("request timeout conflicts with managed profile"))
	}
	if req.Task.Effort != "" && req.Task.Effort != b.Profile.Effort {
		return fail(model.OutcomeNotStarted, errors.New("task effort conflicts with managed profile"))
	}
	if req.MaxCostUSD != float64(b.OperationBudgetMicros)/1e6 {
		return fail(model.OutcomeNotStarted, errors.New("request budget conflicts with managed profile"))
	}
	if err := ctx.Err(); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
	defer cancel()
	if err := launch.ValidateWorkerLaunchContext(req); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	input, err := ManagedInput(req, b, history)
	if err != nil || int64(len(input)) > b.Profile.Limits.InputBytes {
		return fail(model.OutcomeNotStarted, errors.New("managed request exceeds profile input limit"))
	}
	if b.Adapter.Kind == "mock" {
		if err := os.WriteFile(prefix+".input.json", input, 0600); err != nil {
			return fail(model.OutcomeNotStarted, err)
		}
		delay := time.Duration(req.Task.MockDelay * float64(time.Second))
		if delay <= 0 {
			delay = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return fail(model.OutcomeNotStarted, ctx.Err())
		case <-clock.ActiveClock.After(delay):
		}
		response.Output = req.Task.MockResult
		if response.Output == nil {
			response.Output = map[string]string{}
			for _, key := range req.Task.Requires {
				response.Output[key] = "mock " + key
			}
		}
		raw, _ := json.Marshal(response.Output)
		if int64(len(raw)) > b.Profile.Limits.OutputBytes {
			return fail(model.OutcomeSettled, errors.New("mock output exceeds profile limit"))
		}
		response.Outcome = model.OutcomeSettled
		return response, nil
	}
	if b.Adapter.Kind == "ollama_http" {
		return managedOllamaWork(ctx, cell, prefix, req, b, input)
	}
	if b.Adapter.Kind == "codex_cli" {
		return managedCodexWork(ctx, cell, prefix, req, b, input)
	}
	// Recheck the opened regular executable bytes immediately before invocation.
	// This is not an atomic exec-by-descriptor guarantee against same-user changes.
	c := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{b.AdapterName: b.Adapter}, Instances: map[string]model.ExecutionInstance{b.InstanceName: b.Instance}, Profiles: map[string]model.ExecutionProfile{b.ProfileName: b.Profile}, DefaultProfile: b.ProfileName}
	if _, err = launch.ResolveExecutionCatalog(c, "managed-recheck", b.ProfileName); err != nil {
		return fail(model.OutcomeNotStarted, err)
	}
	schema, _ := json.Marshal(artifactstore.ModelSchemaForRequest(req))
	const systemPrompt = ManagedSystemPrompt
	if int64(len(input)+len(schema)+len(systemPrompt)) > b.Profile.Limits.InputBytes {
		return fail(model.OutcomeNotStarted, errors.New("managed input plus schema/system prompt exceeds profile input limit"))
	}
	cmd := exec.Command(b.Adapter.Command[0], "-p", "--model", b.Profile.Model, "--effort", b.Profile.Effort, "--safe-mode", "--tools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--disable-slash-commands", "--no-session-persistence", "--output-format", "json", "--json-schema", string(schema), "--max-budget-usd", strconv.FormatFloat(req.MaxCostUSD, 'f', -1, 64), "--system-prompt", systemPrompt)
	cmd.Dir = cell
	cmd.Env = providerenv.Claude()
	raw, processErr := launch.RunProcess(ctx, cmd, prefix, input)
	if processErr != nil {
		if model.KnownNotStarted(processErr) {
			return fail(model.OutcomeNotStarted, processErr)
		}
		if errors.Is(processErr, context.Canceled) || errors.Is(processErr, context.DeadlineExceeded) || !model.ProviderProcessStopped(prefix) {
			return fail(model.OutcomeUncertain, processErr)
		}
		// A nonzero exit may still carry a complete billed error result. Parse it
		// before deciding certainty; a successful-looking result cannot override exit failure.
	}
	if int64(len(raw)) > b.Profile.Limits.OutputBytes {
		return fail(model.OutcomeUncertain, errors.New("managed output exceeds profile limit"))
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, parseErr := jsonio.ParseStrictJSON(decoder, 0)
	if parseErr != nil || !utf8.Valid(raw) || decoder.Decode(new(any)) != io.EOF {
		return fail(model.OutcomeUncertain, errors.New("invalid managed Claude JSON"))
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fail(model.OutcomeUncertain, errors.New("managed Claude result must be an object"))
	}
	if value, present := object["is_error"]; present {
		if _, ok := value.(bool); !ok {
			return fail(model.OutcomeUncertain, errors.New("managed Claude is_error must be boolean"))
		}
	}
	for key := range object {
		for _, canonical := range []string{"type", "structured_output", "is_error", "total_cost_usd"} {
			if strings.EqualFold(key, canonical) && key != canonical {
				return fail(model.OutcomeUncertain, errors.New("managed Claude result has noncanonical field alias"))
			}
		}
	}
	var reply struct {
		Type       string          `json:"type"`
		Structured json.RawMessage `json:"structured_output"`
		IsError    bool            `json:"is_error"`
		Subtype    string          `json:"subtype"`
		Result     string          `json:"result"`
	}
	if err = json.Unmarshal(raw, &reply); err == nil && model.ProviderResultFailure(reply.IsError, reply.Subtype) {
		response.DiagnosticCode, _ = model.ClaudeResultDiagnostic(reply.Subtype, reply.Result)
	}
	if err != nil || reply.Type != "result" {
		return fail(model.OutcomeUncertain, errors.New("managed Claude requires a complete result"))
	}
	cost, costKnown := FiniteReportedCost(object["total_cost_usd"])
	if !costKnown && !req.TrackCost {
		return fail(model.OutcomeUncertain, errors.New("managed Claude result requires finite nonnegative reported cost"))
	}
	response.CostUnknown = !costKnown
	if costKnown {
		response.CostUSD = cost
	}
	if processErr != nil && !model.ProviderResultFailure(reply.IsError, reply.Subtype) {
		return fail(model.OutcomeUncertain, processErr)
	}
	if costpkg.CostExceeds(response.CostUSD, req.MaxCostUSD) {
		response.DiagnosticCode = model.DiagProviderOperationBudget
		return fail(model.OutcomeSettled, errors.New("managed provider exceeded operation reservation"))
	}
	if model.ProviderResultFailure(reply.IsError, reply.Subtype) {
		var message string
		response.DiagnosticCode, message = model.ClaudeResultDiagnostic(reply.Subtype, reply.Result)
		return fail(model.OutcomeSettled, errors.New(message))
	}
	if len(reply.Structured) == 0 || string(reply.Structured) == "null" {
		return fail(model.OutcomeSettled, errors.New("managed Claude omitted structured output"))
	}
	response.Output, err = artifactstore.NormalizeModelOutput(req.Task, reply.Structured)
	if err != nil {
		return fail(model.OutcomeSettled, fmt.Errorf("managed output contract: %w", err))
	}
	response.Outcome = model.OutcomeSettled
	return response, nil
}
