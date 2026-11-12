package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/approvals"
	"github.com/grainulation/irrigator/internal/branchreview"
	"github.com/grainulation/irrigator/internal/buildinfo"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/dashboard"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runops"
	"github.com/grainulation/irrigator/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	McpTextCap       = 8 * 1024
	McpErrorCap      = 1024
	mcpDefaultEvents = 100
	mcpMaxEvents     = 500
)

type McpConfig struct {
	RunRoots     []string
	Workspace    string
	AllowControl bool
}

type mcpPathList []string

func (l *mcpPathList) String() string     { return strings.Join(*l, ",") }
func (l *mcpPathList) Set(v string) error { *l = append(*l, v); return nil }

func McpCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	var roots mcpPathList
	fs.Var(&roots, "runs", "directory whose subdirectories are runs (repeatable)")
	workspace := fs.String("workspace", ".", "Git repository for review_list and base for relative plan paths")
	allowControl := fs.Bool("allow-control", false, "also register tools that change runs: apply, pause, resume, retry_cell, decide_approval")
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, errors.New("usage: irrigator mcp [--runs DIR]... [--workspace DIR] [--allow-control]")
	}
	cfg, err := newMCPConfig(roots, *workspace, *allowControl)
	if err != nil {
		return 1, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// stdout carries the protocol; diagnostics go to stderr only.
	if err := NewMCPServer(cfg).Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		return 1, err
	}
	return 0, nil
}

func newMCPConfig(roots []string, workspace string, allowControl bool) (McpConfig, error) {
	cfg := McpConfig{AllowControl: allowControl}
	for _, r := range roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return cfg, err
		}
		cfg.RunRoots = append(cfg.RunRoots, abs)
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return cfg, err
	}
	cfg.Workspace = abs
	return cfg, nil
}

func NewMCPServer(cfg McpConfig) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "irrigator", Version: buildinfo.Version}, &mcp.ServerOptions{
		Instructions: "Inspect Irrigator runs. Run arguments are a run directory path or, with --runs, a run name. Interrupted work is never replayed automatically; retry_cell (when enabled) requires explicit allow_uncertain and confirm_stopped.",
	})
	t := &mcpTools{cfg: cfg}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "list_runs", Description: "List runs under the configured --runs directories: name, path, control state, verified/total cells and reported cost.", Annotations: readOnly}, t.listRuns)
	mcp.AddTool(s, &mcp.Tool{Name: "run_status", Description: "Status of one run: per-cell status, attempts and (truncated) errors.", Annotations: readOnly}, t.runStatus)
	mcp.AddTool(s, &mcp.Tool{Name: "run_logs", Description: "Sequenced run events, as `irrigator logs --json`. Pass the returned next_since to continue.", Annotations: readOnly}, t.runLogs)
	mcp.AddTool(s, &mcp.Tool{Name: "inspect_cell", Description: "One cell's spec, attempts and result fields; large values are truncated with a note.", Annotations: readOnly}, t.inspectCell)
	mcp.AddTool(s, &mcp.Tool{Name: "review_list", Description: "Branches and documents awaiting review for a workspace, as `irrigator review list`.", Annotations: readOnly}, t.reviewList)
	mcp.AddTool(s, &mcp.Tool{Name: "validate_plan", Description: "Validate a plan file (path) or inline plan JSON (json) and return the errors.", Annotations: readOnly}, t.validatePlan)
	if !cfg.AllowControl {
		return s
	}
	f := false
	write := &mcp.ToolAnnotations{DestructiveHint: &f}
	mcp.AddTool(s, &mcp.Tool{Name: "apply", Description: "Create a run from a plan and start its controller detached, like `irrigator apply`. Returns the run path. A per-call budget is required (not for managed plans).", Annotations: write}, t.apply)
	mcp.AddTool(s, &mcp.Tool{Name: "pause", Description: "Ask a run to drain; durable state is retained.", Annotations: write}, t.pause)
	mcp.AddTool(s, &mcp.Tool{Name: "resume", Description: "Start a detached controller for a stopped run, like `irrigator resume`.", Annotations: write}, t.resume)
	mcp.AddTool(s, &mcp.Tool{Name: "retry_cell", Description: "Prepare an explicit retry of failed or cancelled cells, like `irrigator retry`. allow_uncertain and confirm_stopped are required and keep their CLI meaning; uncertain outcomes are never retried unless both are attested.", Annotations: write}, t.retryCell)
	mcp.AddTool(s, &mcp.Tool{Name: "decide_approval", Description: "Approve or deny a blocked request through the run's approval ledger/mailbox.", Annotations: write}, t.decideApproval)
	return s
}

type mcpTools struct {
	cfg McpConfig
	// applyMu serializes apply so two calls cannot race to create one run.
	applyMu sync.Mutex
}

type mcpOut = map[string]any

// mcpResult sanitizes the structured output and pairs it with a short summary.
func mcpResult(summary string, out mcpOut) (*mcp.CallToolResult, mcpOut, error) {
	clean, truncated := mcpSanitize(out)
	m, _ := clean.(map[string]any)
	if m == nil {
		m = mcpOut{}
	}
	if len(truncated) > 0 {
		m["truncated"] = truncated
		summary += fmt.Sprintf(" (%d value(s) truncated to %d bytes; see \"truncated\")", len(truncated), McpTextCap)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: summary}}}, m, nil
}

var mcpSecretKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|credential|authorization)`)

// mcpSanitize round-trips v through JSON, caps every string at mcpTextCap, and
// never lets environment values or credential-named fields through: "env"
// lists keep only variable names.
func mcpSanitize(v any) (any, []string) {
	raw, err := json.Marshal(v)
	if err != nil {
		return mcpOut{"error": "output could not be encoded"}, nil
	}
	var generic any
	_ = json.Unmarshal(raw, &generic)
	var truncated []string
	var walk func(path string, v any) any
	walk = func(path string, v any) any {
		switch x := v.(type) {
		case string:
			s, cut := mcpTruncate(x, McpTextCap)
			if cut {
				truncated = append(truncated, path)
			}
			return s
		case []any:
			for i := range x {
				x[i] = walk(path+"["+strconv.Itoa(i)+"]", x[i])
			}
			return x
		case map[string]any:
			for k, val := range x {
				child := path + "." + k
				switch {
				case strings.EqualFold(k, "env"):
					x[k] = mcpEnvNames(val)
				case mcpSecretKey.MatchString(k):
					if _, isString := val.(string); isString {
						x[k] = "[redacted]"
					} else {
						x[k] = walk(child, val)
					}
				default:
					x[k] = walk(child, val)
				}
			}
			return x
		}
		return v
	}
	return walk("$", generic), truncated
}

func mcpEnvNames(v any) any {
	switch x := v.(type) {
	case []any:
		names := make([]any, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				names = append(names, strings.SplitN(s, "=", 2)[0])
			}
		}
		return names
	case map[string]any:
		names := make([]any, 0, len(x))
		for k := range x {
			names = append(names, k)
		}
		return names
	}
	return "[redacted]"
}

// mcpTruncate cuts s to at most limit bytes on a rune boundary.
func mcpTruncate(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… [truncated: %d of %d bytes shown]", cut, len(s)), true
}

func (t *mcpTools) resolvePath(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(t.cfg.Workspace, p)
}

// resolveRun accepts a run directory path, or a bare name found under a --runs root.
func (t *mcpTools) resolveRun(ref string) (string, error) {
	if ref == "" || strings.ContainsRune(ref, 0) {
		return "", errors.New("run is required: a run directory path or a run name under --runs")
	}
	if !strings.ContainsRune(ref, filepath.Separator) && ref != "." && ref != ".." {
		for _, root := range t.cfg.RunRoots {
			candidate := filepath.Join(root, ref)
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate, nil
			}
		}
		if len(t.cfg.RunRoots) > 0 {
			return "", fmt.Errorf("no run named %q under the configured --runs directories", ref)
		}
	}
	return t.resolvePath(ref), nil
}

// openRun resolves ref and reads its checkpoint.
func (t *mcpTools) openRun(ref string) (string, *model.State, error) {
	root, err := t.resolveRun(ref)
	if err != nil {
		return "", nil, err
	}
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return "", nil, fmt.Errorf("cannot read run %s: %w", filepath.Base(root), err)
	}
	return root, s, nil
}

func mcpVerified(s *model.State) (verified, total int) {
	for _, row := range s.Tasks {
		if row.Status == "verified" {
			verified++
		}
	}
	return verified, len(s.Tasks)
}

type mcpNoInput struct{}

func (t *mcpTools) listRuns(_ context.Context, _ *mcp.CallToolRequest, _ mcpNoInput) (*mcp.CallToolResult, mcpOut, error) {
	runs := []mcpOut{}
	for _, root := range t.cfg.RunRoots {
		entries, err := os.ReadDir(root)
		if err != nil {
			runs = append(runs, mcpOut{"path": root, "error": err.Error()})
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(path, "state.json")); err != nil {
				continue
			}
			s, err := checkpoint.ReadState(path)
			if err != nil {
				msg, _ := mcpTruncate(err.Error(), McpErrorCap)
				runs = append(runs, mcpOut{"name": e.Name(), "path": path, "error": msg})
				continue
			}
			verified, total := mcpVerified(s)
			runs = append(runs, mcpOut{"name": e.Name(), "path": path, "control": s.Control, "executor": dashboard.RunExecutor(path), "verified": verified, "total": total, "cost_usd": provider.ProjectCost(s).KnownReportedUSD})
		}
	}
	note := ""
	if len(t.cfg.RunRoots) == 0 {
		note = "; no --runs directories configured, pass a run path to the other tools"
	}
	return mcpResult(fmt.Sprintf("%d run(s)%s", len(runs), note), mcpOut{"runs": runs})
}

type mcpRunInput struct {
	Run string `json:"run" jsonschema:"run directory path, or a run name under a --runs directory"`
}

func (t *mcpTools) runStatus(_ context.Context, _ *mcp.CallToolRequest, in mcpRunInput) (*mcp.CallToolResult, mcpOut, error) {
	root, s, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	cells := []mcpOut{}
	for _, spec := range s.AllTasks() {
		row := s.Tasks[spec.ID]
		cell := mcpOut{"id": spec.ID, "status": row.Status, "attempts": row.Attempt}
		if row.Error != "" {
			cell["error"], _ = mcpTruncate(row.Error, McpErrorCap)
		}
		if row.CostUnknown {
			cell["cost_unknown"] = true
		}
		if row.CostBasis != "" {
			cell["cost_basis"] = row.CostBasis
		}
		if row.Usage != nil {
			cell["usage"] = row.Usage
		}
		if row.Model != "" {
			cell["model"] = row.Model
		}
		cells = append(cells, cell)
	}
	verified, total := mcpVerified(s)
	cost := provider.ProjectCost(s)
	summary := fmt.Sprintf("%s | %s | %s | %s | %d/%d verified | %s", filepath.Base(root), s.Backend, dashboard.RunExecutor(root), s.Control, verified, total, provider.CostStatusText(s))
	return mcpResult(summary, mcpOut{"path": root, "backend": s.Backend, "control": s.Control, "executor": dashboard.RunExecutor(root), "graph_revision": s.GraphRevision, "verified": verified, "total": total, "cost": cost, "cells": cells})
}

type mcpLogsInput struct {
	Run   string `json:"run" jsonschema:"run directory path or run name"`
	Since int    `json:"since,omitempty" jsonschema:"last event sequence already consumed (default 0)"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum events to return (default 100, at most 500)"`
	Cell  string `json:"cell,omitempty" jsonschema:"only events for this cell"`
}

func (t *mcpTools) runLogs(_ context.Context, _ *mcp.CallToolRequest, in mcpLogsInput) (*mcp.CallToolResult, mcpOut, error) {
	if in.Since < 0 || in.Limit < 0 {
		return nil, nil, errors.New("since and limit must not be negative")
	}
	limit := in.Limit
	if limit == 0 {
		limit = mcpDefaultEvents
	}
	limit = min(limit, mcpMaxEvents)
	_, s, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	if in.Cell != "" && s.Tasks[in.Cell] == nil {
		return nil, nil, errors.New("unknown cell filter")
	}
	events := []mcpOut{}
	next := in.Since
	for i, e := range s.Events {
		seq := i + 1
		if seq <= in.Since {
			continue
		}
		if len(events) >= limit {
			break
		}
		next = seq
		if in.Cell != "" && e.Task != in.Cell {
			continue
		}
		events = append(events, mcpOut{"seq": seq, "event": e})
	}
	more := next < len(s.Events)
	return mcpResult(fmt.Sprintf("%d event(s); next_since %d; %d total", len(events), next, len(s.Events)), mcpOut{"events": events, "next_since": next, "total": len(s.Events), "more": more})
}

type mcpInspectInput struct {
	Run  string `json:"run" jsonschema:"run directory path or run name"`
	Cell string `json:"cell" jsonschema:"cell (node) id"`
}

func (t *mcpTools) inspectCell(_ context.Context, _ *mcp.CallToolRequest, in mcpInspectInput) (*mcp.CallToolResult, mcpOut, error) {
	root, err := t.resolveRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	if in.Cell == "" {
		return nil, nil, errors.New("cell is required")
	}
	v, err := runops.InspectNode(root, in.Cell)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot inspect cell %q: %w", in.Cell, err)
	}
	status := ""
	if row, ok := v["node"].(*model.TaskState); ok {
		status = row.Status
	}
	return mcpResult(fmt.Sprintf("cell %s: %s", in.Cell, status), v)
}

type mcpReviewInput struct {
	Workspace string `json:"workspace,omitempty" jsonschema:"Git repository to review (default: the server --workspace)"`
	Base      string `json:"base,omitempty" jsonschema:"branch or commit to compare against (default: the remote default branch, then main or master)"`
}

func (t *mcpTools) reviewList(_ context.Context, _ *mcp.CallToolRequest, in mcpReviewInput) (*mcp.CallToolResult, mcpOut, error) {
	dir := t.cfg.Workspace
	if in.Workspace != "" {
		dir = t.resolvePath(in.Workspace)
	}
	ws, err := session.ChangeWorkspace(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace: %w", err)
	}
	q, err := branchreview.ListReviews(ws, in.Base)
	if err != nil {
		return nil, nil, err
	}
	report, err := branchreview.PruneReviews(ws, 30, false)
	if err != nil {
		return nil, nil, err
	}
	return mcpResult(fmt.Sprintf("%d change(s), %d document(s) against %s", len(q.Changes), len(q.Documents), q.Base), mcpOut{"queue": q, "storage": report})
}

type mcpValidateInput struct {
	Path string `json:"path,omitempty" jsonschema:"plan file to validate"`
	JSON string `json:"json,omitempty" jsonschema:"inline plan JSON to validate (instead of path)"`
}

func (t *mcpTools) validatePlan(_ context.Context, _ *mcp.CallToolRequest, in mcpValidateInput) (*mcp.CallToolResult, mcpOut, error) {
	if (in.Path == "") == (in.JSON == "") {
		return nil, nil, errors.New("provide exactly one of path or json")
	}
	path := ""
	if in.JSON != "" {
		dir, err := os.MkdirTemp("", "irrigator-mcp-plan-")
		if err != nil {
			return nil, nil, err
		}
		defer os.RemoveAll(dir)
		path = filepath.Join(dir, "plan.json")
		if err := os.WriteFile(path, []byte(in.JSON), 0o600); err != nil {
			return nil, nil, err
		}
	} else {
		if in.Path == "-" {
			return nil, nil, errors.New("path - (stdin) is not available over MCP")
		}
		path = t.resolvePath(in.Path)
	}
	p, err := checkpoint.LoadPlan(path)
	if err != nil {
		msg := strings.ReplaceAll(err.Error(), path, "plan")
		return mcpResult("plan is invalid: "+firstLine(msg), mcpOut{"valid": false, "errors": []string{msg}})
	}
	return mcpResult(fmt.Sprintf("plan is valid: %d task(s)", len(p.Tasks)), mcpOut{"valid": true, "errors": []string{}, "name": p.Name, "api_version": p.APIVersion, "tasks": len(p.Tasks)})
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	out, _ := mcpTruncate(line, 200)
	return out
}

// McpStartDetached runs `irrigator resume RUN ...` in its own session so the
// controller outlives this server. Arguments never carry credentials; the
// controller inherits the server's environment exactly as a shell would.
var McpStartDetached = func(args []string) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// mcpControllerIdle fails when a controller already holds the run.
func mcpControllerIdle(root string) error {
	lock, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("a controller is already running")
		}
		return err
	}
	checkpoint.Unlock(lock)
	return nil
}

type mcpApplyInput struct {
	Plan           string   `json:"plan" jsonschema:"plan file path"`
	Run            string   `json:"run" jsonschema:"new run directory (absolute, or a bare name created under the first --runs directory)"`
	Backend        string   `json:"backend,omitempty" jsonschema:"mock, haiku (paid) or a named provider; required for non-managed plans"`
	Budget         *float64 `json:"budget,omitempty" jsonschema:"per-call budget in USD; required except for managed plans, which use profile budgets"`
	Workers        int      `json:"workers,omitempty" jsonschema:"maximum concurrent workers (default 2)"`
	TimeoutSeconds float64  `json:"timeout_seconds,omitempty" jsonschema:"timeout per call in seconds (default 120)"`
	Schedule       string   `json:"schedule,omitempty" jsonschema:"opt-in ready order: breadth or depth"`
	Executor       string   `json:"executor,omitempty" jsonschema:"worker executor: local (default), herdr or tmux; recorded for the run"`
}

func (t *mcpTools) apply(_ context.Context, _ *mcp.CallToolRequest, in mcpApplyInput) (*mcp.CallToolResult, mcpOut, error) {
	if in.Plan == "" || in.Plan == "-" {
		return nil, nil, errors.New("plan must be a file path")
	}
	if in.Run == "" {
		return nil, nil, errors.New("run is required")
	}
	workers, timeout := 2, 120*time.Second
	if in.Workers != 0 {
		workers = in.Workers
	}
	if in.TimeoutSeconds != 0 {
		timeout = time.Duration(in.TimeoutSeconds * float64(time.Second))
	}
	if workers < 1 || workers > 128 || timeout <= 0 || timeout > executor.MaxCallTimeout {
		return nil, nil, fmt.Errorf("invalid limits: workers 1..128, timeout above 0 and at most %s", executor.MaxCallTimeout)
	}
	if in.Budget != nil && !model.ValidCallBudget(*in.Budget) {
		return nil, nil, errors.New("budget must be a positive USD amount")
	}
	if _, err := runops.NormalizeSchedulingOrder(in.Schedule); err != nil {
		return nil, nil, err
	}
	if in.Executor != "" {
		if err := executor.ValidateExecutorName(in.Executor, "executor"); err != nil {
			return nil, nil, err
		}
	}
	p, err := checkpoint.LoadPlan(t.resolvePath(in.Plan))
	if err != nil {
		return nil, nil, err
	}
	backend, budget := in.Backend, 0.25
	if launch.IsManagedPlanVersion(p.APIVersion) {
		if in.Budget != nil {
			return nil, nil, errors.New("managed plans use profile operation budgets; budget is unsupported")
		}
		if backend != "" && backend != "managed" {
			return nil, nil, errors.New("managed plans require backend managed")
		}
		backend = "managed"
	} else {
		if in.Budget == nil {
			return nil, nil, errors.New("budget is required: set an explicit per-call budget in USD")
		}
		budget = *in.Budget
		if backend == "" {
			return nil, nil, errors.New("backend is required: use mock for a free run, or explicitly select haiku or a named provider")
		}
	}
	root := in.Run
	if !filepath.IsAbs(root) {
		if strings.ContainsRune(root, filepath.Separator) || len(t.cfg.RunRoots) == 0 {
			root = t.resolvePath(root)
		} else {
			root = filepath.Join(t.cfg.RunRoots[0], root)
		}
	}
	root = filepath.Clean(root)
	t.applyMu.Lock()
	defer t.applyMu.Unlock()
	if err := checkpoint.Initialize(p, root, backend, model.Options{Workers: workers, Timeout: timeout, Budget: budget}); err != nil {
		return nil, nil, err
	}
	if err := executor.RecordExecutorChoice(root, in.Executor); err != nil {
		return nil, nil, fmt.Errorf("run created at %s but the executor could not be recorded (%v); use resume", root, err)
	}
	args := []string{"resume", root, "--workers", strconv.Itoa(workers), "--timeout", timeout.String()}
	if in.Executor != "" {
		args = append(args, "--executor", in.Executor)
	}
	if !launch.IsManagedPlanVersion(p.APIVersion) {
		args = append(args, "--call-budget", strconv.FormatFloat(budget, 'f', -1, 64))
	}
	if in.Schedule != "" {
		args = append(args, "--schedule", in.Schedule)
	}
	if err := McpStartDetached(args); err != nil {
		return nil, nil, fmt.Errorf("run created at %s but the controller did not start (%v); use resume", root, err)
	}
	return mcpResult("run created and started: "+root, mcpOut{"run": root, "backend": backend, "executor": dashboard.RunExecutor(root), "started": true})
}

func (t *mcpTools) pause(_ context.Context, _ *mcp.CallToolRequest, in mcpRunInput) (*mcp.CallToolResult, mcpOut, error) {
	root, _, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return nil, nil, err
	}
	if err := checkpoint.SetControl(root, "pause"); err != nil {
		return nil, nil, err
	}
	return mcpResult("pause requested; durable state retained", mcpOut{"run": root, "requested": "pause"})
}

type mcpResumeInput struct {
	Run         string `json:"run" jsonschema:"run directory path or run name"`
	RetryFailed bool   `json:"retry_failed,omitempty" jsonschema:"retry replay-safe failed nodes (uncertain work is never replayed)"`
}

func (t *mcpTools) resume(_ context.Context, _ *mcp.CallToolRequest, in mcpResumeInput) (*mcp.CallToolResult, mcpOut, error) {
	root, s, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	if err := checkpoint.RejectQuarantinedRun(root); err != nil {
		return nil, nil, err
	}
	if s.Control == "complete" {
		return nil, nil, errors.New("run is already complete")
	}
	if err := mcpControllerIdle(root); err != nil {
		return nil, nil, err
	}
	args := []string{"resume", root}
	if in.RetryFailed {
		args = append(args, "--retry-failed")
	}
	if err := McpStartDetached(args); err != nil {
		return nil, nil, err
	}
	return mcpResult("controller started for "+root, mcpOut{"run": root, "started": true})
}

type mcpRetryInput struct {
	Run            string `json:"run" jsonschema:"run directory path or run name"`
	Cell           string `json:"cell" jsonschema:"cell (node) id to retry"`
	Recursive      bool   `json:"recursive,omitempty" jsonschema:"include descendants"`
	AllowUncertain bool   `json:"allow_uncertain" jsonschema:"explicitly permit replay of uncertain external actions (same as --allow-uncertain); false keeps the safety rules"`
	ConfirmStopped bool   `json:"confirm_stopped" jsonschema:"attest surviving provider/validator processes have been stopped (same as --confirm-stopped)"`
}

func (t *mcpTools) retryCell(_ context.Context, _ *mcp.CallToolRequest, in mcpRetryInput) (*mcp.CallToolResult, mcpOut, error) {
	root, _, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	if in.Cell == "" {
		return nil, nil, errors.New("cell is required")
	}
	report, err := runops.RetryNodeWithReport(root, in.Cell, in.Recursive, in.AllowUncertain, in.ConfirmStopped)
	if err != nil {
		return nil, nil, err
	}
	summary := "nothing to retry; no eligible failed or cancelled tasks were changed"
	if len(report.Retried) > 0 {
		summary = "prepared retry for: " + strings.Join(report.Retried, ", ") + "; resume the run to execute"
	}
	return mcpResult(summary, mcpOut{"retried": nonNil(report.Retried), "waiting": nonNil(report.Waiting), "skipped": nonNil(report.Skipped)})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type mcpApprovalInput struct {
	Run      string `json:"run" jsonschema:"run directory path or run name"`
	Request  string `json:"request" jsonschema:"blocked request id (a cell id of the run)"`
	Decision string `json:"decision" jsonschema:"approve or deny"`
}

func (t *mcpTools) decideApproval(_ context.Context, _ *mcp.CallToolRequest, in mcpApprovalInput) (*mcp.CallToolResult, mcpOut, error) {
	root, s, err := t.openRun(in.Run)
	if err != nil {
		return nil, nil, err
	}
	if !checkpoint.MailboxTaskName.MatchString(in.Request) || !approvals.ValidApprovalAction(in.Decision) {
		return nil, nil, errors.New("request must be a cell id and decision must be approve or deny")
	}
	if s.Tasks[in.Request] == nil {
		return nil, nil, approvals.ErrApprovalTask
	}
	server, err := approvals.NewApprovalServer(root)
	if err != nil {
		return nil, nil, err
	}
	nonce, err := mcpNonce()
	if err != nil {
		return nil, nil, err
	}
	code, message := server.Decide(approvals.ApprovalClaims{Schema: approvals.ApprovalTokenSchema, RunID: s.RunID, Request: in.Request, Action: in.Decision, Nonce: nonce}, "mcp")
	if code != 200 {
		return nil, nil, fmt.Errorf("decision not recorded: %s", message)
	}
	return mcpResult(message, mcpOut{"run": root, "request": in.Request, "decision": in.Decision})
}

func mcpNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
