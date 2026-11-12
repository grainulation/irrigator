package executor

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestWorkResponseWireCostPrecedence(t *testing.T) {
	const head = `{"protocol":"swarm.worker/v1","request_id":"r",`
	cases := []struct {
		name     string
		body     string
		want     int64
		explicit bool
	}{
		{"explicit zero beats positive usd", `"cost_micros":0,"cost_usd":1.5}`, 0, true},
		{"explicit zero alone", `"cost_micros":0}`, 0, true},
		{"explicit zero with zero usd", `"cost_micros":0,"cost_usd":0}`, 0, true},
		{"usd only legacy wire", `"cost_usd":1.5}`, 1_500_000, false},
		{"absent both", `"output":{"a":"b"}}`, 0, false},
		{"null micros is absent", `"cost_micros":null,"cost_usd":1.5}`, 1_500_000, false},
		{"positive micros beats larger usd", `"cost_micros":250000,"cost_usd":1.5}`, 250_000, true},
		{"positive micros alone", `"cost_micros":250000}`, 250_000, true},
		{"positive micros beats zero usd", `"cost_micros":250000,"cost_usd":0}`, 250_000, true},
		{"key order is irrelevant", `"cost_usd":1.5,"cost_micros":0}`, 0, true},
	}
	for _, c := range cases {
		var response model.WorkResponse
		if err := json.Unmarshal([]byte(head+c.body), &response); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if response.CostMicrosExplicit != c.explicit {
			t.Errorf("%s: explicit = %t, want %t", c.name, response.CostMicrosExplicit, c.explicit)
		}
		got, err := response.ResolveCost()
		if err != nil || got != c.want {
			t.Errorf("%s: resolved %d, %v; want %d", c.name, got, err, c.want)
		}
	}

	invalid := []struct{ name, body string }{
		{"negative micros", `"cost_micros":-1}`},
		{"overflow micros", `"cost_micros":1000000000000001}`},
		{"explicit zero with negative usd", `"cost_micros":0,"cost_usd":-1}`},
		{"explicit micros with overflow usd", `"cost_micros":5,"cost_usd":2000000000}`},
	}
	for _, c := range invalid {
		var response model.WorkResponse
		if err := json.Unmarshal([]byte(head+c.body), &response); err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		if got, err := response.ResolveCost(); err == nil {
			t.Errorf("%s: resolved %d, want fail-closed error", c.name, got)
		}
	}

	// Malformed, duplicated or aliased billing fields never decode at all.
	for name, body := range map[string]string{
		"fractional micros":     `"cost_micros":1.5}`,
		"string micros":         `"cost_micros":"0"}`,
		"duplicate micros":      `"cost_micros":0,"cost_micros":5}`,
		"duplicate usd":         `"cost_usd":0,"cost_usd":5}`,
		"aliased micros":        `"COST_MICROS":0,"cost_usd":1.5}`,
		"aliased usd":           `"cost_micros":5,"Cost_USD":1.5}`,
		"duplicate other field": `"error":"a","error":"b"}`,
	} {
		var response model.WorkResponse
		if err := json.Unmarshal([]byte(head+body), &response); err == nil {
			t.Errorf("%s: decoded without error", name)
		}
	}
	var notObject model.WorkResponse
	if err := json.Unmarshal([]byte(`[1]`), &notObject); err == nil {
		t.Error("non-object response decoded without error")
	}
}

func TestWorkResponseExplicitZeroSurvivesPersistence(t *testing.T) {
	var decoded model.WorkResponse
	if err := json.Unmarshal([]byte(`{"protocol":"swarm.worker/v1","request_id":"r","cost_micros":0,"cost_usd":1.5}`), &decoded); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cost_micros":0`) || !strings.Contains(string(raw), `"cost_usd":0`) {
		t.Fatalf("explicit zero not persisted authoritatively: %s", raw)
	}
	var reread model.WorkResponse
	if err := json.Unmarshal(raw, &reread); err != nil {
		t.Fatal(err)
	}
	if got, err := reread.ResolveCost(); err != nil || got != 0 || !reread.CostMicrosExplicit {
		t.Fatalf("round trip lost explicit zero: %d %v explicit=%t", got, err, reread.CostMicrosExplicit)
	}

	// Even an unnormalized in-memory explicit zero persists as zero, not as stale USD.
	stale := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: "r", CostUSD: 1.5, CostMicrosExplicit: true}
	raw, err = json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	var back model.WorkResponse
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got, err := back.ResolveCost(); err != nil || got != 0 {
		t.Fatalf("stale USD resurrected after round trip: %d %v", got, err)
	}

	// Provider receipts (the durable ledger form) keep it too.
	receipt := model.ProviderReceipt{Hash: strings.Repeat("a", 64), Status: "completed", Response: decoded}
	raw, err = json.Marshal(map[string]model.ProviderReceipt{"op": receipt})
	if err != nil {
		t.Fatal(err)
	}
	var ledger map[string]model.ProviderReceipt
	if err := json.Unmarshal(raw, &ledger); err != nil {
		t.Fatal(err)
	}
	if got, err := ledger["op"].Response.ResolveCost(); err != nil || got != 0 {
		t.Fatalf("receipt round trip charged %d, %v; want 0", got, err)
	}

	// Defaults are unchanged: an in-memory zero never overrides a valid USD amount,
	// legacy responses marshal byte-for-byte as before, and positive micros persist.
	legacy := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: "r", CostUSD: 1.5}
	if got, err := legacy.ResolveCost(); err != nil || got != 1_500_000 {
		t.Fatalf("default zero overrode USD: %d %v", got, err)
	}
	raw, _ = json.Marshal(legacy)
	if strings.Contains(string(raw), "cost_micros") {
		t.Fatalf("legacy marshal grew a cost_micros field: %s", raw)
	}
	plain, _ := json.Marshal(model.WorkResponse{Protocol: "p"})
	if string(plain) != `{"protocol":"p","request_id":"","cost_usd":0}` {
		t.Fatalf("zero-value response marshal bytes changed: %s", plain)
	}
	raw, _ = json.Marshal(model.WorkResponse{Protocol: model.ProtocolVersion, CostMicros: 250000, CostUSD: 0.25})
	if !strings.Contains(string(raw), `"cost_micros":250000`) {
		t.Fatalf("positive micros not persisted: %s", raw)
	}
	if got := runfixture.CostAmount(model.WorkResponse{CostUSD: 0.05}); got != 50000 {
		t.Fatalf("costAmount = %d", got)
	}
}

// The real command-provider boundary, with the strict whole-response checks in
// place before and after the presence-aware decoder.
func TestExternalProviderCostBoundary(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	runProvider := func(t *testing.T, body string) (model.WorkResponse, error) {
		t.Helper()
		cell := t.TempDir()
		payload := `{"protocol":"swarm.worker/v1","request_id":"req-1","output":{"ok":"yes"},` + body + `}`
		spec := model.CommandSpec{Command: []string{"/bin/sh", "-c", "cat >/dev/null; printf '%s' '" + payload + "'"}}
		req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "task", RequestID: "req-1", Task: model.Task{ID: "build", Requires: []string{"ok"}}, TimeoutMS: 10000, MaxCostUSD: 10}
		return External(context.Background(), cell, filepath.Join(cell, "attempt"), spec, req)
	}

	t.Run("explicit zero micros overrides positive usd", func(t *testing.T) {
		r, err := runProvider(t, `"cost_micros":0,"cost_usd":1.5`)
		if err != nil || r.CostMicros != 0 || r.CostUSD != 0 || r.CostUnknown {
			t.Fatalf("got %+v, %v; want a known zero charge", r, err)
		}
		if got, e := r.ResolveCost(); e != nil || got != 0 {
			t.Fatalf("normalized response charges %d, %v", got, e)
		}
		raw, _ := json.Marshal(r)
		var persisted model.WorkResponse
		if err := json.Unmarshal(raw, &persisted); err != nil {
			t.Fatal(err)
		}
		if got, e := persisted.ResolveCost(); e != nil || got != 0 {
			t.Fatalf("persisted response charges %d, %v", got, e)
		}
	})
	t.Run("positive micros wins", func(t *testing.T) {
		r, err := runProvider(t, `"cost_micros":250000,"cost_usd":1.5`)
		if err != nil || r.CostMicros != 250000 || r.CostUSD != 0.25 {
			t.Fatalf("got %+v, %v", r, err)
		}
	})
	t.Run("usd only wire response keeps old meaning", func(t *testing.T) {
		r, err := runProvider(t, `"cost_usd":0.05`)
		if err != nil || r.CostMicros != 50000 || r.CostUSD != 0.05 || r.CostUnknown {
			t.Fatalf("got %+v, %v", r, err)
		}
	})
	t.Run("absent cost stays unknown, never a known zero", func(t *testing.T) {
		r, err := runProvider(t, `"error":""`)
		if err != nil || !r.CostUnknown || r.CostBasis != "unknown" {
			t.Fatalf("got %+v, %v", r, err)
		}
	})
	t.Run("explicit zero cannot make unreadable usd known", func(t *testing.T) {
		r, err := runProvider(t, `"cost_micros":0,"cost_usd":-1`)
		if err != nil || !r.CostUnknown || r.CostBasis != "unknown" {
			t.Fatalf("got %+v, %v", r, err)
		}
	})
	for name, body := range map[string]string{
		"negative micros":   `"cost_micros":-5,"cost_usd":0.1`,
		"overflow micros":   `"cost_micros":1000000000000001,"cost_usd":0.1`,
		"fractional micros": `"cost_micros":1.5,"cost_usd":0.1`,
		"duplicate micros":  `"cost_micros":0,"cost_micros":5,"cost_usd":0.1`,
		"aliased micros":    `"COST_MICROS":0,"cost_usd":1.5`,
	} {
		t.Run("fails closed: "+name, func(t *testing.T) {
			r, err := runProvider(t, body)
			if err == nil {
				t.Fatalf("invalid cost accepted: %+v", r)
			}
			if r.CostMicros != 0 {
				t.Fatalf("failed response still carries a charge: %+v", r)
			}
		})
	}
}

func poolWithReservation(t *testing.T, reserved int64) (*provider.ProviderPool, model.WorkRequest, string) {
	t.Helper()
	hash := strings.Repeat("a", 64)
	p := &provider.ProviderPool{Opts: provider.ProviderPoolOptions{Dir: t.TempDir(), Budget: 1}, Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}}
	p.Ledger.Receipts["op"] = model.ProviderReceipt{Hash: hash, Status: "pending", Reserved: cost.MicrosToFloat(reserved), ReservedMicros: reserved}
	return p, model.WorkRequest{OperationID: "op", RequestID: "r"}, hash
}

func TestProviderReceiptInvalidCostKeepsReservationAndIsUncertain(t *testing.T) {
	const reserved = int64(250_000)
	for name, response := range map[string]model.WorkResponse{
		"negative micros":      {Protocol: model.ProtocolVersion, RequestID: "r", Output: map[string]string{"ok": "yes"}, CostMicros: -7},
		"overflow micros":      {Protocol: model.ProtocolVersion, RequestID: "r", Output: map[string]string{"ok": "yes"}, CostMicros: cost.MaxMicros + 1},
		"negative usd":         {Protocol: model.ProtocolVersion, RequestID: "r", Output: map[string]string{"ok": "yes"}, CostUSD: -1},
		"explicit zero + NaN":  {Protocol: model.ProtocolVersion, RequestID: "r", Output: map[string]string{"ok": "yes"}, CostMicrosExplicit: true, CostUSD: math.NaN()},
		"explicit zero + -USD": {Protocol: model.ProtocolVersion, RequestID: "r", Output: map[string]string{"ok": "yes"}, CostMicrosExplicit: true, CostUSD: -2},
	} {
		t.Run(name, func(t *testing.T) {
			p, req, hash := poolWithReservation(t, reserved)
			got := p.Finish(req, hash, response, true)
			receipt := p.Ledger.Receipts["op"]
			if receipt.Status != "uncertain" {
				t.Fatalf("status = %q, want uncertain (never completed)", receipt.Status)
			}
			if receipt.ReservedAmount() != reserved {
				t.Fatalf("reservation = %d, want the full %d retained", receipt.ReservedAmount(), reserved)
			}
			if got.Outcome != model.OutcomeUncertain || got.Error == "" || got.Output != nil {
				t.Fatalf("response must be uncertain with an error and no output: %+v", got)
			}
			// The held reservation is accounting only. It is NOT a reported charge:
			// the response states unknown billing with a zero amount.
			if !got.CostUnknown || got.CostUSD != 0 || got.CostMicros != 0 {
				t.Fatalf("response must be unknown-cost with zero amount, got %+v", got)
			}
			if m, err := got.ResolveCost(); err != nil || m != 0 {
				t.Fatalf("returned response reports %d, %v; the reservation must not become spend", m, err)
			}
			if p.ReservedCost() != reserved {
				t.Fatalf("pool reserved total = %d, want %d", p.ReservedCost(), reserved)
			}
			if !receipt.CostUnknown || !receipt.Response.CostUnknown || receipt.Response.CostUSD != 0 {
				t.Fatalf("receipt must durably record unknown billing: %+v", receipt)
			}
			// The wire object states no cost at all (never a known zero invoice,
			// never the reservation).
			wire, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(wire, &object); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"cost_usd", "cost_micros", "output"} {
				if _, has := object[field]; has {
					t.Fatalf("%s serialized for an unknown-cost response: %s", field, wire)
				}
			}
			// Reload the ledger: unknown stays unknown, the reservation stays full.
			ledgerRaw, err := json.Marshal(p.Ledger)
			if err != nil {
				t.Fatal(err)
			}
			var reloaded observer.ProviderLedger
			if err := json.Unmarshal(ledgerRaw, &reloaded); err != nil {
				t.Fatal(err)
			}
			back := reloaded.Receipts["op"]
			if back.Status != "uncertain" || back.ReservedAmount() != reserved || !back.CostUnknown || !back.Response.CostUnknown {
				t.Fatalf("reloaded receipt lost unknown billing or shrank the reservation: %+v", back)
			}
			if m, err := back.Response.ResolveCost(); err != nil || m != 0 {
				t.Fatalf("reloaded receipt reports %d, %v", m, err)
			}
		})
	}
}

func TestProviderReceiptExplicitZeroSettlesToZero(t *testing.T) {
	p, req, hash := poolWithReservation(t, 250_000)
	response := model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: "r", Output: map[string]string{"ok": "yes"}, CostMicrosExplicit: true, CostUSD: 1.5}
	got := p.Finish(req, hash, response, true)
	receipt := p.Ledger.Receipts["op"]
	if receipt.Status != "completed" || receipt.ReservedAmount() != 0 || got.Outcome != model.OutcomeSettled || got.Error != "" {
		t.Fatalf("explicit zero did not settle to zero: %+v / %+v", receipt, got)
	}
	// The completed receipt validates after a ledger round trip (the startup rule).
	raw, err := json.Marshal(p.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded observer.ProviderLedger
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	r := reloaded.Receipts["op"]
	if m, err := r.Response.ResolveCost(); err != nil || m != r.ReservedAmount() {
		t.Fatalf("reloaded completed receipt inconsistent: %d %v vs %d", m, err, r.ReservedAmount())
	}
}
