package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
)

func request(s *model.State, t model.Task, attempt int, inputs map[string]map[string]string, opts model.Options) model.WorkRequest {
	op := s.Tasks[t.ID].OperationID
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%d", op, attempt))))
	var boundaries *model.WorkerBoundaries
	// Preserve custom command-provider request bytes; built-in model adapters
	// understand this additive execution metadata.
	if !checkpoint.ManagedRun(s) && checkpoint.ProviderName(s, t) == "haiku" {
		boundaries = workerBoundaries(s, t)
	}
	contexts := managed.TaskContext(s.Plan, t)
	if row := s.Tasks[t.ID]; row != nil && len(row.Notes) > 0 {
		contexts[messaging.NotesContextKey] = messaging.NotesContext(row.Notes)
	}
	return model.WorkRequest{Boundaries: boundaries, RunID: s.RunID, Protocol: model.ProtocolVersion, Kind: "task", RequestID: id, OperationID: op, Task: t, Attempt: attempt,
		Context: contexts, Dependencies: inputs, TimeoutMS: opts.Timeout.Milliseconds(), MaxCostUSD: opts.Budget}
}

func External(ctx context.Context, cell, prefix string, spec model.CommandSpec, req model.WorkRequest) (model.WorkResponse, error) {
	var response model.WorkResponse
	if err := launch.PrepareWorkContext(&req, cell); err != nil {
		return response, model.MarkNotStarted(err)
	}
	input, err := json.Marshal(req)
	if err != nil {
		return response, err
	}
	if len(spec.Command) == 0 {
		return response, errors.New("missing command")
	}
	var cmd *exec.Cmd
	var sandboxBin, sandboxName string
	if spec.Sandbox != nil {
		var sbErr error
		if cmd, sandboxBin, sandboxName, sbErr = launch.SandboxCommand(spec, cell, req); sbErr != nil {
			return response, sbErr
		}
	} else {
		cmd = exec.Command(spec.Command[0], spec.Command[1:]...)
	}
	cmd.Dir = cell
	// Explicit env names, never persisted secret values. Providers may opt into
	// credential variables; built-in Haiku retains the host CLI's normal auth.
	names := append([]string{"PATH", "HOME", "TMPDIR", "TEMP", "SystemRoot"}, spec.Env...)
	if spec.Sandbox != nil {
		// The runtime client reads these for `-e NAME`; values never enter argv.
		names = append(names, spec.Sandbox.Env...)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !seen[name] {
			if value, ok := os.LookupEnv(name); ok {
				cmd.Env = append(cmd.Env, name+"="+value)
			}
			seen[name] = true
		}
	}
	forwarded := forwardedSecretValues(spec)
	raw, processErr := launch.RunProcess(ctx, cmd, prefix, input)
	if sandboxName != "" && processErr != nil {
		launch.
			// Killing the runtime client may leave the container running.
			KillSandboxContainer(sandboxBin, sandboxName, cmd.Env)
	}
	if errors.Is(processErr, context.Canceled) || errors.Is(processErr, context.DeadlineExceeded) {
		if req.Kind == "task" {
			// The supervisor preserves bounded stdout while stopping the group.
			// Retain attributable partial billing without implying final settlement.
			evidence, _ := model.ReadProcessEvidence(prefix+".stdout.json", model.MaxOutputBytes)
			if billing := provider.ExternalCostEvidence(evidence, req); billing.Known {
				response.Protocol, response.RequestID = model.ProtocolVersion, req.RequestID
				response.CostMicros, response.CostMicrosExplicit = billing.Micros, billing.Explicit
				response.CostUSD = cost.MicrosToFloat(billing.Micros)
				response.CostBasis = "reported"
			} else {
				response.CostBasis = "unknown"
			}
			var evObj map[string]any
			dec := json.NewDecoder(bytes.NewReader(evidence))
			dec.UseNumber()
			if dec.Decode(&evObj) == nil {
				if uRaw, hasU := evObj["usage"]; hasU && uRaw != nil {
					if u, err := model.ParseStrictUsage(uRaw); err == nil && u != nil {
						response.Usage = u
					}
				}
				if mRaw, hasM := evObj["model"]; hasM && mRaw != nil {
					if mStr, ok := mRaw.(string); ok && model.ValidUsageModel(mStr) {
						response.Model = mStr
					}
				}
			}
		}
		return response, model.TransportOutcome(req, processErr)
	}
	if req.Kind == "task" {
		strict := json.NewDecoder(bytes.NewReader(raw))
		value, parseErr := jsonio.ParseStrictJSON(strict, 0)
		if parseErr != nil || !utf8.Valid(raw) || strict.Decode(new(any)) != io.EOF {
			return response, model.MalformedOutcome(req, raw, processErr, errors.New("provider response must be one unambiguous JSON object"))
		}
		if object, ok := value.(map[string]any); ok {
			for key := range object {
				for _, canonical := range []string{"protocol", "request_id", "cost_usd", "cost_micros", "outcome", "error", "output", "usage", "model"} {
					if strings.EqualFold(key, canonical) && key != canonical {
						return response, model.MalformedOutcome(req, raw, processErr, errors.New("provider response has noncanonical field alias"))
					}
				}
			}
		}
	}
	// Cost is optional telemetry for command providers. Decode the contract
	// independently so an unavailable monetary estimate cannot invalidate output.
	decodeRaw := raw
	if req.Kind == "task" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil && fields != nil {
			delete(fields, "cost_usd")
			delete(fields, "usage")
			decodeRaw, _ = json.Marshal(fields)
			billing := provider.ExternalCostEvidence(raw, req)
			if billing.Invalid {
				// A malformed or contradictory cost_micros is never optional telemetry.
				return model.WorkResponse{}, model.MalformedOutcome(req, raw, processErr, errors.New("invalid reported cost"))
			}
			// The cost-stripped body still carries cost_micros, so the decoder below
			// records its presence; seed USD from the same evidence.
			response.CostUSD, response.CostUnknown = cost.MicrosToFloat(billing.Micros), !billing.Known
			if !billing.Known {
				response.CostUSD = 0
				response.CostBasis = "unknown"
			} else {
				response.CostBasis = "reported"
			}
			var rawObj map[string]any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&rawObj); err == nil {
				if uRaw, hasU := rawObj["usage"]; hasU {
					if uRaw == nil {
						return response, model.MalformedOutcome(req, raw, processErr, errors.New("usage must be a JSON object"))
					}
					u, uerr := model.ParseStrictUsage(uRaw)
					if uerr != nil {
						return response, model.MalformedOutcome(req, raw, processErr, fmt.Errorf("invalid provider usage: %w", uerr))
					}
					response.Usage = u
				}
				if mRaw, hasM := rawObj["model"]; hasM && mRaw != nil {
					if mStr, ok := mRaw.(string); ok && model.ValidUsageModel(mStr) {
						response.Model = mStr
						if response.Usage != nil && response.Usage.Model == "" {
							response.Usage.Model = mStr
						} else if response.Usage == nil {
							response.Usage = &model.TaskUsage{Model: mStr}
						}
					} else {
						return response, model.MalformedOutcome(req, raw, processErr, errors.New("invalid model in provider response"))
					}
				}
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(decodeRaw))
	if err = decoder.Decode(&response); err != nil {
		return response, model.MalformedOutcome(req, raw, processErr, fmt.Errorf("invalid provider response: %w (%v); expected one %s JSON response on stdout with the matching request_id (see docs/provider-protocol.md)", err, processErr, model.ProtocolVersion))
	}
	response.Error = scrubForwarded(response.Error, forwarded)
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return response, model.MalformedOutcome(req, raw, processErr, errors.New("provider must return exactly one JSON object"))
	}
	if response.Protocol != model.ProtocolVersion || response.RequestID != req.RequestID {
		return response, model.MalformedOutcome(req, raw, processErr, errors.New("provider protocol or request_id mismatch"))
	}
	// A wire cost_micros (even zero) is authoritative; see WorkResponse.resolveCost.
	costMicros, err := response.ResolveCost()
	if err != nil {
		return model.WorkResponse{}, model.MalformedOutcome(req, raw, processErr, errors.New("invalid reported cost"))
	}
	response.CostMicros = costMicros
	response.CostUSD = cost.MicrosToFloat(costMicros)
	certainty, certaintyErr := model.ParseOutcome(response.Outcome)
	if certaintyErr != nil {
		return response, &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: "invalid_outcome", Err: certaintyErr}
	}
	// The response itself must carry the required rejection error. A failed
	// process cannot make a contradictory not_started response trustworthy.
	if certainty == model.OutcomeNotStarted && response.Error == "" {
		return response, model.SuccessOutcomeError(certainty)
	}
	if req.Kind == "task" {
		if certainty == model.OutcomeNotStarted {
			response.CostUnknown = false
		}
		if certainty == model.OutcomeNotStarted && response.CostUSD > 0 {
			return response, &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: "invalid_outcome", Err: errors.New("not-started outcome contradicts positive reported cost")}
		}
		if cost.CostExceeds(response.CostUSD, req.MaxCostUSD) {
			response.Output = nil
			response.DiagnosticCode = model.DiagProviderOperationBudget
			if certainty == "" {
				certainty = model.OutcomeSettled
				if processErr != nil {
					certainty = model.OutcomeUncertain
				}
			}
			return response, model.WithOutcome(certainty, errors.New("provider exceeded per-call budget"))
		}
	}
	if processErr != nil {
		return response, model.WithOutcome(certainty, fmt.Errorf("provider exited unsuccessfully: %w", processErr))
	}
	if response.Error != "" {
		return response, model.WithOutcome(certainty, errors.New(response.Error))
	}
	if err := launch.RecordContextReceipt(prefix, req, response.ContextReceipt); err != nil {
		return response, model.MalformedOutcome(req, raw, processErr, err)
	}
	return response, model.SuccessOutcomeError(certainty)
}

// minForwardedSecretLen keeps the scrub from rewriting short, common values
// (a flag such as "1" or "true") that happen to appear in provider messages.
const minForwardedSecretLen = 6

// forwardedScrubber finds forwarded credential values in provider text: verbatim
// and percent-encoded (needles, matched against the percent-decoded text) and in
// base64 and JSON-escaped form (literals).
type forwardedScrubber struct {
	needles  [][]byte
	literals []string
}

// forwardedSecretValues builds the scrubber for the variables a provider opted
// into (CommandSpec.Env and Sandbox.Env). The always-present baseline (PATH,
// HOME, ...) is excluded, as are values shorter than minForwardedSecretLen.
func forwardedSecretValues(spec model.CommandSpec) forwardedScrubber {
	names := append([]string(nil), spec.Env...)
	if spec.Sandbox != nil {
		names = append(names, spec.Sandbox.Env...)
	}
	seen := map[string]bool{}
	var out forwardedScrubber
	needle := func(v string) {
		if !seen["n:"+v] {
			seen["n:"+v] = true
			out.needles = append(out.needles, []byte(v))
		}
	}
	literal := func(v string) {
		if v != "" && !seen["l:"+v] {
			seen["l:"+v] = true
			out.literals = append(out.literals, v)
		}
	}
	for _, name := range names {
		v, ok := os.LookupEnv(name)
		if !ok || len(v) < minForwardedSecretLen {
			continue
		}
		needle(v)
		needle(strings.ReplaceAll(v, " ", "+")) // form-encoded space
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			literal(enc.EncodeToString([]byte(v)))
		}
		if quoted, err := json.Marshal(v); err == nil {
			literal(string(quoted[1 : len(quoted)-1]))
		}
	}
	// Longest first, so a value that contains another is replaced whole.
	sort.Slice(out.literals, func(i, j int) bool { return len(out.literals[i]) > len(out.literals[j]) })
	return out
}

// scrubForwarded removes any forwarded credential a provider reflected into its
// own error text, so it cannot reach terminal output, state or the notification
// log through the run's error message.
//
// Percent-encoded echoes are found without a pattern per credential: the text is
// percent-decoded once (either hex case, any character), each credential is
// searched in the decoded bytes with a linear substring search, and the matching
// ranges of the ORIGINAL text are replaced. Cost is linear in the text length.
func scrubForwarded(text string, s forwardedScrubber) string {
	for _, lit := range s.literals {
		text = strings.ReplaceAll(text, lit, "[redacted]")
	}
	if len(s.needles) == 0 || text == "" {
		return text
	}
	dec := make([]byte, 0, len(text))
	from := make([]int, 0, len(text)) // original offset where each decoded byte starts
	to := make([]int, 0, len(text))   // original offset where it ends
	for i := 0; i < len(text); {
		if text[i] == '%' && i+2 < len(text) && isHex(text[i+1]) && isHex(text[i+2]) {
			dec = append(dec, unhex(text[i+1])<<4|unhex(text[i+2]))
			from, to = append(from, i), append(to, i+3)
			i += 3
			continue
		}
		dec = append(dec, text[i])
		from, to = append(from, i), append(to, i+1)
		i++
	}
	var marked []bool
	for _, n := range s.needles {
		for off := 0; ; {
			at := bytes.Index(dec[off:], n)
			if at < 0 {
				break
			}
			at += off
			if marked == nil {
				marked = make([]bool, len(text))
			}
			for pos := from[at]; pos < to[at+len(n)-1]; pos++ {
				marked[pos] = true
			}
			off = at + len(n) // non-overlapping: keeps cost linear when text and credential repeat
		}
	}
	if marked == nil {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if !marked[i] {
			b.WriteByte(text[i])
		} else if i == 0 || !marked[i-1] {
			b.WriteString("[redacted]")
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

func validateCandidate(ctx context.Context, cell string, s *model.State, row *model.TaskState) (string, error) {
	if err := artifactstore.Accept(row.Spec, row.Result); err != nil {
		return "structural", err
	}
	level := "structural"
	for i, c := range row.Spec.Checks {
		level = "task-checks"
		value := row.Result[c.Field]
		var err error
		switch c.Kind {
		case "contains":
			if !strings.Contains(value, c.Value) {
				err = fmt.Errorf("%s must contain %q", c.Field, c.Value)
			}
		case "min_length":
			if utf8.RuneCountInString(value) < c.Minimum {
				err = fmt.Errorf("%s is shorter than %d characters", c.Field, c.Minimum)
			}
		case "go_source":
			_, err = parser.ParseFile(token.NewFileSet(), c.Field+".go", value, parser.AllErrors)
		case "command":
			timeout := time.Duration(c.TimeoutSeconds * float64(time.Second))
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			checkCtx, cancel := context.WithTimeout(ctx, timeout)
			req := request(s, row.Spec, row.Attempt, nil, model.Options{Timeout: timeout})
			req.Kind = "validate"
			req.Candidate = row.Result
			req.RequestID += fmt.Sprintf("/check/%d", i)
			response, e := External(checkCtx, cell, filepath.Join(cell, strconv.Itoa(row.Attempt)+fmt.Sprintf(".check-%d", i)), model.CommandSpec{Command: c.Command, Env: c.Env}, req)
			cancel()
			err = e
			if err == nil && (response.Passed == nil || !*response.Passed) {
				err = errors.New("validator did not pass candidate")
			}
		default:
			err = errors.New("unknown check")
		}
		if err != nil {
			return level, fmt.Errorf("check %d (%s): %w", i, c.Kind, err)
		}
	}
	return level, nil
}
func Finalize(root, id string, leases ...*os.File) error {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	row := s.Tasks[id]
	if row == nil || row.Status != "produced" {
		return nil
	}
	fence := graph.FenceForCell(s, row)
	cells := CheckpointCells{Root: root}
	base := context.Background()
	if len(leases) > 0 && leases[0] != nil {
		base = context.WithValue(base, launch.ProcessLeaseKey{}, leases[0])
	}
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s, e := checkpoint.ReadState(root)
				if e != nil || s.Control == "down" || s.Tasks[id] == nil || s.Tasks[id].CancelRequested || s.Tasks[id].OperationID != fence.OperationID || s.Tasks[id].Attempt != fence.Attempt {
					cancel()
					return
				}
			}
		}
	}()
	level, checkErr := validateCandidate(ctx, filepath.Join(root, "cells", id), s, row)
	unresolved, markerErr := launch.UnresolvedProcess(filepath.Join(root, "cells", id), row.Attempt)
	if unresolved || markerErr != nil {
		cancel()
		<-done
		return cells.RecordValidation(CellValidation{Fence: fence, CleanupUnresolved: unresolved, cleanupError: markerErr})
	}
	if model.IsUncertain(checkErr) {
		// An explicitly uncertain external validator must not become failed, verified or interrupted.
		cancel()
		<-done
		return cells.RecordValidation(CellValidation{Fence: fence, Err: checkErr})
	}
	cancel()
	<-done
	if checkErr == nil && checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
		if err := graph.CommitTurnExpansionFenced(root, id, level, &fence); err == nil {
			return nil
		} else {
			checkErr = err
		}
	}
	if checkErr == nil && row.Spec.Spawn != nil {
		if err := graph.CommitExpansionFenced(root, id, level, &fence); err == nil {
			return nil
		} else {
			checkErr = err
		}
	}
	return cells.RecordValidation(CellValidation{Fence: fence, Level: level, Err: checkErr})
}
