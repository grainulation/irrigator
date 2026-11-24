package mcpserver

import (
	"strings"

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
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: mcpToolName(cfg, name), Description: description,
		InputSchema: namedMCPInputSchema(cfg, name), OutputSchema: mcpOutputSchema(name),
		Annotations: toolAnnotations(readOnly, destructive, idempotent, openWorld),
	}, handler)
}

var mcpToolDescriptions = map[string]string{
	"list_runs":        "List saved runs under configured --runs directories, with path, control state, executor, verified/total cells and known reported USD cost. Use to discover run names, then {{run_status}} for one run; without --runs the list is empty. Unreadable roots or checkpoints appear as error rows. Cost is a recorded subtotal, not proof of free execution.",
	"run_status":       "Read one run's current checkpoint overview: control state, executor, graph revision, cost accounting and per-cell status, attempt count and errors (capped at 1 KB). Use to locate cells needing attention; {{inspect_cell}} returns one cell's specification and results, and {{run_logs}} returns transition events. Missing or unreadable runs are tool errors; this does not start or repair work.",
	"run_logs":         "Read a page of saved orchestration events, not provider stdout/stderr. Use for state transitions; {{attempt_output}} reads one attempt's captured output. since is the last consumed sequence; pass next_since to continue, while more reports unread checkpoint events. limit defaults to 100 and is clamped to 500. An unknown cell filter or unreadable run is a tool error.",
	"inspect_cell":     "Read one exact cell's saved specification, current state, accepted result, provider, replay safety and descendant IDs. Use after {{run_status}} identifies the cell; {{attempt_output}} reads historical stdout/stderr instead. Large strings are capped at 8 KB and listed under truncated; missing runs or cells are tool errors. Inspection never retries uncertain work.",
	"attempt_output":   "Read one exact historical task attempt's bounded, redacted stdout/stderr. Use for process output; {{run_logs}} reads orchestration events. Pass next_cursor only for the same run, task, attempt, stream and source snapshot; a changed source requires restarting without cursor. has_more means unread pages, while source_truncated and capture_complete may be null when capture evidence is absent. Missing captures or invalid cursors are tool errors; this cannot read arbitrary files or retry work.",
	"provider_limit":   "Read provider quota/reset evidence captured for one exact node attempt. Use after {{attempt_output}} shows a possible provider limit; {{run_status}} reports run accounting. Returns an advisory status, nullable reset_at and observed_at, evidence sources and read_errors. Unknown or conflicting reset evidence stays unknown. This never authenticates an account, queries live quota, retries work or authorizes dispatch; invalid attempts are tool errors.",
	"delivery_summary": "Read the saved delivery candidate, included/excluded task IDs, output conflicts and recorded check outcomes. Use to inspect recorded delivery evidence; {{review_list}} lists workspace review items. candidate_sha256 may be null when no candidate can be identified. This never assembles files, runs checks, applies changes or grants approval; an unreadable run is a tool error.",
	"inspect_review":   "Read one queued branch review with its pinned-head diff, or one queued document with its body and comments. Use {{review_list}} to obtain a branch name or snapshot ID; supply exactly one of branch or snapshot. file narrows a branch diff; base/file are invalid for documents. Strings are capped at 8 KB with truncated paths disclosed; use the local CLI for a full oversized diff. Missing/stale documents or invalid revisions are tool errors. This never captures files, marks them viewed, records a verdict or merges.",
	"review_list":      "List branches and documents awaiting review in a local Git workspace, plus a preview of storage eligible for pruning. Use {{inspect_review}} to read a listed branch or document; {{delivery_summary}} describes one saved run's candidate. workspace defaults to --workspace; base defaults to the remote default branch, then main/master. Invalid Git workspaces or bases are tool errors. No pruning, review decisions or merges are performed.",
	"validate_plan":    "Validate exactly one plan file path or inline JSON string, returning valid and errors plus identity/task count when valid. Use before {{apply}} when control is enabled; validation does not start work or estimate provider charges. Invalid plan content is a normal valid:false result; missing/both inputs or path '-' are tool errors. Relative paths use --workspace; inline JSON uses a temporary local file that is removed afterward.",
	"apply":            "Create a new run from a plan and start its controller detached; returns run path, backend, executor and started:true. Use only for a new run; {{resume}} starts an existing stopped run. Non-managed plans require an explicit backend and positive per-call budget; managed plans use profile budgets and reject budget here. Providers may contact external services and incur charges. Existing runs are never overwritten. If creation succeeds but launch fails, the tool error identifies the created run to inspect before resuming.",
	"pause":            "Request a run to drain while retaining saved work. Use to stop admitting work; this does not hard-cancel an active provider or prove it stopped. {{resume}} restarts a stopped controller. Every call appends a pause request event, so repeated calls are not idempotent. Returns requested:pause; missing or quarantined runs are tool errors.",
	"resume":           "Start a detached controller for an existing stopped run; returns run and started:true. Use after {{pause}} has drained or {{retry_cell}} has prepared work; {{apply}} creates a new run. retry_failed defaults to false and only retries replay-safe failures when true; uncertain external effects are never replayed automatically. A live controller, complete/quarantined run or launch failure is a tool error. Providers may contact external services and incur charges.",
	"retry_cell":       "Prepare failed/cancelled cells for retry without starting workers. Use only after the controller and provider processes have stopped; then {{resume}} executes pending work. Required allow_uncertain and confirm_stopped attestations have no defaults. Resetting eligible cells clears their current result/error/validation while preserving historical attempts; accepted verified cells are skipped. Returns retried, waiting and skipped IDs. Unsafe replay, live workers or unconfirmed cleanup are tool errors; authorization to duplicate uncertain external work must be explicit.",
	"decide_approval":  "Record a single-use approve/deny decision for one blocked run request through its approval ledger/mailbox. Use only for that workflow decision; {{review_list}} is workspace review inventory and this tool never approves or merges a Git branch. Denial can prevent pending work, and decisions cannot be overwritten or safely replayed. Returns run, request and decision; unknown/stale requests or consumed decisions are tool errors. No workers are started by this call.",
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
	run := schemaField("string", "Saved run directory path, or a bare name under a configured --runs directory; paths are not confined by --runs.")
	props := map[string]any{"run": run}
	required := []string{"run"}
	switch name {
	case "list_runs":
		props, required = map[string]any{}, nil
	case "run_logs":
		props["since"] = map[string]any{"type": "integer", "minimum": 0, "description": "Last event sequence already consumed; zero starts at the beginning."}
		props["limit"] = map[string]any{"type": "integer", "minimum": 0, "description": "Requested page size; zero/omitted uses 100, values above 500 are clamped."}
		props["cell"] = schemaField("string", "Optional exact cell ID; unknown nonempty IDs are rejected.")
	case "inspect_cell", "retry_cell":
		props["cell"] = schemaField("string", "Exact saved cell (node) ID.")
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
		props[id] = schemaField("string", "Exact saved task ID, not a filename or arbitrary path.")
		props["attempt"] = map[string]any{"type": "integer", "minimum": 1, "description": "Explicit recorded historical attempt number, starting at one."}
		required = append(required, id, "attempt")
		if name == "attempt_output" {
			props["stream"] = map[string]any{"type": "string", "enum": []string{"", "stdout", "stderr"}, "description": "Captured stream; omitted or empty selects stdout."}
			props["cursor"] = schemaField("string", "Opaque next_cursor from this exact source snapshot; omit for first page.")
			props["max_bytes"] = map[string]any{"type": "integer", "anyOf": []any{map[string]any{"const": 0}, map[string]any{"minimum": 4, "maximum": 8192}}, "description": "Presentation bytes per page, 4..8192; zero/omitted defaults to 4096."}
		}
	case "inspect_review":
		props = map[string]any{"workspace": schemaField("string", "Local Git workspace; defaults to --workspace."), "branch": schemaField("string", "Queued local branch name from review_list; supply this or snapshot."), "snapshot": schemaField("string", "Queued document snapshot ID from review_list; no arbitrary paths."), "base": schemaField("string", "Optional base branch/commit for branch inspection."), "file": schemaField("string", "Optional changed path to narrow the branch diff; invalid for documents.")}
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
	for _, value := range props {
		if field, ok := value.(map[string]any); ok {
			if description, ok := field["description"].(string); ok {
				field["description"] = strings.ReplaceAll(description, "review_list", "{{review_list}}")
			}
		}
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
	case "list_runs":
		props["runs"] = array("Discovered run rows with name/path/control/executor/verified/total/cost_usd, or path/name/error for unreadable entries. cost_usd is only known reported USD.")
		required = []string{"runs"}
	case "run_status":
		props = map[string]any{"path": text("Resolved run directory."), "backend": text("Saved provider backend."), "control": text("Current durable run control state."), "executor": text("Recorded local executor."), "graph_revision": number("Current admitted graph revision."), "verified": number("Count of accepted verified cells."), "total": number("Total cells."), "cost": object("Reported cost projection; unknown-cost operations remain explicitly unknown."), "cells": array("Per-cell id/status/attempts plus optional capped error, cost_unknown, cost_basis, usage, model and last_activity.")}
		required = []string{"path", "backend", "control", "executor", "graph_revision", "verified", "total", "cost", "cells"}
	case "run_logs":
		props = map[string]any{"events": array("Each row contains seq (one-based checkpoint event sequence) and event (recorded orchestration event)."), "next_since": number("Sequence consumed including skipped filter entries; pass as since."), "total": number("Total saved events before filtering."), "more": schemaField("boolean", "Unconsumed checkpoint events remain, possibly outside the cell filter.")}
		required = []string{"events", "next_since", "total", "more"}
	case "inspect_cell":
		props = map[string]any{"node": object("Saved TaskState including spec, status, attempts and result; secret fields are redacted."), "provider": text("Resolved saved provider name."), "replay_safe": schemaField("boolean", "Declared replay policy; not proof an uncertain external call had no effects."), "cell": text("Local cell directory."), "descendants": stringArray("Descendant cell IDs, excluding the requested cell."), "last_activity": object("Latest saved attempt activity when a captured activity log is available.")}
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
		props = map[string]any{"queue": object("Workspace review queue with base, changes and documents."), "storage": object("Preview of eligible storage pruning; nothing removed.")}
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
	props["truncated"] = stringArray("JSON paths of capped strings; strings end with a truncation notice. This is presentation truncation, not capture-loss evidence.")
	return schemaObject("Sanitized structured result; strings are capped at 8 KB with a suffix and credential fields are redacted. Tool errors use isError:true text instead of this success result.", props, required...)
}

func namedMCPInputSchema(cfg McpConfig, name string) map[string]any {
	out := mcpInputSchema(name)
	out["description"] = "Arguments for " + mcpToolName(cfg, name) + "."
	props := out["properties"].(map[string]any)
	for _, value := range props {
		if field, ok := value.(map[string]any); ok {
			if description, ok := field["description"].(string); ok {
				for legacy := range canonicalToolNames {
					description = strings.ReplaceAll(description, "{{"+legacy+"}}", mcpToolName(cfg, legacy))
				}
				field["description"] = description
			}
		}
	}
	return out
}
