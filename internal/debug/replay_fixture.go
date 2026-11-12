package debug

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/session"
)

const ReplaySchema = "irrigator.replay-fixture/v1"

type ReplayTask struct {
	ID              string   `json:"id"`
	DependsOn       []string `json:"depends_on"`
	Parent          string   `json:"parent,omitempty"`
	Spawn           bool     `json:"spawn,omitempty"`
	TurnRole        string   `json:"turn_role,omitempty"`
	TurnIndex       int      `json:"turn_index,omitempty"`
	Status          string   `json:"status"`
	Attempt         int      `json:"attempt"`
	Generation      int      `json:"generation"`
	CancelRequested bool     `json:"cancel_requested"`
	ErrorCode       string   `json:"error_code"`
	DiagnosticCode  string   `json:"diagnostic_code"`
	FailurePhase    string   `json:"failure_phase,omitempty"`
	CostReported    bool     `json:"cost_reported"`
	CostUnknown     bool     `json:"cost_unknown"`
}

type ReplayEvent struct {
	Kind string `json:"kind"`
	Task string `json:"task"`
}

type replayLifecycle struct {
	Activity       string   `json:"activity"`
	Blockers       []string `json:"blockers"`
	BlockersTotal  int      `json:"blockers_total"`
	Active         []string `json:"active"`
	ActiveTotal    int      `json:"active_total"`
	UncertainTotal int      `json:"uncertain_total"`
	FailedTotal    int      `json:"failed_total"`
	CancelledTotal int      `json:"cancelled_total"`
}

type replayRecNode struct {
	ID             string           `json:"id"`
	Classification string           `json:"classification"`
	ErrorCode      string           `json:"error_code"`
	Lifecycle      *replayLifecycle `json:"lifecycle,omitempty"`
}

type replayRecovery struct {
	Attention []replayRecNode `json:"attention"`
	NextSteps []string        `json:"next_steps"`
}

type ReplayFixture struct {
	Schema   string         `json:"schema"`
	Scenario string         `json:"scenario"`
	Backend  string         `json:"backend"`
	Tasks    []ReplayTask   `json:"tasks"`
	Events   []ReplayEvent  `json:"events"`
	Recovery replayRecovery `json:"recovery"`
}

// replayFailurePhase classifies a failed task from the shape of its last
// "failed" event detail: a map is the validation commit (cell validation), a
// string is an execution-side failure. Only the shape is read, never the text.
func replayFailurePhase(s *model.State, id string, row *model.TaskState) string {
	if row.Status != "failed" {
		return ""
	}
	phase := ""
	for _, e := range s.Events {
		if e.Task != id || e.Kind != "failed" {
			continue
		}
		switch e.Detail.(type) {
		case string:
			phase = "execution"
		case map[string]any, map[string]string:
			phase = "validation"
		}
	}
	return phase
}

// replayEventKind maps an event kind onto the closed vocabulary a fixture may
// carry: the kinds the runtime already exposes publicly (panelEventActions) plus
// the few replay asserts on. Anything else collapses to the generic "event", so a
// worker that writes free text into an event kind cannot get it into a fixture.
func replayEventKind(kind string) string {
	if _, ok := session.PanelEventActions[kind]; ok {
		return kind
	}
	switch kind {
	case "legacy_cost_unknown", "outcome_uncertain", "turn_verified", "helpers_admitted", "event":
		return kind
	}
	return "event"
}

// exportReplayFixture projects a run. Task IDs are anonymised to t1..tn in plan
// order; only fixed enumerations, booleans, counters and event kinds survive.
func ExportReplayFixture(s *model.State, scenario string) ReplayFixture {
	anon := map[string]string{}
	var order []string
	for i, t := range s.AllTasks() {
		anon[t.ID] = fmt.Sprintf("t%d", i+1)
		order = append(order, t.ID)
	}
	fx := ReplayFixture{Schema: ReplaySchema, Scenario: scenario, Backend: s.Backend, Tasks: []ReplayTask{}, Events: []ReplayEvent{}}
	unknownEvent := map[string]bool{}
	for _, e := range s.Events {
		if e.Kind == "legacy_cost_unknown" {
			unknownEvent[e.Task] = true
		}
	}
	for _, id := range order {
		row := s.Tasks[id]
		if row == nil {
			continue
		}
		deps := []string{}
		for _, d := range row.Spec.DependsOn {
			deps = append(deps, anon[d])
		}
		rt := ReplayTask{ID: anon[id], DependsOn: deps, Parent: anon[row.Spec.Parent], Spawn: row.Spec.Spawn != nil,
			Status: row.Status, Attempt: row.Attempt, Generation: row.Generation, CancelRequested: row.CancelRequested,
			ErrorCode: debugErrorCode(row), DiagnosticCode: model.KnownDiagnosticCode(row.DiagnosticCode),
			FailurePhase: replayFailurePhase(s, id, row), CostReported: row.Cost > 0, CostUnknown: row.CostUnknown || unknownEvent[id]}
		if row.Spec.Turn != nil {
			rt.TurnRole, rt.TurnIndex = row.Spec.Turn.Role, row.Spec.Turn.Index
		}
		fx.Tasks = append(fx.Tasks, rt)
	}
	// Run-level control events are dropped: their count depends on how the run
	// was driven, not on the failure. Event detail is never exported.
	for _, e := range s.Events {
		if e.Task == "" || anon[e.Task] == "" {
			continue
		}
		fx.Events = append(fx.Events, ReplayEvent{Kind: replayEventKind(e.Kind), Task: anon[e.Task]})
	}
	fx.Recovery = replayRecoveryOf(s, anon)
	return fx
}

func replayRecoveryOf(s *model.State, anon map[string]string) replayRecovery {
	rep, _ := debugProject(s, "", "")
	rec := replayRecovery{Attention: []replayRecNode{}, NextSteps: append([]string{}, rep.NextSteps...)}
	mapIDs := func(ids []string) []string {
		out := []string{}
		for _, id := range ids {
			if a, ok := anon[id]; ok {
				id = a
			}
			out = append(out, id)
		}
		return out
	}
	for _, n := range rep.Attention {
		id := n.ID
		if a, ok := anon[id]; ok {
			id = a
		}
		node := replayRecNode{ID: id, Classification: n.Classification, ErrorCode: n.ErrorCode}
		if r := n.Recovery; r != nil {
			node.Lifecycle = &replayLifecycle{Activity: r.Activity, Blockers: mapIDs(r.BlockerIDs), BlockersTotal: r.BlockersTotal,
				Active: mapIDs(r.ActiveIDs), ActiveTotal: r.ActiveTotal, UncertainTotal: r.UncertainTotal,
				FailedTotal: r.FailedTotal, CancelledTotal: r.CancelledTotal}
		}
		rec.Attention = append(rec.Attention, node)
	}
	return rec
}

var (
	replayIDPattern       = regexp.MustCompile(`^t[0-9]{1,4}$`)
	replayScenarioPattern = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	replayStatusSet       = map[string]bool{"pending": true, "running": true, "produced": true, "verified": true, "failed": true, "uncertain": true, "cancelled": true, "waiting": true}
	replayErrCodeRegex    = regexp.MustCompile(`^(|validator_uncertain|cancelled|failed_[a-z_]+|outcome_uncertain_[a-z_]+)$`)
	replayBackendSet      = map[string]bool{"mock": true, "haiku": true, "managed": true}
	replayPhaseSet        = map[string]bool{"": true, "validation": true, "execution": true}
	replayRoleSet         = map[string]bool{"": true, "main": true, "helper": true}
	replayActivitySet     = map[string]bool{model.RecoveryActive: true, model.RecoveryNoActive: true}
	replayClassSet        = map[string]bool{model.RecoveryUncertain: true, model.RecoveryFailed: true, model.RecoveryCancelled: true}
)

// validateReplayFixture is the redaction contract: every string field is an
// anonymous ID, a member of a closed set, a code that matches a fixed pattern
// (known diagnostic codes only), or one of the fixed advice lines produced by
// debugProject. Unknown schema versions are rejected here; unknown JSON fields
// are rejected by decodeReplayFixture.
func ValidateReplayFixture(fx ReplayFixture) error {
	if fx.Schema != ReplaySchema {
		return fmt.Errorf("unsupported schema version %q (this build reads %s)", fx.Schema, ReplaySchema)
	}
	if !replayScenarioPattern.MatchString(fx.Scenario) || !replayBackendSet[fx.Backend] {
		return errors.New("scenario or backend not allowlisted")
	}
	ids := map[string]bool{}
	for _, t := range fx.Tasks {
		if !replayIDPattern.MatchString(t.ID) || !replayStatusSet[t.Status] || !replayErrCodeRegex.MatchString(t.ErrorCode) ||
			!replayPhaseSet[t.FailurePhase] || !replayRoleSet[t.TurnRole] || t.TurnIndex < 0 || t.Attempt < 0 || t.Generation < 0 {
			return fmt.Errorf("task %q field not allowlisted", t.ID)
		}
		if t.DiagnosticCode != "" && model.KnownDiagnosticCode(t.DiagnosticCode) == "" {
			return fmt.Errorf("task %q diagnostic code", t.ID)
		}
		if t.Parent != "" && !replayIDPattern.MatchString(t.Parent) {
			return fmt.Errorf("task %q parent", t.ID)
		}
		for _, d := range t.DependsOn {
			if !replayIDPattern.MatchString(d) {
				return fmt.Errorf("task %q dependency", t.ID)
			}
		}
		if ids[t.ID] {
			return fmt.Errorf("duplicate task %q", t.ID)
		}
		ids[t.ID] = true
	}
	for _, e := range fx.Events {
		if replayEventKind(e.Kind) != e.Kind || !ids[e.Task] {
			return fmt.Errorf("event %q/%q not allowlisted", e.Kind, e.Task)
		}
	}
	idList := func(list []string) bool {
		for _, id := range list {
			if !ids[id] {
				return false
			}
		}
		return true
	}
	for _, n := range fx.Recovery.Attention {
		if !ids[n.ID] || !replayErrCodeRegex.MatchString(n.ErrorCode) || !replayClassSet[n.Classification] {
			return fmt.Errorf("recovery node %q not allowlisted", n.ID)
		}
		if l := n.Lifecycle; l != nil && (!replayActivitySet[l.Activity] || !idList(l.Blockers) || !idList(l.Active)) {
			return fmt.Errorf("recovery node %q lifecycle not allowlisted", n.ID)
		}
	}
	for _, step := range fx.Recovery.NextSteps {
		ok := false
		for _, fixed := range debugFixedSteps {
			ok = ok || step == fixed
		}
		if !ok {
			return errors.New("next step is not a fixed advice line")
		}
	}
	return nil
}

// replayMaxJSONDepth bounds nesting for the duplicate-key walk; fixtures are shallow.
const replayMaxJSONDepth = 64

// replayKeyFold canonicalises an object key the way encoding/json matches struct
// fields: case-insensitively using Unicode simple folding (so Schema, schema and
// a long-s variant collide), compared after JSON unescaping.
func replayKeyFold(k string) string {
	var b strings.Builder
	for _, r := range k {
		min := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < min {
				min = f
			}
		}
		b.WriteRune(min)
	}
	return b.String()
}

// rejectDuplicateJSONKeys fails if any object, at any depth, repeats a key. Go's
// decoder silently keeps the last duplicate, which would let a tampered fixture
// show the validator one value and a reader another. The message carries a byte
// offset only, never the key or its value.
func rejectDuplicateJSONKeys(raw []byte) error {
	type frame struct {
		object  bool
		wantKey bool
		keys    map[string]struct{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var stack []frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if len(stack) != 0 {
				return fmt.Errorf("truncated fixture JSON near byte %d", dec.InputOffset())
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("malformed fixture JSON near byte %d", dec.InputOffset())
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				if len(stack) >= replayMaxJSONDepth {
					return fmt.Errorf("fixture JSON nests deeper than %d", replayMaxJSONDepth)
				}
				stack = append(stack, frame{object: v == '{', wantKey: v == '{', keys: map[string]struct{}{}})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		default:
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
				key, _ := tok.(string)
				canon := replayKeyFold(key)
				if _, dup := stack[n-1].keys[canon]; dup {
					return fmt.Errorf("duplicate object key in fixture JSON near byte %d", dec.InputOffset())
				}
				stack[n-1].keys[canon] = struct{}{}
				stack[n-1].wantKey = false
			} else {
				valueDone()
			}
		}
	}
}

// decodeReplayFixture parses and validates a fixture: unknown fields, trailing
// data, unknown schema versions and non-allowlisted values are all rejected.
func DecodeReplayFixture(raw []byte) (ReplayFixture, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return ReplayFixture{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fx ReplayFixture
	if err := dec.Decode(&fx); err != nil {
		return fx, err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return fx, errors.New("trailing data after fixture")
	}
	return fx, ValidateReplayFixture(fx)
}

// Redaction scan. Independent of the allowlist: it looks at the serialized
// bytes for anything that resembles a secret or a local path, so a future field
// added to the format cannot silently carry one.
var replaySecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`/(Users|home)/[A-Za-z0-9._-]+`),
	regexp.MustCompile(`(?i)\b[a-z]:\\users\\`),
	regexp.MustCompile(`\b(sk|pk|ghp|gho|xox[bap]|AKIA)[-_A-Za-z0-9]{12,}`),
	regexp.MustCompile(`(?i)(bearer|authorization|api[_-]?key|secret|password|token)\s*[:=]`),
	regexp.MustCompile(`https?://`),
	regexp.MustCompile(`\b[A-Fa-f0-9]{32,}\b`),
	regexp.MustCompile(`\.\./|~/`),
}

func ReplaySecretScan(raw []byte) []string {
	var hits []string
	for _, re := range replaySecretPatterns {
		if m := re.Find(raw); m != nil {
			hits = append(hits, re.String())
		}
	}
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 && bytes.Contains(raw, []byte(home)) {
		hits = append(hits, "home directory path")
	}
	return hits
}

// buildReplayFixtureBytes exports, validates, serialises, then decodes and scans
// the serialised bytes. Any failure is returned and nothing is written.
func BuildReplayFixtureBytes(s *model.State, scenario string) ([]byte, error) {
	fx := ExportReplayFixture(s, scenario)
	if err := ValidateReplayFixture(fx); err != nil {
		return nil, fmt.Errorf("redaction check rejected the export: %w", err)
	}
	out, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	if _, err := DecodeReplayFixture(out); err != nil {
		return nil, fmt.Errorf("redaction check rejected the serialised export: %w", err)
	}
	if hits := ReplaySecretScan(out); len(hits) > 0 {
		return nil, fmt.Errorf("redaction check rejected the serialised export: matched %s", strings.Join(hits, ", "))
	}
	return out, nil
}

const debugFixtureUsage = `irrigator debug fixture export RUN_DIR --out FILE [--scenario NAME]   (experimental)

Write a redacted replay fixture (schema irrigator.replay-fixture/v1) of one run.
Read-only on the run. Refuses to overwrite FILE, writes it 0600, and writes
nothing if the redaction check rejects its own output. NAME is a lowercase
label ([a-z0-9-], default exported-run); it is the only caller-chosen text.
`

// replayMaxStateBytes bounds the checkpoint the exporter will read; a larger
// state.json is refused before it is decoded.
const replayMaxStateBytes = 256 << 20

// These checks are path-based and best effort: they are not a defence against a
// worker that modifies the run directory while the export is running.
// replayExportGuards refuses inputs and outputs the exporter must not follow: a
// state.json that is not a regular file or is implausibly large (a supervised
// worker controls the run directory), and an --out located inside the run
// directory (a worker-planted symlinked subdirectory could redirect the write).
func replayExportGuards(runDir, out string) error {
	if st, err := os.Lstat(filepath.Join(runDir, "state.json")); err == nil {
		if !st.Mode().IsRegular() {
			return errors.New("run state.json must be a regular file")
		}
		if st.Size() > replayMaxStateBytes {
			return fmt.Errorf("run state.json exceeds %d bytes", int64(replayMaxStateBytes))
		}
	}
	absRunIn, err := filepath.Abs(runDir)
	if err != nil {
		return fmt.Errorf("cannot resolve the run directory: %w", err)
	}
	absRun, err := filepath.EvalSymlinks(absRunIn)
	if err != nil {
		return nil // an unreadable run directory is reported by readState
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("cannot resolve --out: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absOut))
	if err != nil {
		return nil // a missing parent is reported when the file is created
	}
	target := filepath.Join(parent, filepath.Base(absOut))
	rel, err := filepath.Rel(absRun, target)
	if err != nil {
		return fmt.Errorf("cannot compare --out with the run directory: %w", err) // fail closed
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("--out must be outside the run directory")
	}
	return nil
}

func debugFixtureRun(args []string, w io.Writer) (int, error) {
	if len(args) == 0 || args[0] != "export" {
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
			fmt.Fprint(w, debugFixtureUsage)
			return 0, nil
		}
		return 2, errors.New("usage: irrigator debug fixture export RUN_DIR --out FILE; use irrigator debug --help")
	}
	fs := flag.NewFlagSet("debug fixture export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", "", "fixture file to create (must not exist)")
	scenario := fs.String("scenario", "exported-run", "lowercase scenario label [a-z0-9-]")
	if err := commandflags.ParseCommandArguments(fs, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(w, debugFixtureUsage)
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 1 || *out == "" {
		return 2, errors.New("debug fixture export expects exactly one RUN_DIR and --out FILE")
	}
	if !replayScenarioPattern.MatchString(*scenario) {
		return 2, errors.New("debug fixture export: --scenario must match [a-z0-9-]{1,64}")
	}
	if err := replayExportGuards(fs.Arg(0), *out); err != nil {
		return 1, fmt.Errorf("debug fixture export: %w", err)
	}
	s, err := checkpoint.ReadState(fs.Arg(0))
	if err != nil {
		return 1, fmt.Errorf("debug fixture export: run state unreadable: %w", err)
	}
	if !replayBackendSet[s.Backend] {
		return 1, errors.New("debug fixture export: only mock, haiku and managed runs can be exported in v1")
	}
	data, err := BuildReplayFixtureBytes(s, *scenario)
	if err != nil {
		return 1, fmt.Errorf("debug fixture export: %w", err)
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return 1, errors.New("debug fixture export: --out already exists; refusing to overwrite")
		}
		return 1, fmt.Errorf("debug fixture export: cannot create --out: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(*out)
		return 1, fmt.Errorf("debug fixture export: write failed: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(*out)
		return 1, err
	}
	fmt.Fprintf(w, "wrote %s (experimental replay fixture %s, %d tasks)\n", *out, ReplaySchema, len(s.Tasks))
	return 0, nil
}
