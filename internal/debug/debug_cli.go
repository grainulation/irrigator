package debug

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/session"
)

// irrigator debug is read-only run diagnosis. It reads the durable checkpoint
// once and projects bounded, classified evidence. It starts no provider, takes
// no lock, writes no file, and never prints error text, prompts, results or
// provider configuration. Classification and advice come from the shared
// lifecycleRecoveryFor and status constants; this file only adds the attempt and
// error-code fields that the shared projection does not carry.
const debugUsage = `irrigator debug RUN_DIR [--json] [--node ID]
irrigator debug fixture export RUN_DIR --out FILE [--scenario NAME]   (experimental)

Read-only diagnosis of one durable run: control state, node counts, nodes that
need attention with safe classifications, and recent allowlisted events.
Starts no provider, retries nothing, and never prints error text, prompts or
results. Use status, inspect and logs for more detail.

fixture export (experimental; the format may change before it is stable) writes
a redacted replay fixture of the run: fixed status and error codes, anonymous
task IDs, event kinds and the recovery decision. It refuses to overwrite FILE,
writes it 0600, and writes nothing if its own redaction check rejects the result.
`

const (
	debugSchema     = "irrigator.debug/v1"
	debugMaxNodes   = 25
	debugMaxEvents  = 10
	debugBasisText  = "single read of state.json plus non-blocking lease probes; error text, prompts, results and provider config are never projected; classification is status and explicit evidence only"
	debugValidatorU = "validator outcome uncertain; reconcile external outcome before explicit replay" // fixed text written by commitUncertainValidation
)

// Fixed advice lines. debugProject emits only these, and the replay fixture
// validator accepts only these, so the two cannot drift apart.
const (
	debugStepInspect   = "irrigator inspect RUN_DIR NODE and irrigator logs RUN_DIR --task NODE are read-only follow-ups"
	debugStepUncertain = "uncertain: reconcile the external outcome first; resolve or retry --allow-uncertain needs an explicit user decision"
	debugStepFailed    = "failed: status alone does not establish external effects; review before an explicit retry"
	debugStepCancelled = "cancelled: operator decision to retry or revise"
	debugStepNoWorker  = "control is run with no live worker leases: not proof of a stall; confirm with status before any resume"
)

var debugFixedSteps = []string{debugStepInspect, debugStepUncertain, debugStepFailed, debugStepCancelled, debugStepNoWorker}

// Reasons accepted from the fixed uncertainOutcomeMessage text. Unknown text is never echoed.
var debugUncertainReasons = []string{"provider_reported", "invalid_outcome", "deadline", "cancelled", "malformed_response", "unspecified"}

type debugNode struct {
	ID              string                   `json:"id"`
	Parent          string                   `json:"parent,omitempty"`
	Status          string                   `json:"status"`
	Attempt         int                      `json:"attempt"`
	Generation      int                      `json:"generation"`
	CancelRequested bool                     `json:"cancel_requested,omitempty"`
	CostUSD         float64                  `json:"reported_cost_usd"`
	ErrorRecorded   bool                     `json:"error_recorded"`
	ErrorCode       string                   `json:"error_code,omitempty"`
	Classification  string                   `json:"classification,omitempty"`
	Recovery        *model.LifecycleRecovery `json:"recovery,omitempty"`
}

type debugReport struct {
	Schema           string               `json:"schema"`
	ReadOnly         bool                 `json:"read_only"`
	RunID            string               `json:"run_id"`
	Control          string               `json:"control"`
	Backend          string               `json:"backend"`
	GraphRevision    int                  `json:"graph_revision"`
	CostUSD          float64              `json:"reported_cost_usd"`
	NodesTotal       int                  `json:"nodes_total"`
	StatusCounts     map[string]int       `json:"status_counts"`
	WorkersActive    *int                 `json:"workers_active"`
	WorkersKnown     bool                 `json:"workers_known"`
	Attention        []debugNode          `json:"attention"`
	AttentionTotal   int                  `json:"attention_total"`
	AttentionOmitted int                  `json:"attention_omitted"`
	Selected         *debugNode           `json:"selected,omitempty"`
	Events           []session.PanelEvent `json:"recent_events"`
	EventsTotal      int                  `json:"events_total"`
	NextSteps        []string             `json:"next_steps"`
	Basis            string               `json:"basis"`
}

func DebugCLI(args []string) (int, error) { return DebugRun(args, os.Stdout) }

func DebugRun(args []string, w io.Writer) (int, error) {
	// Only the explicit `fixture export` sequence selects the verb, so
	// `irrigator debug fixture` still diagnoses a run directory named "fixture".
	if len(args) > 1 && args[0] == "fixture" && args[1] == "export" {
		return debugFixtureRun(args[1:], w)
	}
	fs := flag.NewFlagSet("debug", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "JSON diagnosis")
	node := fs.String("node", "", "also project one node by ID")
	var positional []string
	rest := args
	for {
		if err := commandflags.ParseCommandFlags(fs, rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(w, debugUsage)
				return 0, nil
			}
			return 2, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return 2, errors.New("debug expects exactly one RUN_DIR; use irrigator debug --help")
	}
	s, err := checkpoint.ReadState(positional[0])
	if err != nil {
		return 1, fmt.Errorf("debug: run state unreadable: %w", err)
	}
	report, err := debugProject(s, positional[0], *node)
	if err != nil {
		return 1, err
	}
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return 1, err
		}
		return 0, nil
	}
	debugText(w, report)
	return 0, nil
}

// debugErrorCode projects a fixed code for the error field without echoing it.
func debugErrorCode(row *model.TaskState) string {
	switch row.Status {
	case "uncertain":
		if row.Error == debugValidatorU {
			return "validator_uncertain"
		}
		for _, r := range debugUncertainReasons {
			reason := r
			if r == "unspecified" {
				reason = ""
			}
			if row.Error == model.UncertainOutcomeMessage(&model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: reason}) {
				return "outcome_uncertain_" + r
			}
		}
		return "outcome_uncertain_unclassified"
	case "failed":
		if code := model.KnownDiagnosticCode(row.DiagnosticCode); code != "" {
			return "failed_" + code
		}
		if row.Error == "" {
			return "failed_no_error_recorded"
		}
		return "failed_error_not_projected"
	case "cancelled":
		return "cancelled"
	}
	return ""
}

func debugNodeFor(s *model.State, id string) debugNode {
	row := s.Tasks[id]
	n := debugNode{ID: id, Parent: row.Spec.Parent, Status: row.Status, Attempt: row.Attempt, Generation: row.Generation,
		CancelRequested: row.CancelRequested, CostUSD: row.Cost, ErrorRecorded: row.Error != "", ErrorCode: debugErrorCode(row)}
	switch row.Status {
	case "uncertain":
		n.Classification = model.RecoveryUncertain
	case "failed":
		n.Classification = model.RecoveryFailed
	case "cancelled":
		n.Classification = model.RecoveryCancelled
	}
	if rec := model.LifecycleRecoveryFor(s, row); rec != nil {
		n.Recovery, n.Classification = rec, rec.Classification
	}
	if !observer.ObsKnownStatus[row.Status] {
		n.Status = "unknown"
	}
	return n
}

func debugNeedsAttention(n debugNode) bool {
	switch n.Status {
	case "failed", "uncertain", "cancelled":
		return true
	}
	return n.Recovery != nil
}

// debugLeases probes leases without creating files or waiting for a worker.
func debugLeases(root string, ids []string) (int, bool) {
	active, known := 0, true
	for _, id := range ids {
		f, err := os.Open(filepath.Join(root, "cells", id, "lease"))
		if err != nil {
			known = false
			continue
		}
		e := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		switch {
		case errors.Is(e, syscall.EWOULDBLOCK):
			active++
		case e != nil:
			known = false
		default:
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		_ = f.Close()
	}
	return active, known
}

// debugProject is pure over State except the optional lease probe (root != "").
func debugProject(s *model.State, root, only string) (debugReport, error) {
	r := debugReport{Schema: debugSchema, ReadOnly: true, RunID: s.RunID, Control: s.Control, Backend: s.Backend,
		GraphRevision: s.GraphRevision, CostUSD: s.ReportedCost(), StatusCounts: map[string]int{}, Attention: []debugNode{}, NextSteps: []string{}, Basis: debugBasisText}
	if only != "" && s.Tasks[only] == nil {
		return r, errors.New("debug: unknown node")
	}
	ids := make([]string, 0, len(s.Tasks))
	for id, row := range s.Tasks {
		if row != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	kinds := map[string]bool{}
	for _, id := range ids {
		n := debugNodeFor(s, id)
		r.NodesTotal++
		r.StatusCounts[n.Status]++
		if id == only {
			sel := n
			r.Selected = &sel
		}
		if !debugNeedsAttention(n) {
			continue
		}
		r.AttentionTotal++
		kinds[n.Classification] = true
		if len(r.Attention) < debugMaxNodes {
			r.Attention = append(r.Attention, n)
		}
	}
	r.AttentionOmitted = r.AttentionTotal - len(r.Attention)
	if root != "" {
		active, known := debugLeases(root, ids)
		r.WorkersKnown = known
		if known {
			r.WorkersActive = &active
		}
	}
	events := session.PanelEvents(s)
	r.EventsTotal = len(s.Events)
	if len(events) > debugMaxEvents {
		events = events[len(events)-debugMaxEvents:]
	}
	r.Events = events
	r.NextSteps = append(r.NextSteps, debugStepInspect)
	if kinds[model.RecoveryUncertain] {
		r.NextSteps = append(r.NextSteps, debugStepUncertain)
	}
	if kinds[model.RecoveryFailed] {
		r.NextSteps = append(r.NextSteps, debugStepFailed)
	}
	if kinds[model.RecoveryCancelled] {
		r.NextSteps = append(r.NextSteps, debugStepCancelled)
	}
	if s.Control == "run" && r.WorkersKnown && r.WorkersActive != nil && *r.WorkersActive == 0 {
		r.NextSteps = append(r.NextSteps, debugStepNoWorker)
	}
	return r, nil
}

func debugText(w io.Writer, r debugReport) {
	fmt.Fprintf(w, "Irrigator debug (read-only) run=%s control=%s backend=%s revision=%d reported_cost_usd=%.4f\n", r.RunID, r.Control, r.Backend, r.GraphRevision, r.CostUSD)
	keys := make([]string, 0, len(r.StatusCounts))
	for k := range r.StatusCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "Nodes: %d total", r.NodesTotal)
	for _, k := range keys {
		fmt.Fprintf(w, " | %s=%d", k, r.StatusCounts[k])
	}
	fmt.Fprintln(w)
	if r.WorkersKnown && r.WorkersActive != nil {
		fmt.Fprintf(w, "Workers with live leases: %d\n", *r.WorkersActive)
	} else {
		fmt.Fprintln(w, "Workers: unknown (lease probe incomplete)")
	}
	fmt.Fprintf(w, "Attention: %d total, %d shown, %d omitted\n", r.AttentionTotal, len(r.Attention), r.AttentionOmitted)
	for _, n := range r.Attention {
		debugTextNode(w, n)
	}
	if r.Selected != nil {
		fmt.Fprintln(w, "Selected node:")
		debugTextNode(w, *r.Selected)
	}
	fmt.Fprintf(w, "Recent events: %d shown of %d recorded (allowlisted kinds only)\n", len(r.Events), r.EventsTotal)
	for _, e := range r.Events {
		fmt.Fprintf(w, "  %d %s %s\n", e.Seq, e.Kind, e.Task)
	}
	fmt.Fprintln(w, "Next (descriptive; nothing was executed):")
	for _, step := range r.NextSteps {
		fmt.Fprintf(w, "  - %s\n", step)
	}
	fmt.Fprintf(w, "Basis: %s\n", r.Basis)
}

func debugTextNode(w io.Writer, n debugNode) {
	fmt.Fprintf(w, "  %s status=%s attempt=%d generation=%d", n.ID, n.Status, n.Attempt, n.Generation)
	if n.ErrorCode != "" {
		fmt.Fprintf(w, " code=%s", n.ErrorCode)
	}
	if n.Classification != "" {
		fmt.Fprintf(w, " class=%s", n.Classification)
	}
	fmt.Fprintln(w)
	if rec := n.Recovery; rec != nil {
		fmt.Fprintf(w, "    recovery: activity=%s blockers=%v (+%d omitted) active=%v (+%d omitted)\n    advice: %s\n", rec.Activity, rec.BlockerIDs, rec.BlockersOmitted, rec.ActiveIDs, rec.ActiveOmitted, rec.Advice)
	}
}
