package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

// Unmanaged (legacy-plan) checkpoints cannot carry managed CostUnknown/reservation fields. Keep
// missing invoice evidence in append-only operation events instead; explicit
// retry or result resolution must not erase uncertainty from an earlier call.
func RecordLegacyCostUnknown(s *model.State, row *model.TaskState, now time.Time) {
	for _, event := range s.Events {
		if event.Kind == "legacy_cost_unknown" {
			if detail, ok := event.Detail.(map[string]any); ok && detail["operation_id"] == row.OperationID && fmt.Sprint(detail["attempt"]) == fmt.Sprint(row.Attempt) {
				return
			}
		}
	}
	s.Events = append(s.Events, model.Event{Time: now, Kind: "legacy_cost_unknown", Task: row.Spec.ID, Detail: map[string]any{"operation_id": row.OperationID, "attempt": row.Attempt, "basis": "external_billing_unresolved"}})
}

func LegacyUnknownOperations(s *model.State) int {
	operations := map[string]bool{}
	for _, event := range s.Events {
		if event.Kind == "legacy_cost_unknown" {
			if detail, ok := event.Detail.(map[string]any); ok {
				if id, ok := detail["operation_id"].(string); ok && id != "" {
					operations[id] = true
				}
			}
		}
	}
	return len(operations)
}

// Billing evidence must be an unambiguous object. encoding/json alone accepts
// duplicate keys and case-insensitive aliases, which could erase a charge.
func StrictCostObject(raw []byte, canonical ...string) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := jsonio.ParseStrictJSON(dec, 0)
	if err != nil || !utf8.Valid(raw) || dec.Decode(new(any)) != io.EOF {
		return nil, false
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	for key := range object {
		for _, name := range canonical {
			if key != name && strings.EqualFold(key, name) {
				return nil, false
			}
		}
	}
	return object, true
}

// reportedIntegerMicros accepts only a plain non-negative decimal integer within
// the supported range for a raw cost_micros number. Fractions, exponents, signs
// (including "-0"), strings and overflow are all invalid: never coerced.
func reportedIntegerMicros(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	text := number.String()
	if text == "" || len(text) > 19 {
		return 0, false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	micros, err := strconv.ParseInt(text, 10, 64)
	if err != nil || micros > cost.MaxMicros {
		return 0, false
	}
	return micros, true
}
func HaikuReportedCost(raw []byte) (float64, bool) {
	object, ok := StrictCostObject(raw, "total_cost_usd")
	if !ok {
		return 0, false
	}
	return managed.FiniteReportedCost(object["total_cost_usd"])
}

// externalCostEvidence is what a command provider's raw response proves about
// its charge.
//   - known: a trustworthy amount (micros) was reported. An explicit cost_micros,
//     including zero, is authoritative over cost_usd; otherwise cost_usd is used.
//   - neither known nor invalid: billing is simply unavailable (no cost fields,
//     USD-only telemetry that is unreadable, an unattributable response). Command
//     providers that omit cost have this optional-telemetry meaning.
//   - invalid: cost_micros is present but malformed, negative or out of range, or
//     it accompanies an unreadable cost_usd while claiming a positive charge. That
//     is a corrupt billing statement and callers must fail closed.
type externalCostEvidenceResult struct {
	Micros   int64
	Known    bool
	Explicit bool
	Invalid  bool
}

func ExternalCostEvidence(raw []byte, req model.WorkRequest) externalCostEvidenceResult {
	object, ok := StrictCostObject(raw, "protocol", "request_id", "cost_usd", "cost_micros")
	if !ok || object["protocol"] != model.ProtocolVersion || object["request_id"] != req.RequestID {
		return externalCostEvidenceResult{}
	}
	if raw, present := object["cost_micros"]; present && raw != nil {
		micros, valid := reportedIntegerMicros(raw)
		if !valid {
			return externalCostEvidenceResult{Invalid: true}
		}
		if usd, hasUSD := object["cost_usd"]; hasUSD && usd != nil {
			if _, usdValid := managed.ReportedUSDMicros(usd); !usdValid {
				if micros == 0 {
					// Zero cannot vouch for a statement whose USD field is unreadable.
					return externalCostEvidenceResult{}
				}
				return externalCostEvidenceResult{Invalid: true}
			}
		}
		return externalCostEvidenceResult{Micros: micros, Known: true, Explicit: true}
	}
	micros, ok := managed.ReportedUSDMicros(object["cost_usd"])
	if !ok {
		return externalCostEvidenceResult{}
	}
	return externalCostEvidenceResult{Micros: micros, Known: true}
}
