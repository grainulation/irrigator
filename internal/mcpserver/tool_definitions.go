package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Canonical names are an explicit preview, not aliases added to the legacy
// inventory. A host selects exactly one naming contract at startup.
var canonicalToolNames = map[string]string{
	"list_runs": "list_runs", "run_status": "get_run_status", "run_logs": "list_run_events",
	"attempt_output": "get_attempt_output", "provider_limit": "get_attempt_limit_advisory",
	"delivery_summary": "get_delivery_summary", "inspect_cell": "inspect_cell", "review_list": "list_reviews",
	"inspect_review": "inspect_review", "validate_plan": "validate_plan", "apply": "start_run", "pause": "pause_run", "resume": "resume_run",
	"retry_cell": "retry_cell", "decide_approval": "decide_approval",
	"cancel_cell": "cancel_cell", "stop_run": "stop_run", "assemble_delivery": "assemble_delivery", "check_delivery": "check_delivery",
}

func mcpToolName(cfg McpConfig, name string) string {
	if cfg.canonicalNames {
		return canonicalToolNames[name]
	}
	return name
}

func addMCPTool[In any](s *mcp.Server, cfg McpConfig, name string, handler mcp.ToolHandlerFor[In, mcpOut]) {
	description := mcpToolDescriptions[name]
	for legacy := range canonicalToolNames {
		description = strings.ReplaceAll(description, "{{"+legacy+"}}", mcpToolName(cfg, legacy))
	}
	readOnly, destructive, idempotent, openWorld := true, false, true, false
	switch name {
	case "apply", "resume":
		readOnly, destructive, idempotent, openWorld = false, true, false, true
	case "pause":
		readOnly, destructive, idempotent = false, true, false
	case "retry_cell", "decide_approval":
		readOnly, destructive, idempotent = false, true, false
	case "cancel_cell", "stop_run", "assemble_delivery":
		readOnly, destructive, idempotent = false, true, true
	case "check_delivery":
		readOnly, destructive, idempotent, openWorld = false, true, true, true
	}
	outputSchema := namedMCPOutputSchema(cfg, name)
	mcp.AddTool(s, &mcp.Tool{
		Name: mcpToolName(cfg, name), Description: description,
		InputSchema: namedMCPInputSchema(cfg, name), OutputSchema: outputSchema,
		Annotations: toolAnnotations(readOnly, destructive, idempotent, openWorld),
	}, exactMCPOutput(handler, outputSchema))
}

// The SDK's typed output path decodes object results through float64 while
// applying schema defaults. Validate a native-number copy against the same
// published schema, then supply the precise JSON ourselves. Returning nil as the
// typed output keeps the SDK's input/error handling without its lossy rewrite.
func exactMCPOutput[In any](handler mcp.ToolHandlerFor[In, mcpOut], schema map[string]any) mcp.ToolHandlerFor[In, any] {
	var definition jsonschema.Schema
	raw, err := json.Marshal(schema)
	if err == nil {
		err = json.Unmarshal(raw, &definition)
	}
	if err != nil {
		panic(fmt.Errorf("MCP output schema: %w", err))
	}
	resolved, err := definition.Resolve(nil)
	if err != nil {
		panic(fmt.Errorf("MCP output schema: %w", err))
	}
	return func(ctx context.Context, request *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		result, out, err := handler(ctx, request, in)
		if err != nil || (result != nil && (result.IsError || result.InputRequests != nil)) {
			return result, nil, err
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling tool output: %w", err)
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, fmt.Errorf("decoding tool output: %w", err)
		}
		if err := resolved.ApplyDefaults(&value); err != nil {
			return nil, nil, fmt.Errorf("applying output defaults: %w", err)
		}
		validation, err := mcpSchemaValue(value)
		if err != nil {
			return nil, nil, fmt.Errorf("validating tool output: %w", err)
		}
		if err := resolved.Validate(validation); err != nil {
			return nil, nil, fmt.Errorf("validating tool output: %w", err)
		}
		raw, err = json.Marshal(value)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling validated output: %w", err)
		}
		if result == nil {
			result = &mcp.CallToolResult{}
		}
		result.StructuredContent = json.RawMessage(raw)
		if result.Content == nil {
			result.Content = []mcp.Content{&mcp.TextContent{Text: string(raw)}}
		}
		return result, nil, nil
	}
}

// jsonschema-go uses Go numeric kinds for its type checks. Build a separate
// validation value with native numbers; keep the original json.Number values
// for serialization. In particular, all int64/uint64 counters validate exactly.
func mcpSchemaValue(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		if number, err := value.Int64(); err == nil {
			return number, nil
		}
		if number, err := strconv.ParseUint(string(value), 10, 64); err == nil {
			return number, nil
		}
		number, err := value.Float64()
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(number)
		if err != nil {
			return nil, err
		}
		original, originalOK := new(big.Rat).SetString(string(value))
		converted, convertedOK := new(big.Rat).SetString(string(encoded))
		if !originalOK || !convertedOK || original.Cmp(converted) != 0 {
			return nil, errors.New("number cannot be validated without rounding")
		}
		return number, nil
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			converted, err := mcpSchemaValue(item)
			if err != nil {
				return nil, err
			}
			out[key] = converted
		}
		return out, nil
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			converted, err := mcpSchemaValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	default:
		return value, nil
	}
}

var mcpToolDescriptions = map[string]string{
	"cancel_cell":       "Request cooperative cancellation of one inspected cell operation and its descendants, leaving unrelated cells running. Use when a single cell is wrong or stuck; use {{pause}} to drain the whole run or {{stop_run}} to stop it. Requires the exact run/plan/graph identity from {{run_status}} and expected_operation_id from {{inspect_cell}}. Verified results remain immutable; running or uncertain work retains its uncertainty. request_id is a durable deduplication key: retry the same arguments after a lost response; different arguments with that ID are rejected. This records intent, never claims process cleanup or retries work.",
	"stop_run":          "Stop admission and request running workers to stop by recording down control. Use when work must end now; use {{pause}} to let active calls finish or {{cancel_cell}} to stop one cell. Requires the run/plan/graph identity from {{run_status}} and a durable request_id. Repeating the same request does not add events or stop a later graph generation; changed arguments require a new ID. Completed runs stay complete. Acknowledgment records intent, not confirmed provider cleanup; no persisted PID is killed.",
	"assemble_delivery": "Materialize file maps from an already completed and fully verified run into an explicit local destination. Use after {{run_status}} or {{delivery_summary}} shows the graph complete and verified. Requires the run/plan/graph identity from {{run_status}}, selected nodes (cell IDs from {{run_status}}) and a request_id. Refuses incomplete graphs without starting workers. Merges disjoint maps with path/conflict/provenance checks, preserving existing acceptance evidence. Returns the candidate digest needed by {{check_delivery}}. No model call or acceptance command is run.",
	"check_delivery":    "Run the explicitly supplied local checker argv against an already assembled candidate, fenced by its digest and completed run/plan/graph identity. Use after {{assemble_delivery}} returns a candidate digest; read recorded outcomes with {{delivery_summary}}. The command can have external effects. Checker intent is journaled before execution. Retry identical arguments and request_id to retrieve the durable outcome without re-execution; interrupted intent returns unknown. A new attempt after acceptance, failure or interruption requires a new request_id and explicit recheck. Cleanup uncertainty blocks recheck. Never starts graph work or submits a delivery externally.",
	"list_runs":         "Discover saved runs before opening one with {{run_status}}. Scans configured --runs roots in order and their immediate subdirectories by name; returns all discovered rows without pagination. Without --runs the list is empty; a known run path can still be inspected directly. Rows report saved control state, executor, progress and known USD cost. Unreadable roots or checkpoints produce error rows. Cost is a recorded subtotal; unknown charges remain possible.",
	"run_status":        "Read a saved checkpoint overview to locate cells needing attention. Includes every cell's status and attempt count, run control state, executor, graph revision and cost accounting; error text is capped at 1 KB. This is saved state, not a live provider-health check, and activity logs may advance during the read. Use {{inspect_cell}} for a cell's specification and result, or {{run_logs}} for transition history. Reuse the returned path to inspect the same run across discovery roots. Missing or unreadable runs are tool errors.",
	"run_logs":          "Trace how a run reached its current state using saved orchestration events in ascending sequence order. Use {{run_status}} for the current overview or {{attempt_output}} for provider stdout/stderr. Continue by passing next_since as since. A cell filter consumes skipped events too: more means unread events remain, possibly only for other cells, so the final filtered page can be empty. Without a cell filter, all events after since are eligible. Unknown cell IDs and unreadable runs are tool errors.",
	"inspect_cell":      "Inspect a cell's saved specification, current state, recorded result, provider and descendants after {{run_status}} identifies a problem. Pass the saved cell ID; the returned cell field is its directory. replay_safe reports the declared replay policy, not evidence that a previous external call had no effects or permission to repeat it. Use {{attempt_output}} for historical stdout/stderr and {{retry_cell}} to prepare an explicitly authorized retry when controls are enabled. Inspection changes no run state. Strings are capped at 8 KB with truncated paths disclosed; missing runs or cells are tool errors.",
	"attempt_output":    "Read one exact historical task attempt's bounded, redacted stdout/stderr. Use for process output; {{run_logs}} reads orchestration events and {{provider_limit}} interprets captured quota/reset evidence. Pass the returned next_cursor as cursor only for the same run, task, attempt, stream and source snapshot; a changed source requires restarting without cursor. has_more means unread pages, while source_truncated and capture_complete may be null when capture evidence is absent. Missing captures or invalid cursors are tool errors; this cannot read arbitrary files or retry work.",
	"provider_limit":    "Interpret quota and reset evidence from one attempt's saved stdout JSON and stderr log. Use after {{attempt_output}} reveals a possible provider limit; {{run_status}} reports accounting instead. Reads local capture files only, without contacting a provider or authenticating an account. Match node and attempt to the output being investigated; evidence from another attempt does not clear this one. Missing or conflicting reset estimates stay unknown, and missing evidence does not establish available quota. The advisory never retries work or authorizes dispatch; invalid tasks or attempts are tool errors.",
	"delivery_summary":  "Inspect a saved run's delivery selection, candidate conflicts and recorded check outcomes. Use it to assess selected run outputs; {{review_list}} and {{inspect_review}} cover workspace branches and documents. Any saved run can be inspected before a delivery selection exists. candidate_sha256 is null when no tasks are selected or conflicts/issues prevent a candidate. Use './r' for a workspace-relative run even with --runs configured. This reads evidence without assembling files, rerunning checks, applying changes or granting approval; unreadable runs are tool errors.",
	"inspect_review":    "Read one queued branch's diff pinned to review.merge_base/review.head, or a queued document's body and comments. Repeat the workspace used for {{review_list}}. For a branch, pass queue.changes[].branch with queue.base; for a document, pass queue.documents[].snapshot and omit branch/base/file. Each call resolves branch/base again, so inspect a moved head before relying on prior review evidence. file narrows branch diffs; base/file are invalid for documents. Strings are capped at 8 KB with truncated paths disclosed. For a full branch patch, use local git diff --no-ext-diff --no-textconv with the returned merge_base and head IDs. Missing/stale documents or invalid revisions are tool errors; reading records no review decision.",
	"review_list":       "Discover a workspace's branch and document review queue before using {{inspect_review}}. Branches ahead of base come from Git; documents and prior verdicts come from saved review records. Reuse this workspace and the returned queue.base when inspecting a branch. Use {{delivery_summary}} for one run's selected outputs. Storage is an additional pruning preview; storage.error reports an unavailable preview without hiding the queue. Local irrigator review commands record verdicts and perform pruning. Listing changes no review decisions or Git branches. Invalid Git workspaces or bases are tool errors.",
	"validate_plan":     "Check a plan's format and runtime contract before {{apply}} when controls are enabled, or before local CLI execution. Supply exactly one path or inline JSON string. Validation loads the plan and checks its declared graph and constraints; it does not execute providers, run task verification commands or establish their availability or cost. Invalid or unreadable plan content returns valid:false with errors; missing/both inputs or path '-' are tool errors. Relative paths use --workspace; inline JSON uses a private temporary file removed afterward.",
	"apply":             "Create a new run from a plan and start its controller detached; returns run path, backend, executor and started:true. Use only for a new run; {{resume}} starts an existing stopped run. Non-managed plans require an explicit backend and positive per-call budget; managed plans use profile budgets and reject budget here. Providers may contact external services and incur charges. Existing runs are never overwritten. If creation succeeds but launch fails, the tool error identifies the created run to inspect before resuming.",
	"pause":             "Request a run to stop admitting work and let active calls finish. Returns after saving the request; control=pause records intent, not controller exit. Watch cell progress with {{run_status}} while calls drain, then use {{resume}} to continue; its controller check is a preflight. To end work immediately use {{stop_run}}, or {{cancel_cell}} for one cell. Drain time depends on active calls. For provider termination use local irrigator down RUN, then inspect outcomes and confirm process cleanup before retrying uncertain work. Every pause call appends an event. Missing or quarantined runs are tool errors.",
	"resume":            "Start a detached controller for an existing stopped run; started:true acknowledges process launch, not controller ownership or progress. Check {{run_status}} and {{run_logs}} after launch. Use after {{pause}} has drained or {{retry_cell}} has prepared work; {{apply}} creates a new run. retry_failed defaults to false and only retries replay-safe failures when true; uncertain external effects are never replayed automatically. A live controller detected during preflight, a complete/quarantined run or a launch failure is a tool error. Providers may contact external services and incur charges.",
	"retry_cell":        "Prepare eligible unverified cells for retry without starting workers. Use only after the controller and provider processes have stopped; then {{resume}} executes pending work. Required allow_uncertain and confirm_stopped attestations have no defaults. Resetting eligible cells clears their current result/error/validation while preserving historical attempts; accepted verified cells are skipped. Returns retried, waiting and skipped IDs. Unsafe replay, live workers or unconfirmed cleanup are tool errors; authorization to duplicate uncertain external work must be explicit.",
	"decide_approval":   "Record a single-use approve/deny decision for one blocked run request through its approval ledger/mailbox. Use only for that workflow decision; {{review_list}} is workspace review inventory and this tool never approves or merges a Git branch. Denial can prevent pending work, and decisions cannot be overwritten or safely replayed. Returns run, request and decision; unknown/stale requests or consumed decisions are tool errors. No workers are started by this call.",
}

func schemaField(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

func schemaObject(description string, properties map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "description": description, "properties": properties}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func stringArray(description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": schemaField("string", "Recorded value.")}
}

func nullableField(kind, description string) map[string]any {
	return map[string]any{"type": []string{kind, "null"}, "description": description}
}

func mcpInputSchema(name string) map[string]any {
	run := schemaField("string", "Saved run directory path, not run_id. Absolute paths are accepted; paths such as ./r resolve under --workspace. Bare names search --runs roots in order and select the first matching directory; an inspection error stops lookup, and a missing name with roots configured is an error rather than a workspace fallback. Without roots, bare names use --workspace. --runs does not confine paths.")
	props := map[string]any{"run": run}
	required := []string{"run"}
	switch name {
	case "cancel_cell", "stop_run", "assemble_delivery", "check_delivery":
		props["request_id"] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$", "description": "Durable caller-selected request identity. Retries must repeat identical arguments; never recycle this ID."}
		props["expected_run_id"] = schemaField("string", "Inspected durable run_id from run status; prevents targeting another run at the same path.")
		props["expected_plan_sha256"] = map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$", "description": "Inspected immutable plan_sha256 from run status."}
		props["expected_graph_revision"] = map[string]any{"type": "integer", "minimum": 0, "description": "Exact inspected graph_revision, including explicit zero for the original graph."}
		required = append(required, "request_id", "expected_run_id", "expected_plan_sha256", "expected_graph_revision")
		switch name {
		case "cancel_cell":
			props["cell"] = schemaField("string", "Exact inspected cell ID; descendants are included.")
			props["expected_operation_id"] = schemaField("string", "Exact node.operation_id returned by inspect_cell, preventing cancellation of a later retry.")
			required = append(required, "cell", "expected_operation_id")
		case "assemble_delivery":
			props["directory"] = schemaField("string", "Explicit local destination outside the durable run; relative paths resolve under --workspace. Its parent must exist.")
			props["nodes"] = map[string]any{"type": "array", "minItems": 1, "uniqueItems": true, "items": schemaField("string", "Verified cell ID whose file map is selected."), "description": "Nonempty unique selected cell IDs."}
			props["field"] = schemaField("string", "Result field containing a JSON file map, commonly files.")
			required = append(required, "directory", "nodes", "field")
		case "check_delivery":
			props["expected_candidate_sha256"] = map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$", "description": "Exact candidate_sha256 returned by assemble_delivery or delivery summary."}
			props["check"] = map[string]any{"type": "array", "minItems": 1, "items": schemaField("string", "Literal executable argv element; no implicit shell."), "description": "Explicit checker argv. It runs inside the candidate directory and may have external effects."}
			props["timeout_seconds"] = map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 86400, "description": "Explicit positive checker deadline, at most 86400 seconds."}
			props["recheck"] = schemaField("boolean", "Explicitly authorize a new checker attempt after an earlier acceptance, failure or interruption. Requires a new request_id; never overrides uncertain cleanup.")
			required = append(required, "expected_candidate_sha256", "check", "timeout_seconds")
		}
	case "list_runs":
		props, required = map[string]any{}, nil
	case "run_logs":
		props["since"] = map[string]any{"type": "integer", "minimum": 0, "description": "Last event sequence already consumed; zero starts at the beginning."}
		props["limit"] = map[string]any{"type": "integer", "minimum": 0, "description": "Requested page size; zero/omitted uses 100, values above 500 are clamped."}
		props["cell"] = schemaField("string", "Optional saved task/cell ID from {{run_status}} cells[].id; unknown nonempty IDs are rejected.")
	case "inspect_cell", "retry_cell":
		props["cell"] = schemaField("string", "Saved task/cell ID from {{run_status}} cells[].id, not the directory returned as {{inspect_cell}} cell.")
		required = append(required, "cell")
		if name == "retry_cell" {
			props["recursive"] = schemaField("boolean", "Include descendants; defaults to false. Verified cells remain accepted.")
			props["allow_uncertain"] = schemaField("boolean", "Explicitly permit duplicate external effects for uncertain work; false retains replay protection.")
			props["confirm_stopped"] = schemaField("boolean", "Attest surviving provider/validator processes were stopped; never inferred from a free lease.")
			required = append(required, "allow_uncertain", "confirm_stopped")
		}
	case "attempt_output", "provider_limit":
		id := "task"
		if name == "provider_limit" {
			id = "node"
		}
		props[id] = schemaField("string", "Saved task/cell ID from {{run_status}} cells[].id; use the same ID for {{inspect_cell}} cell, {{attempt_output}} task and {{provider_limit}} node. This is not a filename or directory.")
		props["attempt"] = map[string]any{"type": "integer", "minimum": 1, "description": "Recorded historical attempt number for this task, starting at one. The current attempt count does not guarantee that stdout/stderr was captured for every earlier attempt."}
		required = append(required, id, "attempt")
		if name == "attempt_output" {
			props["stream"] = map[string]any{"type": "string", "enum": []string{"", "stdout", "stderr"}, "description": "Captured stream; omitted or empty selects stdout."}
			props["cursor"] = schemaField("string", "Opaque next_cursor from this exact source snapshot; omit for first page.")
			props["max_bytes"] = map[string]any{"type": "integer", "anyOf": []any{map[string]any{"const": 0}, map[string]any{"minimum": 4, "maximum": 8192}}, "description": "Presentation bytes per page, 4..8192; zero/omitted defaults to 4096."}
		}
	case "inspect_review":
		props = map[string]any{"workspace": schemaField("string", "Local Git workspace; defaults to --workspace."), "branch": schemaField("string", "Branch from {{review_list}} queue.changes[].branch; supply this or snapshot."), "snapshot": schemaField("string", "Document ID from {{review_list}} queue.documents[].snapshot; no arbitrary paths."), "base": schemaField("string", "Branch comparison base; pass {{review_list}} queue.base to reuse its base reference. Omitted uses remote default, then local main/master. Invalid for documents."), "file": schemaField("string", "Optional changed path to narrow the branch diff; invalid for documents.")}
		required = nil
	case "review_list":
		props = map[string]any{"workspace": schemaField("string", "Local Git workspace; defaults to --workspace, relative paths resolve there."), "base": schemaField("string", "Branch/commit to compare against; omitted uses remote default, then main/master.")}
		required = nil
	case "validate_plan":
		props = map[string]any{"path": schemaField("string", "Plan file path relative to --workspace or absolute; '-' is unsupported. Supply this or json, never both."), "json": schemaField("string", "Inline plan JSON instead of path; validation uses a temporary local file.")}
		required = nil
	case "apply":
		props["plan"] = schemaField("string", "Plan file path, relative to --workspace or absolute; stdin '-' is unsupported.")
		props["backend"] = schemaField("string", "Explicit mock, haiku or named provider for non-managed plans; managed plans require managed or omission.")
		props["budget"] = schemaField("number", "Positive per-call USD limit, required for non-managed plans; prohibited for managed plans. Not an aggregate invoice cap.")
		props["workers"] = schemaField("integer", "Concurrency 1..128; zero/omitted selects 2.")
		props["timeout_seconds"] = schemaField("number", "Per-call timeout in seconds; zero/omitted selects 120; positive values must not exceed the runtime maximum.")
		props["schedule"] = schemaField("string", "Optional scheduling order: breadth or depth; omitted uses the runtime default.")
		props["executor"] = schemaField("string", "Local executor name: local, herdr or tmux; omitted uses local. Requires availability on this host.")
		props["run"] = schemaField("string", "New directory, never overwritten; a bare name is created under the first --runs root when configured.")
		required = append(required, "plan")
	case "resume":
		props["retry_failed"] = schemaField("boolean", "Retry replay-safe failed cells; defaults to false and never permits uncertain replay.")
	case "decide_approval":
		props["request"] = schemaField("string", "Exact blocked request's cell ID in this run.")
		props["decision"] = map[string]any{"type": "string", "enum": []string{"approve", "deny"}, "description": "Single-use workflow decision; cannot overwrite an earlier decision."}
		required = append(required, "request", "decision")
	}
	out := schemaObject("Arguments for "+name+".", props, required...)
	if name == "inspect_review" {
		props["branch"].(map[string]any)["minLength"] = 1
		props["snapshot"].(map[string]any)["pattern"] = "^[0-9a-f]{64}$"
		out["oneOf"] = []any{
			map[string]any{"required": []string{"branch"}, "not": map[string]any{"required": []string{"snapshot"}}},
			map[string]any{"required": []string{"snapshot"}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"branch"}}, map[string]any{"required": []string{"base"}}, map[string]any{"required": []string{"file"}}}}},
		}
	}
	if name == "validate_plan" {
		props["path"].(map[string]any)["minLength"] = 1
		props["path"].(map[string]any)["not"] = map[string]any{"const": "-"}
		props["json"].(map[string]any)["minLength"] = 1
		out["oneOf"] = []any{
			map[string]any{"required": []string{"path"}, "not": map[string]any{"required": []string{"json"}}},
			map[string]any{"required": []string{"json"}, "not": map[string]any{"required": []string{"path"}}},
		}
	}
	out["additionalProperties"] = false
	return out
}

func mcpOutputSchema(name string) map[string]any {
	text := func(description string) any { return schemaField("string", description) }
	number := func(description string) any { return schemaField("integer", description) }
	object := func(description string) any { return schemaField("object", description) }
	array := func(description string) any {
		return map[string]any{"type": "array", "description": description, "items": map[string]any{}}
	}
	props := map[string]any{}
	required := []string{}
	switch name {
	case "cancel_cell", "stop_run":
		props = map[string]any{"run": text("Resolved run directory."), "run_id": text("Durable run identity bound to the request."), "request_id": text("Durable deduplication identity."), "requested": text("cancel_cell or stop_run."), "cell": text("Cancellation target; empty for stop_run."), "changed": schemaField("boolean", "Whether the original request changed control intent; replay returns the original value."), "cleanup_confirmed": schemaField("boolean", "Always false: acknowledgment does not establish provider cleanup."), "dispatched": schemaField("boolean", "Always false: no new work is launched.")}
		required = []string{"run", "run_id", "request_id", "requested", "cell", "changed", "cleanup_confirmed", "dispatched"}
	case "assemble_delivery", "check_delivery":
		props = map[string]any{"error": text("Recorded checker failure detail when available; replay returns the original request detail."), "run_id": text("Durable run identity."), "request_id": text("Durable deduplication identity."), "candidate_sha256": text("Exact file-map candidate identity bound to the request."), "status": text("exported, accepted, check_failed or unknown; checking intent without a durable outcome is unknown on replay."), "check_started": schemaField("boolean", "A checker attempt was durably admitted and may have started; this is not proof of external effects or cleanup."), "replayed": schemaField("boolean", "True when returning a prior receipt without repeating effects."), "dispatched": schemaField("boolean", "Always false: graph work and model providers are never started.")}
		required = []string{"run_id", "request_id", "candidate_sha256", "status", "check_started", "replayed", "dispatched"}
	case "list_runs":
		props["runs"] = array("Successful run rows contain name/path/control/executor/verified/total/cost_usd. Diagnostic rows contain error and representable path/name when available; invalid UTF-8 selectors are omitted. cost_usd is only known reported USD.")
		required = []string{"runs"}
	case "run_status":
		props = map[string]any{"run_id": text("Durable run identity used by fenced control and delivery requests."), "plan_sha256": text("Immutable admitted plan digest used by fenced requests."), "path": text("Resolved run directory."), "backend": text("Saved provider backend."), "control": text("Current durable run control state."), "executor": text("Recorded local executor."), "graph_revision": number("Current admitted graph revision."), "verified": number("Count of accepted verified cells."), "total": number("Total cells."), "cost": object("Reported cost projection; unknown-cost operations remain explicitly unknown."), "cells": array("Per-cell id/status/attempts plus optional capped error, cost_unknown, cost_basis, usage, model and last_activity. Use id as {{inspect_cell}} cell, {{attempt_output}} task or {{provider_limit}} node; attempts is not a capture inventory.")}
		required = []string{"path", "backend", "control", "executor", "graph_revision", "verified", "total", "cost", "cells"}
	case "run_logs":
		props = map[string]any{"events": array("Each row contains seq (one-based checkpoint event sequence) and event (recorded orchestration event)."), "next_since": number("Sequence consumed including skipped filter entries; pass as since."), "total": number("Total saved events before filtering."), "more": schemaField("boolean", "Unconsumed checkpoint events remain, possibly outside the cell filter.")}
		required = []string{"events", "next_since", "total", "more"}
	case "inspect_cell":
		props = map[string]any{"node": object("Saved TaskState object including spec, status, attempts and result; this is not the string ID accepted by {{provider_limit}} node. Secret fields are redacted."), "provider": text("Resolved saved provider name."), "replay_safe": schemaField("boolean", "Declared replay policy; not proof an uncertain external call had no effects."), "cell": text("Local directory for the requested cell; a path, not the ID accepted by cell/task/node selectors."), "descendants": stringArray("Descendant cell IDs, excluding the requested cell."), "last_activity": object("Latest saved attempt activity when a captured activity log is available.")}
		required = []string{"node", "provider", "replay_safe", "cell", "descendants"}
	case "attempt_output":
		for _, key := range []string{"schema", "run_id", "task", "stream", "representation", "source_sha256", "capture_metadata", "output", "next_cursor"} {
			props[key] = text(map[string]string{"schema": "Attempt-output format identifier.", "run_id": "Saved run identity.", "task": "Exact task ID.", "stream": "Selected stdout or stderr.", "representation": "Redacted terminal-safe text.", "source_sha256": "Digest of exact captured source bytes before presentation.", "capture_metadata": "Capture evidence state: verified, unrecorded, invalid or mismatch.", "output": "This presentation page; pagination is separate from capture loss.", "next_cursor": "Present only when has_more; bound to this source snapshot."}[key])
		}
		for _, key := range []string{"attempt", "source_bytes", "offset", "next_offset", "total_bytes"} {
			props[key] = number(map[string]string{"attempt": "Exact historical attempt.", "source_bytes": "Raw source bytes.", "offset": "Page start in presentation bytes.", "next_offset": "Page end in presentation bytes.", "total_bytes": "Full redacted presentation byte length."}[key])
		}
		props["source_truncated"] = nullableField("boolean", "Whether capture lost bytes; null means not established, not false.")
		props["capture_complete"] = nullableField("boolean", "Whether capture completed; null means no reliable capture evidence.")
		props["has_more"] = schemaField("boolean", "More presentation pages remain in the exact snapshot.")
		required = []string{"schema", "run_id", "task", "attempt", "stream", "representation", "source_sha256", "source_bytes", "source_truncated", "capture_complete", "capture_metadata", "offset", "next_offset", "total_bytes", "has_more", "output"}
	case "provider_limit":
		props = map[string]any{"schema": text("Provider-limit advisory format."), "run_id": text("Saved run ID."), "task": text("Exact task ID."), "attempt": number("Exact attempt."), "status": text("Advisory evidence classification; not account authentication or dispatch permission."), "reset_at": nullableField("string", "Estimated reset timestamp or null when unknown/conflicting."), "observed_at": nullableField("string", "Source observation timestamp or null."), "reset_basis": text("Basis of estimate, including unknown or conflicting_estimates."), "sources": stringArray("Captured artifact names inspected."), "read_errors": stringArray("Unavailable sources; missing evidence is not a cleared limit."), "binding_digest": text("Saved provider binding when known."), "diagnostic_code": text("Saved budget diagnostic when present."), "advisory_only": schemaField("boolean", "Always true; this cannot authorize work."), "automatic_retry": schemaField("boolean", "Always false; no work was replayed.")}
		required = []string{"schema", "run_id", "task", "attempt", "status", "reset_at", "observed_at", "reset_basis", "sources", "read_errors", "advisory_only", "automatic_retry"}
	case "delivery_summary":
		props = map[string]any{"schema": text("Delivery-summary format."), "run_id": text("Saved run ID."), "plan_sha256": text("Admitted plan hash."), "graph_revision": number("Saved graph revision."), "delivery": object("Recorded delivery selection view."), "tasks": array("Task and recorded check outcomes; unknown checks remain unknown."), "included": stringArray("Included task IDs."), "excluded": stringArray("Excluded task IDs."), "conflicts": array("Output conflict rows with path, tasks and kind."), "files": number("Candidate file count."), "candidate_sha256": nullableField("string", "Candidate identity or null when unavailable."), "acceptance": text("Recorded acceptance assessment; no checks run by this call."), "issues": stringArray("Issues preventing an unambiguous candidate."), "dispatched": schemaField("boolean", "False; no workers started by inspection.")}
		required = []string{"schema", "run_id", "plan_sha256", "graph_revision", "delivery", "tasks", "included", "excluded", "conflicts", "files", "candidate_sha256", "acceptance", "issues", "dispatched"}
	case "inspect_review":
		props = map[string]any{"kind": text("branch or document."), "review": object("Branch metadata, exact head, changed files and comments; or queued document ID/kind/title/body/comments."), "diff": text("Branch diff at the returned head, absent for documents; inspect truncated before relying on it.")}
		required = []string{"kind", "review"}
	case "review_list":
		props = map[string]any{"queue": object("Workspace review queue. Repeat the selected workspace in {{inspect_review}}. For branches pass changes[].branch with base; for documents pass documents[].snapshot and omit branch/base/file."), "storage": object("Preview of eligible storage pruning; nothing removed.")}
		required = []string{"queue", "storage"}
	case "validate_plan":
		props = map[string]any{"valid": schemaField("boolean", "Whether the plan contract validates."), "errors": stringArray("Validation errors; invalid content is a normal result."), "name": text("Plan name, present only for valid plans."), "api_version": text("Plan format, present only for valid plans."), "tasks": number("Initial task count, present only for valid plans.")}
		required = []string{"valid", "errors"}
	case "apply":
		props = map[string]any{"run": text("Created run directory."), "backend": text("Admitted backend."), "executor": text("Recorded local executor."), "started": schemaField("boolean", "True only after controller process launch succeeds.")}
		required = []string{"run", "backend", "executor", "started"}
	case "pause":
		props = map[string]any{"run": text("Resolved run directory."), "requested": text("pause; this records a drain request, not process termination.")}
		required = []string{"run", "requested"}
	case "resume":
		props = map[string]any{"run": text("Resolved run directory."), "started": schemaField("boolean", "True only after launching the detached controller.")}
		required = []string{"run", "started"}
	case "retry_cell":
		props = map[string]any{"retried": stringArray("Cells reset to pending."), "waiting": stringArray("Cells waiting for dependencies."), "skipped": stringArray("Cells left unchanged, including accepted verified cells.")}
		required = []string{"retried", "waiting", "skipped"}
	case "decide_approval":
		props = map[string]any{"run": text("Resolved run directory."), "request": text("Blocked request cell ID."), "decision": text("Recorded approve or deny decision.")}
		required = []string{"run", "request", "decision"}
	}
	props["truncated"] = stringArray("Exact JSON paths of shortened display values. These addresses are not shortened, even when they exceed the display-string cap. This is presentation truncation, not capture-loss evidence.")
	return schemaObject("Sanitized structured result. Display strings are capped at 8192 bytes including any truncation notice; exact addresses in truncated are exempt. Credential fields are redacted. Tool errors use isError:true text instead of this success result.", props, required...)
}

func namedMCPInputSchema(cfg McpConfig, name string) map[string]any {
	out := mcpInputSchema(name)
	out["description"] = "Arguments for " + mcpToolName(cfg, name) + "."
	renderMCPSchemaReferences(cfg, out)
	return out
}

func namedMCPOutputSchema(cfg McpConfig, name string) map[string]any {
	out := mcpOutputSchema(name)
	renderMCPSchemaReferences(cfg, out)
	return out
}

// References can occur in nested properties and array items as well as the
// top-level description. Only description text is rendered; schema values and
// externally supplied strings are not renamed.
func renderMCPSchemaReferences(cfg McpConfig, value any) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "description" {
				if description, ok := child.(string); ok {
					for legacy := range canonicalToolNames {
						description = strings.ReplaceAll(description, "{{"+legacy+"}}", mcpToolName(cfg, legacy))
					}
					value[key] = description
				}
			} else {
				renderMCPSchemaReferences(cfg, child)
			}
		}
	case []any:
		for _, child := range value {
			renderMCPSchemaReferences(cfg, child)
		}
	}
}
