package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/grainulation/irrigator/internal/cost"
)

const ProtocolVersion = "swarm.worker/v1"
const MaxInputBytes = 1024 * 1024
const MaxOutputBytes = 4 * 1024 * 1024

type WorkRequest struct {
	Boundaries    *WorkerBoundaries            `json:"execution_boundaries,omitempty"`
	TrackCost     bool                         `json:"track_cost,omitempty"`
	LaunchContext *WorkerLaunchContext         `json:"launch_context,omitempty"`
	RunID         string                       `json:"run_id,omitempty"`
	Protocol      string                       `json:"protocol"`
	Kind          string                       `json:"kind"`
	RequestID     string                       `json:"request_id"`
	OperationID   string                       `json:"operation_id"`
	Task          Task                         `json:"task"`
	Attempt       int                          `json:"attempt"`
	Context       map[string]string            `json:"context,omitempty"`
	Dependencies  map[string]map[string]string `json:"dependencies,omitempty"`
	Candidate     map[string]string            `json:"candidate,omitempty"`
	TimeoutMS     int64                        `json:"timeout_ms"`
	MaxCostUSD    float64                      `json:"max_cost_usd"`
}
type WorkResponse struct {
	CostUnknown    bool              `json:"-"` // Internal accounting; never accepted from wire responses.
	CostBasis      string            `json:"-"`
	Usage          *TaskUsage        `json:"-"`
	Model          string            `json:"-"`
	ContextReceipt *ContextReceipt   `json:"context_receipt,omitempty"`
	Protocol       string            `json:"protocol"`
	RequestID      string            `json:"request_id"`
	Output         map[string]string `json:"output,omitempty"`
	Passed         *bool             `json:"passed,omitempty"`
	Error          string            `json:"error,omitempty"`
	CostUSD        float64           `json:"cost_usd"`
	CostMicros     int64             `json:"cost_micros,omitempty"`
	// CostMicrosExplicit records that cost_micros was present in the JSON this
	// response was decoded from (or must be persisted as present). An explicit
	// cost_micros, including zero, is authoritative over cost_usd. The default
	// false applies the sentinel rule: zero micros means "not supplied",
	// so an in-memory response never zeroes a valid USD amount by default.
	CostMicrosExplicit bool `json:"-"`
	// Outcome is OPTIONAL typed certainty (see outcome_recovery.go): "not_started"
	// (nothing dispatched; known failure), "settled" (provider completed; Error is a
	// known failure) or "uncertain" (dispatch may have happened; never replay
	// automatically). Absent means unclassified; any other value fails closed as uncertain.
	Outcome string `json:"outcome,omitempty"`
	// DiagnosticCode is an OPTIONAL stable typed failure code (see DiagProvider*).
	// Absent means unknown. Only known codes are propagated; raw provider text never is.
	DiagnosticCode string `json:"diagnostic_code,omitempty"`
}

// UnmarshalJSON decodes a response while remembering whether cost_micros was
// present, so an explicit zero stays authoritative over cost_usd. It rejects
// top-level duplicate keys and non-canonical spellings of the cost fields itself:
// encoding/json would otherwise accept both, and an ambiguous billing field must
// never be resolved to the smaller amount. Absent or null cost_micros means
// not supplied. Fields already set on the receiver keep their
// values when the JSON omits them, exactly as with the default decoder.
func (r *WorkResponse) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	present, err := workResponseCostMicrosPresent(data)
	if err != nil {
		return err
	}
	type plain WorkResponse // no methods: avoids recursion
	decoded := plain(*r)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = WorkResponse(decoded)
	r.CostMicrosExplicit = present
	return nil
}

// workResponseCostMicrosPresent scans the top-level object once, enforcing
// unambiguous keys, and reports whether a non-null cost_micros is present.
func workResponseCostMicrosPresent(data []byte) (bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return false, errors.New("work response must be a JSON object")
	}
	seen := map[string]bool{}
	present := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return false, err
		}
		key, _ := keyTok.(string)
		if seen[key] {
			return false, fmt.Errorf("work response has duplicate field %q", key)
		}
		seen[key] = true
		for _, canonical := range []string{"cost_micros", "cost_usd"} {
			if key != canonical && strings.EqualFold(key, canonical) {
				return false, fmt.Errorf("work response has noncanonical cost field %q", key)
			}
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return false, err
		}
		if key == "cost_micros" && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			present = true
		}
	}
	if _, err := dec.Token(); err != nil {
		return false, err
	}
	return present, nil
}

// MarshalJSON keeps an explicit zero cost_micros durable. Without it omitempty
// would drop the zero and a persisted response would round-trip into the legacy
// "not supplied" sentinel, letting a stale positive cost_usd charge again. An
// explicit zero is authoritative, so cost_usd is persisted as zero with it.
// Every other response marshals byte-for-byte as before.
//
// A response whose billing is unknown (CostUnknown) states NO cost at all: cost_usd
// and cost_micros are omitted. Serializing unknown as `"cost_usd":0` would be read
// by a legacy client as a known zero invoice, and a held reservation must never be
// published as a charge. Receivers already treat absent cost as unknown.
func (r WorkResponse) MarshalJSON() ([]byte, error) {
	type plain WorkResponse // no methods: avoids recursion
	p := plain(r)
	if r.CostUnknown {
		p.CostMicros = 0
		return json.Marshal(struct {
			plain
			CostUSD *float64 `json:"cost_usd,omitempty"`
		}{plain: p})
	}
	if !r.CostMicrosExplicit || r.CostMicros != 0 {
		return json.Marshal(p)
	}
	p.CostUSD = 0
	zero := int64(0)
	return json.Marshal(struct {
		plain
		CostMicros *int64 `json:"cost_micros"`
	}{plain: p, CostMicros: &zero})
}

// resolveCost is the authoritative reported cost in micros. A present cost_micros
// (including zero) wins; otherwise nonzero micros win, then cost_usd. Invalid,
// negative or overflowing values fail closed: callers must not substitute zero.
func (r WorkResponse) ResolveCost() (int64, error) {
	return cost.ResolveCostExplicit(r.CostMicros, r.CostMicrosExplicit, r.CostUSD)
}

// Stable typed diagnostic codes. Fixed strings only: never derived from cost and never from raw provider text.
const (
	// DiagProviderSessionBudget: the CLI result subtype reported the cumulative CLI session cap was spent (distinct from the per-operation reservation).
	DiagProviderSessionBudget = "provider_session_budget_exhausted"
	// DiagProviderOperationBudget: a turn cost more than its own per-operation reservation.
	DiagProviderOperationBudget = "provider_operation_budget_exceeded"
	// DiagProviderError: the CLI reported an error result with an unknown or absent subtype.
	DiagProviderError            = "provider_error"
	DiagProviderAuth             = "provider_authentication_required"
	DiagProviderStructuredOutput = "provider_structured_output_retries_exhausted"
)

// knownDiagnosticCode returns v only when it is an exact known code, else "". The input is never echoed.
func KnownDiagnosticCode(v string) string {
	switch v {
	case DiagProviderSessionBudget, DiagProviderOperationBudget, DiagProviderError, DiagProviderAuth, DiagProviderStructuredOutput:
		return v
	}
	return ""
}

// providerResultFailure: is_error, or any CLI result subtype that starts with "error", is a failure.
func ProviderResultFailure(isError bool, subtype string) bool {
	return isError || strings.HasPrefix(subtype, "error")
}

// providerResultDiagnostic maps the CLI result subtype to a stable code and a fixed, never-empty message.
// The subtype and the raw result text are inspected or ignored, never echoed.
func ProviderResultDiagnostic(subtype string) (code, message string) {
	switch subtype {
	case "error_max_budget_usd":
		return DiagProviderSessionBudget, "provider returned error: CLI session budget cap exhausted (error_max_budget_usd)"
	case "error_max_structured_output_retries":
		return DiagProviderStructuredOutput, "provider could not produce schema-valid output within its retry limit; inspect the output contract before an explicit retry"
	case "error_max_turns":
		return DiagProviderError, "provider returned error: CLI max turns reached (error_max_turns)"
	case "error_during_execution":
		return DiagProviderError, "provider returned error: CLI error during execution (error_during_execution)"
	case "":
		return DiagProviderError, "provider returned error: no result subtype or text was reported"
	}
	return DiagProviderError, "provider returned error: result subtype not recognized"
}
