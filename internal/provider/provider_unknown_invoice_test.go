package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// noInvoiceFields fails if the marshaled wire bytes state any cost at all.
func noInvoiceFields(t *testing.T, label string, response model.WorkResponse) []byte {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cost_usd") || strings.Contains(string(raw), "cost_micros") {
		t.Fatalf("%s: unknown billing must publish no invoice fields: %s", label, raw)
	}
	return raw
}

func requireUnknownUncertain(t *testing.T, label string, p *ProviderPool, id string, got model.WorkResponse) {
	t.Helper()
	if got.Outcome != model.OutcomeUncertain || !got.CostUnknown || got.CostUSD != 0 || got.CostMicros != 0 || got.CostBasis != "unknown" {
		t.Fatalf("%s: response %+v", label, got)
	}
	noInvoiceFields(t, label, got)
	receipt := p.Ledger.Receipts[id]
	if receipt.Status != "uncertain" || !receipt.CostUnknown || receipt.ReservedAmount() != 100000 || receipt.Reserved != .1 {
		t.Fatalf("%s: receipt must stay uncertain/unknown with the full reservation: %+v", label, receipt)
	}
	if receipt.Response.CostUSD != 0 || receipt.Response.CostMicros != 0 {
		t.Fatalf("%s: reservation leaked into reported cost: %+v", label, receipt.Response)
	}
	// Ledger reload keeps the durable unknown marker and never a known zero.
	data, err := json.Marshal(p.Ledger)
	if err != nil || !strings.Contains(string(data), `"cost_unknown":true`) {
		t.Fatalf("%s: durable marker missing: %v %s", label, err, data)
	}
	var reloaded observer.ProviderLedger
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatal(err)
	}
	back := reloaded.Receipts[id]
	if !back.CostUnknown || !back.Response.CostUnknown || back.ReservedAmount() != 100000 {
		t.Fatalf("%s: reload lost unknown billing: %+v", label, back)
	}
	noInvoiceFields(t, label+" reloaded", back.Response)
}

// Covers the eof and invalid_cost cases through the actual p.work path with a
// controlled fake executable (never a real model CLI).
func TestProviderPoolNoEvidencePathsAreUnknownBilling(t *testing.T) {
	for _, mode := range []string{"eof", "invalid_cost"} {
		t.Run(mode, func(t *testing.T) {
			p := fakeProviderPool(t)
			if mode == "invalid_cost" {
				script := "#!/bin/sh\nread line\nprintf '%s\\n' '{\"type\":\"result\",\"total_cost_usd\":-1,\"structured_output\":{\"answer\":\"x\"}}'\ncat >/dev/null\n"
				if err := os.WriteFile(p.Opts.Claude, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				setFakeProviderMode(t, p, mode)
			}
			req := runfixture.FakeProviderRequest("rid")
			got := p.Work(context.Background(), req)
			requireUnknownUncertain(t, mode, p, "rid", got)
			if got.Error == "" || got.RequestID != "rid" {
				t.Fatalf("error and request identity must be preserved: %+v", got)
			}
			// Cached uncertain replay stays unknown and is never re-executed.
			again := p.Work(context.Background(), req)
			if again.Outcome != model.OutcomeUncertain || !again.CostUnknown || !strings.Contains(again.Error, "replay prohibited") {
				t.Fatalf("replay %+v", again)
			}
			noInvoiceFields(t, mode+" replay", again)
		})
	}
}

// A genuinely observed positive floor survives a ledger roundtrip and a cached
// uncertain replay; the full reservation stays held and nothing is re-executed.
func TestProviderCachedUncertainReplayPreservesObservedFloor(t *testing.T) {
	p := fakeProviderPool(t)
	req := runfixture.FakeProviderRequest("floor")
	first := p.Work(context.Background(), req)
	if first.Error != "" || first.CostUSD != .01 {
		t.Fatalf("setup: %+v", first)
	}
	receipt := p.Ledger.Receipts[req.OperationID]
	receipt.Status = "uncertain"
	receipt.SetReserved(100000)
	receipt.Response.Outcome = model.OutcomeUncertain
	p.Ledger.Receipts[req.OperationID] = receipt
	data, err := json.Marshal(p.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded observer.ProviderLedger
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatal(err)
	}
	p.Ledger = reloaded
	// A re-dispatch would fail loudly: the helper process would now emit eof.
	setFakeProviderMode(t, p, "eof")
	again := p.Work(context.Background(), req)
	if again.Outcome != model.OutcomeUncertain || again.CostUnknown || again.Output != nil || !strings.Contains(again.Error, "replay prohibited") {
		t.Fatalf("replay %+v", again)
	}
	if again.CostUSD != .01 || again.CostMicros != 10000 || again.RequestID != req.RequestID {
		t.Fatalf("observed floor erased on replay: %+v", again)
	}
	if raw, _ := json.Marshal(again); !strings.Contains(string(raw), `"cost_usd":0.01`) {
		t.Fatalf("floor missing from wire: %s", raw)
	}
	after := p.Ledger.Receipts[req.OperationID]
	if after.Status != "uncertain" || after.CostUnknown || after.ReservedAmount() != 100000 {
		t.Fatalf("receipt changed by replay: %+v", after)
	}
}

// finish distinguishes billing certainty from execution certainty.
func TestProviderFinishBillingCertaintyMatrix(t *testing.T) {
	cases := []struct {
		name      string
		known     bool
		response  model.WorkResponse
		unknown   bool
		status    string
		wantMicro int64
	}{
		{"pre-dispatch known zero releases", true, model.WorkResponse{Protocol: model.ProtocolVersion, Outcome: model.OutcomeNotStarted, Error: "x"}, false, "completed", 0},
		{"post-handoff no evidence", false, model.WorkResponse{Protocol: model.ProtocolVersion, Error: "eof"}, true, "uncertain", 100000},
		{"preexisting marker honored", true, model.WorkResponse{Protocol: model.ProtocolVersion, CostUnknown: true}, true, "uncertain", 100000},
		{"observed floor preserved", false, model.WorkResponse{Protocol: model.ProtocolVersion, Error: "x", CostUSD: .03}, false, "uncertain", 100000},
		{"explicit zero observation stays known", false, model.WorkResponse{Protocol: model.ProtocolVersion, Error: "x", CostMicrosExplicit: true}, false, "uncertain", 100000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeProviderPool(t)
			req := runfixture.FakeProviderRequest("f")
			p.Ledger.Receipts["f"] = model.ProviderReceipt{Hash: strings.Repeat("a", 64), Status: "pending", Reserved: .1, ReservedMicros: 100000}
			got := p.Finish(req, strings.Repeat("a", 64), tc.response, tc.known)
			receipt := p.Ledger.Receipts["f"]
			if got.CostUnknown != tc.unknown || receipt.CostUnknown != tc.unknown || receipt.Status != tc.status || receipt.ReservedAmount() != tc.wantMicro {
				t.Fatalf("response %+v receipt %+v", got, receipt)
			}
			raw, _ := json.Marshal(got)
			hasCost := strings.Contains(string(raw), "cost_usd")
			if tc.unknown && (hasCost || got.CostUSD != 0 || got.CostMicros != 0) {
				t.Fatalf("unknown must omit costs: %s", raw)
			}
			if !tc.unknown && !hasCost {
				t.Fatalf("observed billing must keep its cost fields: %s", raw)
			}
			if tc.name == "observed floor preserved" && got.CostUSD != .03 {
				t.Fatalf("observed floor erased: %+v", got)
			}
		})
	}
}

func writeTestMetrics(t *testing.T, p *ProviderPool, id string, cost float64) {
	t.Helper()
	path := filepath.Join(p.Opts.Dir, fmt.Sprintf("operation-%x.metrics.json", sha256.Sum256([]byte(id))))
	if err := jsonio.AtomicJSON(path, map[string]any{"operation_id": id, "cost_usd": cost}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderStartupPendingBillingEvidence(t *testing.T) {
	pending := func(t *testing.T, metrics *float64) (*ProviderPool, model.ProviderReceipt) {
		p := fakeProviderPool(t)
		p.Ledger.Receipts["p"] = model.ProviderReceipt{Hash: strings.Repeat("b", 64), Status: "pending", Reserved: .1, ReservedMicros: 100000}
		if metrics != nil {
			writeTestMetrics(t, p, "p", *metrics)
		}
		p.RestoreAdmissionBarrier()
		return p, p.Ledger.Receipts["p"]
	}
	t.Run("no metrics is unknown", func(t *testing.T) {
		p, receipt := pending(t, nil)
		if receipt.Status != "uncertain" || !receipt.CostUnknown || !receipt.Response.CostUnknown || receipt.ReservedAmount() != 100000 {
			t.Fatalf("%+v", receipt)
		}
		noInvoiceFields(t, "startup", receipt.Response)
		replay := p.Work(context.Background(), runfixture.FakeProviderRequest("p"))
		if replay.Outcome != model.OutcomeUncertain {
			t.Fatalf("%+v", replay)
		}
	})
	t.Run("observed zero stays known zero", func(t *testing.T) {
		zero := 0.0
		_, receipt := pending(t, &zero)
		if receipt.Status != "uncertain" || receipt.CostUnknown || receipt.Response.CostUnknown || receipt.ReservedAmount() != 100000 {
			t.Fatalf("%+v", receipt)
		}
	})
	t.Run("observed charge floor preserved", func(t *testing.T) {
		cost := .03
		_, receipt := pending(t, &cost)
		if receipt.CostUnknown || receipt.Response.CostUnknown || receipt.Response.CostUSD != .03 || receipt.ReservedAmount() != 100000 {
			t.Fatalf("%+v", receipt)
		}
	})
	t.Run("malformed metrics stay fail closed", func(t *testing.T) {
		p := fakeProviderPool(t)
		p.Ledger.Receipts["p"] = model.ProviderReceipt{Hash: strings.Repeat("b", 64), Status: "pending", Reserved: .1, ReservedMicros: 100000}
		path := filepath.Join(p.Opts.Dir, fmt.Sprintf("operation-%x.metrics.json", sha256.Sum256([]byte("p"))))
		if err := os.WriteFile(path, []byte(`{"operation_id":"p","cost_usd":"bad"}`), 0600); err != nil {
			t.Fatal(err)
		}
		p.RestoreAdmissionBarrier()
		receipt := p.Ledger.Receipts["p"]
		if receipt.Status != "pending" || !p.Ledger.AdmissionBlocked || receipt.ReservedAmount() != 100000 {
			t.Fatalf("%+v", receipt)
		}
	})
}

func TestLostProviderResponseIsUnknownBilling(t *testing.T) {
	var out bytes.Buffer
	if err := model.LostProviderResponse([]byte(`{"request_id":"req-9","operation_id":"op"}`), &out); err != nil {
		t.Fatal(err)
	}
	wire := out.String()
	if strings.Contains(wire, "cost_usd") || strings.Contains(wire, "cost_micros") {
		t.Fatalf("lost response must not state a cost: %s", wire)
	}
	var decoded model.WorkResponse
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RequestID != "req-9" || decoded.Outcome != model.OutcomeUncertain || decoded.Protocol != model.ProtocolVersion || !strings.Contains(decoded.Error, "uncertain") || decoded.Output != nil {
		t.Fatalf("%+v", decoded)
	}
}
