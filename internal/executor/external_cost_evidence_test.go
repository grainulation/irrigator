package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// externalCostProvider runs the real external() boundary against a controlled
// /bin/sh provider that prints payload verbatim. No model executable is involved.
func externalCostProvider(t *testing.T, payload string) (model.WorkResponse, error) {
	t.Helper()
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	if strings.Contains(payload, "'") {
		t.Fatal("payload must not contain single quotes")
	}
	cell := t.TempDir()
	spec := model.CommandSpec{Command: []string{"/bin/sh", "-c", "cat >/dev/null; printf '%s' '" + payload + "'"}}
	req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "task", RequestID: "rid", OperationID: "op", Task: model.Task{ID: "a"}, TimeoutMS: 10000, MaxCostUSD: 10}
	return External(context.Background(), cell, filepath.Join(cell, "attempt"), spec, req)
}

const externalCostHead = `{"protocol":"swarm.worker/v1","request_id":"rid","output":{"answer":"ok"},"outcome":"settled"`

func TestExternalMicrosOnlyZeroIsKnownZero(t *testing.T) {
	// The exact shape reported by the host overlay.
	response, err := callOutcomeExternal(t, "task", "", `{"protocol":"swarm.worker/v1","request_id":"rid","cost_micros":0,"output":{"answer":"ok"},"outcome":"settled"}`)
	if err != nil {
		t.Fatalf("micros-only zero rejected: %v", err)
	}
	if response.CostUnknown || response.CostBasis != "reported" || response.CostMicros != 0 || response.CostUSD != 0 || !response.CostMicrosExplicit {
		t.Fatalf("explicit zero micros must be a known zero charge: %+v", response)
	}
	if got, e := response.ResolveCost(); e != nil || got != 0 {
		t.Fatalf("resolved %d, %v", got, e)
	}
	if response.Output["answer"] != "ok" {
		t.Fatalf("output lost: %+v", response)
	}
}

func TestExternalMicrosCostMatrix(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		micros    int64
		unknown   bool
		wantError bool
	}{
		{"micros only positive", `,"cost_micros":250000}`, 250000, false, false},
		{"micros only zero", `,"cost_micros":0}`, 0, false, false},
		{"zero micros beats positive usd", `,"cost_micros":0,"cost_usd":1.5}`, 0, false, false},
		{"positive micros beats other usd", `,"cost_micros":250000,"cost_usd":0.01}`, 250000, false, false},
		{"key order irrelevant", `,"cost_usd":1.5,"cost_micros":0}`, 0, false, false},
		{"usd only legacy", `,"cost_usd":0.05}`, 50000, false, false},
		{"usd only zero legacy", `,"cost_usd":0}`, 0, false, false},
		{"null micros falls back to usd", `,"cost_micros":null,"cost_usd":0.05}`, 50000, false, false},
		{"absent cost is unknown", `}`, 0, true, false},
		{"unreadable usd only is unknown", `,"cost_usd":-1}`, 0, true, false},
		{"zero micros cannot vouch for unreadable usd", `,"cost_micros":0,"cost_usd":-1}`, 0, true, false},
		{"positive micros with unreadable usd fails closed", `,"cost_micros":5,"cost_usd":-1}`, 0, false, true},
		{"positive micros with overflow usd fails closed", `,"cost_micros":5,"cost_usd":2000000000}`, 0, false, true},
		{"negative micros", `,"cost_micros":-1}`, 0, false, true},
		{"negative zero micros", `,"cost_micros":-0}`, 0, false, true},
		{"overflow micros", `,"cost_micros":1000000000000001}`, 0, false, true},
		{"fractional micros", `,"cost_micros":1.5}`, 0, false, true},
		{"exponent micros", `,"cost_micros":1e3}`, 0, false, true},
		{"string micros", `,"cost_micros":"0"}`, 0, false, true},
		{"duplicate micros", `,"cost_micros":0,"cost_micros":5}`, 0, false, true},
		{"duplicate usd", `,"cost_usd":0,"cost_usd":5}`, 0, false, true},
		{"aliased micros", `,"COST_MICROS":0}`, 0, false, true},
		{"aliased usd with micros", `,"cost_micros":0,"Cost_USD":1.5}`, 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, err := externalCostProvider(t, externalCostHead+tc.body)
			if tc.wantError {
				if err == nil {
					t.Fatalf("accepted: %+v", response)
				}
				if response.CostMicros != 0 || response.CostUSD != 0 {
					t.Fatalf("a failed response must not carry a charge: %+v", response)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if response.CostUnknown != tc.unknown {
				t.Fatalf("CostUnknown=%t want %t: %+v", response.CostUnknown, tc.unknown, response)
			}
			if tc.unknown {
				if response.CostBasis != "unknown" || response.CostUSD != 0 {
					t.Fatalf("unknown cost must stay zero-valued and unknown-basis: %+v", response)
				}
				return
			}
			if response.CostBasis != "reported" || response.CostMicros != tc.micros || response.CostUSD != cost.MicrosToFloat(tc.micros) {
				t.Fatalf("got %+v, want %d micros reported", response, tc.micros)
			}
		})
	}
}

// The decoder receives a cost-stripped body; everything the caller seeded that is
// not a wire field must survive it.
func TestExternalCostUnmarshalPreservesSeededAccountingFields(t *testing.T) {
	seeded := model.WorkResponse{Usage: &model.TaskUsage{Model: "seed-model"}, Model: "seed-model", CostBasis: "reported", CostUSD: 0.25}
	body := `{"protocol":"swarm.worker/v1","request_id":"rid","cost_micros":250000,"output":{"a":"b"}}`
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&seeded); err != nil {
		t.Fatal(err)
	}
	if seeded.Usage == nil || seeded.Usage.Model != "seed-model" || seeded.Model != "seed-model" || seeded.CostBasis != "reported" || seeded.CostUnknown {
		t.Fatalf("seeded accounting fields lost: %+v", seeded)
	}
	if !seeded.CostMicrosExplicit || seeded.CostMicros != 250000 || seeded.Output["a"] != "b" {
		t.Fatalf("wire fields not applied: %+v", seeded)
	}
	unknown := model.WorkResponse{CostUnknown: true, CostBasis: "unknown"}
	if err := json.Unmarshal([]byte(`{"protocol":"swarm.worker/v1","request_id":"rid"}`), &unknown); err != nil {
		t.Fatal(err)
	}
	if !unknown.CostUnknown || unknown.CostBasis != "unknown" {
		t.Fatalf("seeded unknown marker lost: %+v", unknown)
	}
}

// Interrupted-attempt evidence uses the same strict parser as a normal response.
func TestExternalCostInterruptedEvidenceHonorsExplicitMicros(t *testing.T) {
	req := model.WorkRequest{RequestID: "rid"}
	const head = `{"protocol":"swarm.worker/v1","request_id":"rid",`
	cases := []struct {
		name, raw string
		micros    int64
		known     bool
		invalid   bool
	}{
		{"micros zero", head + `"cost_micros":0}`, 0, true, false},
		{"micros positive", head + `"cost_micros":70000}`, 70000, true, false},
		{"micros beats usd", head + `"cost_micros":70000,"cost_usd":9}`, 70000, true, false},
		{"usd only", head + `"cost_usd":0.07}`, 70000, true, false},
		{"nothing", head + `"output":{}}`, 0, false, false},
		{"truncated", head + `"cost_micros":7`, 0, false, false},
		{"wrong request", `{"protocol":"swarm.worker/v1","request_id":"other","cost_micros":5}`, 0, false, false},
		{"wrong protocol", `{"protocol":"x","request_id":"rid","cost_micros":5}`, 0, false, false},
		{"duplicate", head + `"cost_micros":0,"cost_micros":5}`, 0, false, false},
		{"alias", head + `"COST_MICROS":5}`, 0, false, false},
		{"negative", head + `"cost_micros":-5}`, 0, false, true},
		{"overflow", head + `"cost_micros":1000000000000001}`, 0, false, true},
		{"fractional", head + `"cost_micros":2.5}`, 0, false, true},
		{"zero micros with unreadable usd is unknown", head + `"cost_micros":0,"cost_usd":-1}`, 0, false, false},
	}
	for _, tc := range cases {
		got := provider.ExternalCostEvidence([]byte(tc.raw), req)
		if got.Known != tc.known || got.Invalid != tc.invalid || (got.Known && got.Micros != tc.micros) {
			t.Errorf("%s: %+v, want micros=%d known=%t invalid=%t", tc.name, got, tc.micros, tc.known, tc.invalid)
		}
		usd, known := externalReportedCost([]byte(tc.raw), req)
		if known != tc.known || (known && usd != cost.MicrosToFloat(tc.micros)) {
			t.Errorf("%s: externalReportedCost = %v,%t", tc.name, usd, known)
		}
	}
}

func TestExternalInterruptedAttemptRetainsMicrosEvidence(t *testing.T) {
	script := `printf '{"protocol":"swarm.worker/v1","request_id":"rid","cost_micros":70000}'; sleep 5`
	cell := t.TempDir()
	ctx, cancel := runfixture.OutcomeLeaseContext(t, cell, 800*time.Millisecond)
	defer cancel()
	req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "task", RequestID: "rid", OperationID: "op", Task: model.Task{ID: "a"}, Attempt: 1, TimeoutMS: 800, MaxCostUSD: 10}
	response, err := External(ctx, cell, filepath.Join(cell, "1"), model.CommandSpec{Command: []string{"/bin/sh", "-c", script}}, req)
	if err == nil || !model.IsUncertain(err) {
		t.Fatalf("interrupted attempt must be uncertain: %v", err)
	}
	if response.CostBasis != "reported" || response.CostMicros != 70000 || response.CostUSD != 0.07 || response.CostUnknown {
		t.Fatalf("partial micros evidence lost: %+v", response)
	}
}

func externalCostDigest(req model.WorkRequest) string {
	canonical := req
	canonical.RequestID = ""
	canonical.Attempt = 0
	canonical.LaunchContext = nil
	encoded, _ := json.Marshal(canonical)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func TestExternalCostInvalidCostReplayStaysUnknownAcrossReload(t *testing.T) {
	const reserved = int64(250_000)
	p, _ := pdiagPool(t, "unused", ".1", 5)
	req := runfixture.PdiagReq("op", 0.25)
	hash := externalCostDigest(req)
	p.Ledger.Receipts["op"] = model.ProviderReceipt{Hash: hash, Status: "pending", Reserved: cost.MicrosToFloat(reserved), ReservedMicros: reserved}

	got := p.Finish(req, hash, model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: "op", Output: map[string]string{"answer": "x"}, CostUSD: -3}, true)
	if got.Outcome != model.OutcomeUncertain || !got.CostUnknown || got.CostUSD != 0 || got.Output != nil {
		t.Fatalf("finish: %+v", got)
	}
	if p.Ledger.Receipts["op"].ReservedAmount() != reserved || p.ReservedCost() != reserved {
		t.Fatalf("reservation shrank or grew: %+v", p.Ledger.Receipts["op"])
	}

	replayWire := func(t *testing.T, pool *provider.ProviderPool) map[string]json.RawMessage {
		t.Helper()
		replay := pool.Work(context.Background(), req)
		if replay.Outcome != model.OutcomeUncertain || !replay.CostUnknown || replay.CostUSD != 0 || replay.Output != nil {
			t.Fatalf("replay must stay uncertain with unknown zero-valued cost: %+v", replay)
		}
		raw, err := json.Marshal(replay)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"cost_usd", "cost_micros"} {
			if _, has := object[field]; has {
				t.Fatalf("replay serialized %s: %s", field, raw)
			}
		}
		return object
	}
	replayWire(t, p)

	// Persist and reload through the real ledger encoding, then replay again.
	ledgerRaw, err := json.Marshal(p.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]map[string]json.RawMessage
	if err := json.Unmarshal(ledgerRaw, &persisted); err != nil {
		t.Fatal(err)
	}
	var storedReceipt map[string]json.RawMessage
	if err := json.Unmarshal(persisted["receipts"]["op"], &storedReceipt); err != nil {
		t.Fatal(err)
	}
	if string(storedReceipt["cost_unknown"]) != "true" {
		t.Fatalf("receipt does not durably record unknown billing: %s", persisted["receipts"]["op"])
	}
	var storedResponse map[string]json.RawMessage
	if err := json.Unmarshal(storedReceipt["response"], &storedResponse); err != nil {
		t.Fatal(err)
	}
	if _, has := storedResponse["cost_usd"]; has {
		t.Fatalf("ledger persisted a cost for unknown billing: %s", storedReceipt["response"])
	}
	var reloaded observer.ProviderLedger
	if err := json.Unmarshal(ledgerRaw, &reloaded); err != nil {
		t.Fatal(err)
	}
	p2 := &provider.ProviderPool{Opts: p.Opts, Ledger: reloaded}
	p2.RestoreAdmissionBarrier()
	if r := p2.Ledger.Receipts["op"]; r.Status != "uncertain" || r.ReservedAmount() != reserved || !r.CostUnknown {
		t.Fatalf("recovery changed the receipt: %+v", r)
	}
	if p2.ReservedCost() != reserved {
		t.Fatalf("reloaded reservation = %d, want %d", p2.ReservedCost(), reserved)
	}
	replayWire(t, p2)
}

func TestExternalCostLegacyLedgerWithoutUnknownFlagStillLoads(t *testing.T) {
	raw := `{"receipts":{"op":{"hash":"` + strings.Repeat("a", 64) + `","status":"uncertain","reserved_usd":0.25,"response":{"protocol":"swarm.worker/v1","request_id":"r","cost_usd":0.25,"outcome":"uncertain","error":"x"}}}}`
	var ledger observer.ProviderLedger
	if err := json.Unmarshal([]byte(raw), &ledger); err != nil {
		t.Fatal(err)
	}
	r := ledger.Receipts["op"]
	if r.CostUnknown || r.Response.CostUnknown || r.ReservedAmount() != 250_000 {
		t.Fatalf("old receipt changed meaning: %+v", r)
	}
	if m, err := r.Response.ResolveCost(); err != nil || m != 250_000 {
		t.Fatalf("old reported cost = %d, %v", m, err)
	}
}

func TestExternalCostStartupSanitizesInvalidUncertainCostWithoutShrinkingReservation(t *testing.T) {
	const reserved = int64(300_000)
	p, _, hash := poolWithReservation(t, reserved)
	var bad model.WorkResponse
	if err := json.Unmarshal([]byte(`{"protocol":"swarm.worker/v1","request_id":"r","cost_micros":-9,"output":{"ok":"secret"}}`), &bad); err != nil {
		t.Fatal(err)
	}
	p.Ledger.Receipts["op"] = model.ProviderReceipt{Hash: hash, Status: "uncertain", Reserved: cost.MicrosToFloat(reserved), ReservedMicros: reserved, Response: bad}
	p.RestoreAdmissionBarrier()
	r := p.Ledger.Receipts["op"]
	if r.Status != "uncertain" || r.ReservedAmount() != reserved || p.ReservedCost() != reserved {
		t.Fatalf("reservation changed or receipt completed: %+v", r)
	}
	if !r.CostUnknown || !r.Response.CostUnknown || r.Response.CostMicros != 0 || r.Response.CostUSD != 0 || r.Response.Output != nil {
		t.Fatalf("invalid cost not sanitized to unknown: %+v", r)
	}
	if m, err := r.Response.ResolveCost(); err != nil || m != 0 {
		t.Fatalf("sanitized response reports %d, %v", m, err)
	}
}

func TestExternalCostValidFloorStillSettlesNormally(t *testing.T) {
	// A genuinely observed valid charge is untouched by the unknown-cost path.
	p, req, hash := poolWithReservation(t, 250_000)
	got := p.Finish(req, hash, model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: "r", Output: map[string]string{"ok": "yes"}, CostMicros: 100_000}, true)
	r := p.Ledger.Receipts["op"]
	if got.CostUnknown || r.CostUnknown || r.Status != "completed" || r.ReservedAmount() != 100_000 {
		t.Fatalf("valid charge mishandled: %+v / %+v", got, r)
	}
	wire, _ := json.Marshal(got)
	if !strings.Contains(string(wire), `"cost_micros":100000`) {
		t.Fatalf("known cost missing from wire: %s", wire)
	}
}

func externalCostPaidState(backend string, schema int) *model.State {
	t := runfixture.Task("cell", 0)
	s := &model.State{SchemaVersion: schema, Version: 1, RunID: "phase5-run", Plan: model.Plan{Tasks: []model.Task{t}}, Backend: backend, Control: "run", Tasks: map[string]*model.TaskState{}}
	s.Tasks[t.ID] = s.NewTask(t)
	return s
}

func externalCostBegin(s *model.State) (AdmittedCellAttempt, error) {
	return beginCellAttempt(s, "cell", s.Tasks["cell"].OperationID, 123, time.Unix(1, 0))
}

func TestBeginCellAttemptRefusesUnconsentedPaidStatesWithoutMutation(t *testing.T) {
	for _, backend := range []string{"haiku", "custom-provider"} {
		for _, schema := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/schema%d", backend, schema), func(t *testing.T) {
				s := externalCostPaidState(backend, schema)
				if backend != "haiku" {
					s.Plan.Providers = map[string]model.CommandSpec{backend: {Command: []string{"/nonexistent/never-run"}}}
				}
				before := cellBytes(t, s)
				_, err := externalCostBegin(s)
				if !errors.Is(err, ErrLegacyUnbudgeted) {
					t.Fatalf("paid unconsented admission = %v, want errLegacyUnbudgeted", err)
				}
				if cellBytes(t, s) != before {
					t.Fatal("refused admission mutated attempts, events or reservations")
				}
				if row := s.Tasks["cell"]; row.Attempt != 0 || row.Status != "pending" {
					t.Fatalf("refused admission advanced the row: %+v", row)
				}
			})
		}
	}
}

// Eligibility fences keep their errNoWork meaning and run before the consent gate.
func TestBeginCellAttemptKeepsEligibilityFencesBeforeConsentGate(t *testing.T) {
	for _, name := range []string{"paused", "cancelled", "accepted", "stale-operation", "absent-row"} {
		t.Run(name, func(t *testing.T) {
			s := externalCostPaidState("haiku", 2)
			id, expected := "cell", s.Tasks["cell"].OperationID
			switch name {
			case "paused":
				s.Control = "pause"
			case "cancelled":
				s.Tasks["cell"].CancelRequested = true
			case "accepted":
				s.Tasks["cell"].Status = "verified"
			case "stale-operation":
				expected = "old"
			case "absent-row":
				id = "nope"
			}
			before := cellBytes(t, s)
			if _, err := beginCellAttempt(s, id, expected, 123, time.Unix(1, 0)); !errors.Is(err, launch.ErrNoWork) {
				t.Fatalf("got %v, want errNoWork", err)
			}
			if cellBytes(t, s) != before {
				t.Fatal("rejected admission mutated state")
			}
		})
	}
}

func TestBeginCellAttemptAdmitsConsentedOrFreeRuns(t *testing.T) {
	cases := map[string]func(s *model.State){
		"recorded allow": func(s *model.State) { s.UnbudgetedDecision = checkpoint.UnbudgetedAllow },
		"new run marker": func(s *model.State) { s.UnbudgetedDecision = checkpoint.UnbudgetedInitial },
		"positive call limit": func(s *model.State) {
			model.SaveCallLimits(s, model.Options{Workers: 1, Timeout: time.Second, Budget: 0.25})
		},
	}
	for name, setup := range cases {
		for _, schema := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s/schema%d", name, schema), func(t *testing.T) {
				s := externalCostPaidState("haiku", schema)
				setup(s)
				a, err := externalCostBegin(s)
				if err != nil {
					t.Fatalf("consented admission refused: %v", err)
				}
				if a.Fence.Attempt != 1 || s.Tasks["cell"].Status != "running" {
					t.Fatalf("attempt not admitted: %+v", a.Fence)
				}
			})
		}
	}
	t.Run("zero call limit is not consent", func(t *testing.T) {
		s := externalCostPaidState("haiku", 2)
		model.SaveCallLimits(s, model.Options{Workers: 1, Timeout: time.Second, Budget: 0})
		if _, err := externalCostBegin(s); !errors.Is(err, ErrLegacyUnbudgeted) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("truly mock run needs no decision", func(t *testing.T) {
		s := externalCostPaidState("mock", 2)
		if _, err := externalCostBegin(s); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("mock backend with a paid task provider is gated", func(t *testing.T) {
		s := externalCostPaidState("mock", 2)
		s.Tasks["cell"].Spec.Provider = "haiku"
		s.Plan.Tasks[0].Provider = "haiku"
		if _, err := externalCostBegin(s); !errors.Is(err, ErrLegacyUnbudgeted) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestBeginCellAttemptAdmitsManagedRun(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "claude_cli", "0.25", 1)
	s := runfixture.Read(t, root)
	if UnbudgetedGateApplies(s) {
		t.Fatal("managed runs are governed by profile budgets, not the legacy gate")
	}
	a, err := beginCellAttempt(s, "a", s.Tasks["a"].OperationID, 123, time.Unix(1, 0))
	if err != nil || a.Fence.Attempt != 1 {
		t.Fatalf("managed admission = %v %+v", err, a.Fence)
	}
}
