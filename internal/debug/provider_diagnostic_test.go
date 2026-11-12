package debug

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func pdiagPool(t *testing.T, mode, step string, budget float64) (*provider.ProviderPool, string) {
	t.Helper()
	dir := t.TempDir()
	starts := filepath.Join(dir, "starts")
	// Fixture configuration belongs to this executable, not the ambient provider
	// environment. Keep the production credential allowlist under test unchanged.
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	configuration := "PDIAG_MODE=" + quote(mode) + "\nPDIAG_STEP=" + quote(step) + "\nPDIAG_STARTS=" + quote(starts) + "\n"
	body := strings.Replace(runfixture.PdiagScript, "#!/bin/sh\n", "#!/bin/sh\n"+configuration, 1)
	script := filepath.Join(dir, "claude")
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	p := &provider.ProviderPool{Opts: provider.ProviderPoolOptions{Dir: dir, Claude: script, Budget: budget}, Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}, Slots: make(chan *provider.ProviderSlot, 1)}
	p.Slots <- &provider.ProviderSlot{}
	t.Cleanup(func() { (<-p.Slots).Stop() })
	return p, starts
}

func pdiagState(row model.TaskState) *model.State {
	row.Spec = model.Task{ID: "t1", Prompt: "p", Requires: []string{"answer"}}
	return &model.State{RunID: "run-1", Control: "run", Backend: "haiku", Plan: model.Plan{Name: "n", Tasks: []model.Task{row.Spec}}, Tasks: map[string]*model.TaskState{"t1": &row}}
}

func TestProviderDiagnosticUnknownSubtypeEmptyResult(t *testing.T) {
	p, _ := pdiagPool(t, "unknown", ".1", 5)
	got := p.Work(context.Background(), runfixture.PdiagReq("a", .5))
	if got.DiagnosticCode != model.DiagProviderError || got.Outcome != model.OutcomeSettled || got.Error == "" || strings.TrimSpace(strings.TrimPrefix(got.Error, "provider returned error:")) == "" {
		t.Fatalf("unknown subtype with empty result needs a safe nonempty fallback: %+v", got)
	}
	if r := p.Ledger.Receipts["a"]; r.Status != "completed" || r.Response.DiagnosticCode != model.DiagProviderError {
		t.Fatalf("%+v", r)
	}
}

func TestProviderDiagnosticBudgetSubtypeSettledAndRotates(t *testing.T) {
	p, starts := pdiagPool(t, "budget", ".1", 5)
	req := runfixture.PdiagReq("a", .5)
	got := p.Work(context.Background(), req)
	if got.DiagnosticCode != model.DiagProviderSessionBudget || got.Outcome != model.OutcomeSettled || !strings.Contains(got.Error, "session budget") {
		t.Fatalf("%+v", got)
	}
	if r := p.Ledger.Receipts["a"]; r.Status != "completed" || r.Response.Outcome != model.OutcomeSettled {
		t.Fatalf("settled failure must stay settled: %+v", r)
	}
	again := p.Work(context.Background(), req)
	if again.DiagnosticCode != model.DiagProviderSessionBudget || again.Outcome != model.OutcomeSettled || runfixture.PdiagCount(t, starts+".turns") != 1 {
		t.Fatalf("settled receipt must replay from the ledger, never redispatch: %+v", again)
	}
	_ = p.Work(context.Background(), runfixture.PdiagReq("b", .5))
	if runfixture.PdiagCount(t, starts) != 2 {
		t.Fatalf("a spent CLI session must not be reused")
	}
}

func TestProviderDiagnosticUncertainStaysUncertain(t *testing.T) {
	p, starts := pdiagPool(t, "eof", ".1", 5)
	req := runfixture.PdiagReq("a", .5)
	got := p.Work(context.Background(), req)
	if got.Outcome != model.OutcomeUncertain || got.DiagnosticCode != "" || got.Error == "" {
		t.Fatalf("%+v", got)
	}
	if r := p.Ledger.Receipts["a"]; r.Status != "uncertain" || r.Reserved != .5 {
		t.Fatalf("uncertain reservation must not be released: %+v", r)
	}
	again := p.Work(context.Background(), req)
	if again.Outcome != model.OutcomeUncertain || !strings.Contains(again.Error, "replay prohibited") || runfixture.PdiagCount(t, starts+".turns") != 1 {
		t.Fatalf("%+v", again)
	}
}

func TestProviderDiagnosticSecretPayloadNotProjected(t *testing.T) {
	p, _ := pdiagPool(t, "secret", ".1", 5)
	got := p.Work(context.Background(), runfixture.PdiagReq("a", .5))
	raw, _ := json.Marshal(got)
	led, _ := os.ReadFile(filepath.Join(p.Opts.Dir, "ledger.json"))
	if got.DiagnosticCode != model.DiagProviderError || strings.Contains(string(raw)+string(led), "SECRET") {
		t.Fatalf("raw provider text echoed: %s %s", raw, led)
	}
	row := model.TaskState{Status: "failed", Attempt: 1, Error: "provider returned error: sk-SECRET-TOKEN-123 /Users/me/.ssh", DiagnosticCode: got.DiagnosticCode}
	s := pdiagState(row)
	rep, err := debugProject(s, "", "t1")
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(rep)
	if out := string(b2); strings.Contains(out, "SECRET") || strings.Contains(out, ".ssh") || !strings.Contains(out, model.DiagProviderError) {
		t.Fatalf("debug projection wrong: %s", out)
	}
	// A hostile or unknown code from an untrusted provider is dropped, never echoed.
	if model.KnownDiagnosticCode("sk-SECRET-TOKEN-123") != "" {
		t.Fatal("unknown code accepted")
	}
}

func TestProviderDiagnosticOldMissingCodeCompatible(t *testing.T) {
	var row model.TaskState
	old := `{"spec":{"id":"t1","prompt":"p","requires":["answer"]},"status":"failed","attempt":3,"reported_cost_usd":1.0044482,"error":"provider returned error: "}`
	if err := json.Unmarshal([]byte(old), &row); err != nil || row.DiagnosticCode != "" {
		t.Fatalf("%v %+v", err, row)
	}
	// The old failure's exact cause must NOT be inferred from cost alone.
	if c := debugErrorCode(&row); c != "failed_error_not_projected" {
		t.Fatal(c)
	}
	if c := debugErrorCode(&model.TaskState{Status: "failed", DiagnosticCode: model.DiagProviderSessionBudget, Error: "x"}); c != "failed_"+model.DiagProviderSessionBudget {
		t.Fatal(c)
	}
	var resp model.WorkResponse
	if err := json.Unmarshal([]byte(`{"protocol":"swarm.worker/v1","request_id":"r","error":"e","outcome":"settled"}`), &resp); err != nil || resp.DiagnosticCode != "" {
		t.Fatalf("%v %+v", err, resp)
	}
	enc, _ := json.Marshal(resp)
	if strings.Contains(string(enc), "diagnostic_code") {
		t.Fatalf("absent code must stay absent on the wire: %s", enc)
	}
}

func TestProviderSessionHeadroomRotation(t *testing.T) {
	if provider.ProviderSessionHeadroomCovers(1000000, 1004448, 1000000) || provider.ProviderSessionHeadroomCovers(1000000, 500000, 1000000) || !provider.ProviderSessionHeadroomCovers(2000000, 500000, 1000000) || !provider.ProviderSessionHeadroomCovers(1000000, 0, 1000000) {
		t.Fatal("headroom guard arithmetic")
	}
	p, starts := pdiagPool(t, "ok", ".45", 10)
	// Session caps equal the first request's reservation (1.0): a smaller later request fits the
	// remaining authority (.55) exactly and reuses the session.
	for _, r := range []model.WorkRequest{runfixture.PdiagReq("a", 1), runfixture.PdiagReq("b", .55)} {
		got := p.Work(context.Background(), r)
		if got.Error != "" || math.Abs(got.CostUSD-.45) > 1e-6 {
			t.Fatalf("%s: %+v", r.OperationID, got)
		}
	}
	if runfixture.PdiagCount(t, starts) != 1 {
		t.Fatalf("smaller later request with enough headroom should share the session")
	}
	// Remaining headroom is 1.0-.9=.1 < .5, so a fresh session capped at the reservation is required.
	c := p.Work(context.Background(), runfixture.PdiagReq("c", .5))
	if c.Error != "" || math.Abs(c.CostUSD-.45) > 1e-6 {
		t.Fatalf("fresh session cost delta expected: %+v", c)
	}
	if runfixture.PdiagCount(t, starts) != 2 {
		t.Fatalf("remaining session cap (%.2f) cannot cover the reservation: a fresh session is required", 1.0-.9)
	}
	if runfixture.PdiagCount(t, starts+".turns") != 3 {
		t.Fatalf("no operation may be replayed on rotation")
	}
}

func TestProviderSessionCapNotWidenedBeyondReservation(t *testing.T) {
	// The session cap is exactly the per-operation reservation, so equal-cap requests after any
	// positive spend (.45 of .5 leaves .05) each need a fresh session.
	p, starts := pdiagPool(t, "ok", ".45", 10)
	for _, id := range []string{"a", "b", "c"} {
		got := p.Work(context.Background(), runfixture.PdiagReq(id, .5))
		if got.Error != "" || math.Abs(got.CostUSD-.45) > 1e-6 {
			t.Fatalf("%s: %+v", id, got)
		}
	}
	if runfixture.PdiagCount(t, starts) != 3 {
		t.Fatalf("same-cap paid requests must not share a session (cap would exceed the reservation)")
	}
}
