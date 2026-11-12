package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestProviderReceiptFailureRepairsBeforeUnrelatedSpending(t *testing.T) {
	for _, budget := range []float64{1, .2} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			p, _ := pdiagPool(t, "ok", ".01", budget)
			cleanScript, err := os.ReadFile(p.Opts.Claude)
			if err != nil {
				t.Fatal(err)
			}
			cachedRequest := runfixture.PdiagReq("cached", .1)
			if response := p.Work(context.Background(), cachedRequest); response.Error != "" {
				t.Fatal(response.Error)
			}
			script := `#!/bin/sh
IFS= read -r line
echo dispatched >> dispatches
cp ledger.json ledger.pending
rm ledger.json
mkdir ledger.json
printf '{"type":"result","total_cost_usd":0.3,"structured_output":{"answer":"ok"}}\n'
`
			if err = os.WriteFile(p.Opts.Claude, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			first := p.Work(context.Background(), runfixture.PdiagReq("first", .1))
			if first.Outcome != model.OutcomeUncertain || first.CostUSD != .3 || p.Ledger.Receipts["first"].Reserved != .3 || !strings.Contains(first.Error, "persistence failed") {
				t.Fatalf("lost failed receipt: %+v", first)
			}
			if response := p.Work(context.Background(), runfixture.PdiagReq("while-broken", .1)); response.Outcome != model.OutcomeNotStarted || !strings.Contains(response.Error, "repair failed") {
				t.Fatalf("dispatched with broken storage: %+v", response)
			}
			if response := p.Work(context.Background(), cachedRequest); response.Error != "" {
				t.Fatal("storage failure hid cache", response)
			}
			pending, err := os.ReadFile(filepath.Join(p.Opts.Dir, "ledger.pending"))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filepath.Join(p.Opts.Dir, "ledger.json")); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(p.Opts.Dir, "ledger.json"), pending, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(p.Opts.Claude, cleanScript, 0700); err != nil {
				t.Fatal(err)
			}
			second := p.Work(context.Background(), runfixture.PdiagReq("second", .1))
			if budget == 1 && second.Error != "" {
				t.Fatalf("healthy storage did not reopen unrelated work: %+v", second)
			}
			if budget == .2 && (second.Outcome != model.OutcomeNotStarted || !strings.Contains(second.Error, "budget exhausted")) {
				t.Fatalf("repaired overrun granted extra authority: %+v", second)
			}
			if p.Ledger.AdmissionBlocked || p.Ledger.Receipts["first"].Reserved != .3 || p.Ledger.Receipts["first"].Status != "uncertain" {
				t.Fatal("repair changed uncertain operation")
			}
			if response := p.Work(context.Background(), runfixture.PdiagReq("first", .1)); response.Outcome != model.OutcomeUncertain {
				t.Fatal("failed operation replayed", response)
			}
			if runfixture.PdiagCount(t, filepath.Join(p.Opts.Dir, "dispatches")) != 1 {
				t.Fatal("failed publication dispatched twice")
			}
			var saved observer.ProviderLedger
			raw, err := os.ReadFile(filepath.Join(p.Opts.Dir, "ledger.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.AdmissionBlocked || saved.Receipts["first"].Reserved != .3 || saved.Receipts["first"].Status != "uncertain" {
				t.Fatal("full repair not durable")
			}
			// Restart from the old pending file must recover the .30 durable metric,
			// never silently return to its .10 reservation.
			var old observer.ProviderLedger
			if err = json.Unmarshal(pending, &old); err != nil {
				t.Fatal(err)
			}
			restored := &ProviderPool{Opts: p.Opts, Ledger: old, Slots: p.Slots}
			restored.RestoreAdmissionBarrier()
			response := restored.Work(context.Background(), runfixture.PdiagReq("after-restart", .1))
			if restored.Ledger.Receipts["first"].Reserved != .3 || restored.Ledger.Receipts["first"].Status != "uncertain" {
				t.Fatal("restart lost observed overrun")
			}
			if budget == 1 && response.Error != "" {
				t.Fatal(response.Error)
			}
			if budget == .2 && !strings.Contains(response.Error, "budget exhausted") {
				t.Fatalf("restart escaped budget: %+v", response)
			}
			if cached := restored.Work(context.Background(), cachedRequest); cached.Error != "" {
				t.Fatal("restart hid cached result", cached)
			}
		})
	}
}

func TestProviderShrinkingReservationRotatesExcessAuthority(t *testing.T) {
	p, starts := pdiagPool(t, "ok", ".01", .5)
	for _, req := range []model.WorkRequest{runfixture.PdiagReq("first", .2), runfixture.PdiagReq("second", .05)} {
		if response := p.Work(context.Background(), req); response.Error != "" {
			t.Fatal(response.Error)
		}
	}
	if runfixture.PdiagCount(t, starts) != 2 {
		t.Fatal("new .05 reservation inherited .19 remaining session authority")
	}
	if runfixture.PdiagCount(t, starts+".turns") != 2 {
		t.Fatal("rotation duplicated or dropped dispatch")
	}
}

func TestProviderPublicationRepairPreservesLiveQueuedAdmission(t *testing.T) {
	p := fakeProviderPool(t)
	held := <-p.Slots
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan model.WorkResponse, 1)
	req := runfixture.FakeProviderRequest("queued")
	go func() { done <- p.Work(ctx, req) }()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		_, reserved := p.Ledger.Receipts[req.OperationID]
		if reserved {
			p.Ledger.AdmissionBlocked = true
		}
		p.mu.Unlock()
		if reserved {
			break
		}
		if time.Now().After(deadline) {
			p.Slots <- held
			t.Fatal("request never queued")
		}
		time.Sleep(time.Millisecond)
	}
	p.Slots <- held
	select {
	case response := <-done:
		if response.Error != "" || response.Outcome != model.OutcomeSettled {
			t.Fatalf("durable repair stopped live queued request: %+v", response)
		}
	case <-ctx.Done():
		t.Fatal("queued request did not finish")
	}
	if p.Ledger.Receipts[req.OperationID].Status != "completed" || p.Ledger.AdmissionBlocked {
		t.Fatal("live pending admission was converted to startup uncertainty")
	}
}

func TestProviderQueuedAdmissionCannotSpendNewlyObservedOverrun(t *testing.T) {
	p := fakeProviderPool(t)
	held := <-p.Slots
	p.Opts.Budget = .2
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan model.WorkResponse, 1)
	req := runfixture.FakeProviderRequest("queued")
	go func() { done <- p.Work(ctx, req) }()
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		_, reserved := p.Ledger.Receipts[req.OperationID]
		if reserved {
			p.Ledger.AdmissionBlocked = true
			p.Ledger.Receipts["observed"] = model.ProviderReceipt{Hash: strings.Repeat("a", 64), Status: "uncertain", Reserved: .3, Response: model.WorkResponse{CostUSD: .3}}
		}
		p.mu.Unlock()
		if reserved {
			break
		}
		if time.Now().After(deadline) {
			p.Slots <- held
			t.Fatal("request never queued")
		}
		time.Sleep(time.Millisecond)
	}
	p.Slots <- held
	select {
	case response := <-done:
		if response.Outcome != model.OutcomeNotStarted || !strings.Contains(response.Error, "budget exhausted") {
			t.Fatalf("queued request overspent repaired ledger: %+v", response)
		}
	case <-ctx.Done():
		t.Fatal("queued request did not finish")
	}
	slot := <-p.Slots
	p.Slots <- slot
	if slot.cmd != nil {
		t.Fatal("queued operation launched a provider despite exhausted budget")
	}
}

func TestProviderRestartCompletedLedgerRetainsOverrunAndReplay(t *testing.T) {
	p, starts := pdiagPool(t, "ok", ".30", .20)
	req := runfixture.PdiagReq("observed-overrun", .10)
	original := p.Work(context.Background(), req)
	if original.CostUSD != .30 || original.DiagnosticCode != model.DiagProviderOperationBudget {
		t.Fatalf("fixture did not report overrun: %+v", original)
	}
	// A rename can publish the complete terminal receipt before parent-directory
	// fsync fails. The old process stops admitting, but a restart that reads that
	// complete ledger has the full charge and cached response, not lost evidence.
	p.Ledger.AdmissionBlocked = true
	raw, err := os.ReadFile(filepath.Join(p.Opts.Dir, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ledger observer.ProviderLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		t.Fatal(err)
	}
	restarted := &ProviderPool{Opts: p.Opts, Ledger: ledger}
	restarted.RestoreAdmissionBarrier()
	replay := restarted.Work(context.Background(), req)
	if replay.CostUSD != .30 || replay.DiagnosticCode != original.DiagnosticCode || runfixture.PdiagCount(t, starts+".turns") != 1 {
		t.Fatalf("terminal receipt lost or replayed: %+v", replay)
	}
	next := restarted.Work(context.Background(), runfixture.PdiagReq("next", .01))
	if next.Outcome != model.OutcomeNotStarted || !strings.Contains(next.Error, "budget exhausted") {
		t.Fatalf("completed overrun restored allowance: %+v", next)
	}
}

func TestProviderStartupMetricsRecoveryAndMalformedEvidence(t *testing.T) {
	for _, kind := range []string{"missing", "valid", "malformed", "wrong-operation", "missing-cost", "negative", "duplicate", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			p, _ := pdiagPool(t, "ok", ".01", 1)
			p.Ledger.Receipts["interrupted"] = model.ProviderReceipt{Hash: strings.Repeat("a", 64), Status: "pending", Reserved: .1}
			path := filepath.Join(p.Opts.Dir, fmt.Sprintf("operation-%x.metrics.json", sha256.Sum256([]byte("interrupted"))))
			raw := `{"operation_id":"interrupted","cost_usd":0.3}`
			switch kind {
			case "malformed":
				raw = "{bad"
			case "wrong-operation":
				raw = `{"operation_id":"other","cost_usd":0.3}`
			case "missing-cost":
				raw = `{"operation_id":"interrupted"}`
			case "negative":
				raw = `{"operation_id":"interrupted","cost_usd":-1}`
			case "duplicate":
				raw = `{"operation_id":"interrupted","cost_usd":0.3,"cost_usd":0.1}`
			}
			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "metric")
				if err := os.WriteFile(target, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if kind != "missing" {
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p.RestoreAdmissionBarrier()
			response := p.Work(context.Background(), runfixture.PdiagReq("unrelated", .1))
			if kind == "missing" || kind == "valid" {
				if response.Error != "" {
					t.Fatal(response.Error)
				}
				expected := .1
				if kind == "valid" {
					expected = .3
				}
				if r := p.Ledger.Receipts["interrupted"]; r.Status != "uncertain" || r.Reserved != expected || !strings.Contains(r.Response.Error, "uncertain") {
					t.Fatalf("bad startup floor: %+v", r)
				}
				raw, err := os.ReadFile(filepath.Join(p.Opts.Dir, "ledger.json"))
				if err != nil {
					t.Fatal(err)
				}
				var restored observer.ProviderLedger
				if json.Unmarshal(raw, &restored) != nil {
					t.Fatal("bad repaired ledger")
				}
				if restored.Receipts["interrupted"].Reserved != expected || restored.AdmissionBlocked {
					t.Fatal("startup repair was not durable")
				}
			} else if response.Outcome != model.OutcomeNotStarted || !strings.Contains(response.Error, "malformed accounting evidence") {
				t.Fatalf("bad metrics authorized dispatch: %+v", response)
			}
		})
	}
}

func TestProviderLaterFinishCannotDropRetainedRepairFloor(t *testing.T) {
	p := fakeProviderPool(t)
	p.Opts.Budget = 1
	p.Ledger.AdmissionBlocked = true
	p.Ledger.Receipts["failed-publication"] = model.ProviderReceipt{Hash: strings.Repeat("a", 64), Status: "uncertain", Reserved: .3, Response: model.WorkResponse{CostUSD: .3, Outcome: model.OutcomeUncertain}}
	other := runfixture.FakeProviderRequest("already-inflight")
	p.Ledger.Receipts[other.OperationID] = model.ProviderReceipt{Hash: strings.Repeat("b", 64), Status: "pending", Reserved: .1}
	p.Finish(other, strings.Repeat("b", 64), model.WorkResponse{CostUSD: .01}, true)
	if response := p.Work(context.Background(), runfixture.FakeProviderRequest("unrelated")); response.Error != "" {
		t.Fatal(response.Error)
	}
	raw, err := os.ReadFile(filepath.Join(p.Opts.Dir, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var durable observer.ProviderLedger
	if err = json.Unmarshal(raw, &durable); err != nil {
		t.Fatal(err)
	}
	if durable.AdmissionBlocked || durable.Receipts["failed-publication"].Reserved != .3 || durable.Receipts["failed-publication"].Status != "uncertain" {
		t.Fatal("concurrent finish/repair lost retained exposure")
	}
}

func TestProviderStartupRetainsObservedUncertainFloor(t *testing.T) {
	p := fakeProviderPool(t)
	p.Opts.Budget = .2
	p.Ledger.Receipts["old-uncertain"] = model.ProviderReceipt{Hash: strings.Repeat("a", 64), Status: "uncertain", Reserved: .1, Response: model.WorkResponse{CostUSD: .3, Outcome: model.OutcomeUncertain}}
	p.RestoreAdmissionBarrier()
	response := p.Work(context.Background(), runfixture.FakeProviderRequest("unrelated"))
	if response.Outcome != model.OutcomeNotStarted || !strings.Contains(response.Error, "budget exhausted") || p.Ledger.Receipts["old-uncertain"].Reserved != .3 {
		t.Fatalf("known observation lost on restart: %+v", response)
	}
}
