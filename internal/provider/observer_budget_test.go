package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func bWrite(t *testing.T, dir string, recs map[string]model.ProviderReceipt) {
	t.Helper()
	data, err := json.Marshal(observer.ProviderLedger{Receipts: recs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func bReadLedger(t *testing.T, dir string) observer.ProviderLedger {
	t.Helper()
	var l observer.ProviderLedger
	raw, err := os.ReadFile(filepath.Join(dir, observer.BudgetLedgerFile))
	if err != nil || json.Unmarshal(raw, &l) != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return l
}

func bTestPool(dir string) *ProviderPool {
	p := &ProviderPool{Opts: ProviderPoolOptions{Dir: dir, Budget: 5, workers: 1, Model: "haiku", Claude: "/nonexistent/claude"},
		Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{}}, Slots: make(chan *ProviderSlot, 1)}
	p.Slots <- &ProviderSlot{}
	return p
}

func bReq(run, op string) model.WorkRequest {
	return model.WorkRequest{RunID: run, Protocol: model.ProtocolVersion, Kind: "task", RequestID: "r1", OperationID: op, Attempt: 1, TimeoutMS: 1000, MaxCostUSD: 2,
		Task: model.Task{ID: "t1", Prompt: "do", Requires: []string{"x"}}}
}

func TestBudgetRetryOfSameOperationNotDoubled(t *testing.T) {
	dir := t.TempDir()
	p := bTestPool(dir)
	req := bReq(runfixture.BRunA, model.OperationID(runfixture.BRunA, "t1", 2))
	canonical := req
	canonical.RequestID, canonical.Attempt = "", 0
	enc, _ := json.Marshal(canonical)
	p.Ledger.Receipts[req.OperationID] = model.ProviderReceipt{Hash: fmt.Sprintf("%x", sha256.Sum256(enc)), Status: "pending", Reserved: 2, Attribution: observer.ProviderReceiptAttribution(req)}
	if err := p.save(); err != nil {
		t.Fatal(err)
	}
	retry := req
	retry.RequestID, retry.Attempt = "r2", 2
	for _, r := range []model.WorkRequest{req, retry} {
		if resp := p.Work(context.Background(), r); !strings.Contains(resp.Error, "pending or uncertain") {
			t.Fatalf("retry must not reserve again: %+v", resp)
		}
	}
	ledger := bReadLedger(t, dir)
	r, ok := ledger.Receipts[req.OperationID]
	if len(ledger.Receipts) != 1 || !ok || r.Status != "pending" || r.Reserved != 2 {
		t.Fatalf("one pending receipt reserving 2 expected: %+v", ledger)
	}
}

func TestBudgetAttributionFromActualWorkRequest(t *testing.T) {
	dir := t.TempDir()
	p := bTestPool(dir)
	p.Slots = make(chan *ProviderSlot, 2)
	p.Slots <- &ProviderSlot{}
	p.Slots <- &ProviderSlot{}
	good := bReq(runfixture.BRunA, model.OperationID(runfixture.BRunA, "t1", 3))
	p.Work(context.Background(), good)
	r := p.Ledger.Receipts[good.OperationID]
	if r.Attribution == nil || r.Attribution.RunID != runfixture.BRunA || r.Attribution.TaskID != "t1" || r.Attribution.Generation != 3 || r.Status != "completed" {
		t.Fatalf("new receipt must carry verified attribution: %+v", r)
	}
	unverified := bReq(runfixture.BRunA, "not-an-operation-hash")
	p.Work(context.Background(), unverified)
	if r := p.Ledger.Receipts[unverified.OperationID]; r.Attribution != nil {
		t.Fatalf("unverifiable operation must stay unattributed: %+v", r)
	}
}

func TestProviderReceiptAttributionRejectsUnverifiableRequests(t *testing.T) {
	good := bReq(runfixture.BRunA, model.OperationID(runfixture.BRunA, "t1", 5))
	if a := observer.ProviderReceiptAttribution(good); a == nil || *a != (model.ProviderAttribution{RunID: runfixture.BRunA, TaskID: "t1", Generation: 5}) {
		t.Fatalf("verified operation must attribute: %+v", a)
	}
	forged := bReq(runfixture.BRunA, model.OperationID("fedcba9876543210fedcba9876543210", "t1", 5))
	for name, req := range map[string]model.WorkRequest{
		"forged":       forged,
		"empty op":     bReq(runfixture.BRunA, ""),
		"empty run":    bReq("", good.OperationID),
		"unknown form": bReq(runfixture.BRunA, "not-an-operation-hash"),
	} {
		if a := observer.ProviderReceiptAttribution(req); a != nil {
			t.Fatalf("%s must stay unattributed: %+v", name, a)
		}
	}
}

func TestRecordProviderCeilingRevision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, observer.BudgetCeilingFile)
	read := func() observer.ProviderCeilingRecord {
		var r observer.ProviderCeilingRecord
		raw, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(raw, &r) != nil {
			t.Fatalf("read: %v", err)
		}
		return r
	}
	if err := observer.RecordProviderCeiling(dir, 10); err != nil {
		t.Fatal(err)
	}
	first := read()
	if err := observer.RecordProviderCeiling(dir, 10); err != nil {
		t.Fatal(err)
	}
	if again := read(); again.Revision != 1 || !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("unchanged ceiling must not rewrite: %+v", again)
	}
	if err := observer.RecordProviderCeiling(dir, 12); err != nil {
		t.Fatal(err)
	}
	if changed := read(); changed.Revision != 2 || changed.CeilingUSD != 12 {
		t.Fatalf("changed ceiling: %+v", changed)
	}
}

// budgetReadFile pins the file identity: a concurrent atomic replacement may
// surface only as the safe "changed" code, never as a torn or partial ledger.
func TestBudgetReadFileConcurrentReplacementNeverTorn(t *testing.T) {
	dir := t.TempDir()
	recs := map[string]model.ProviderReceipt{}
	runfixture.BAdd(recs, runfixture.BRunA, "t0", 1, "pending", 1)
	bWrite(t, dir, recs)
	ledgerPath := filepath.Join(dir, "ledger.json")
	var wg sync.WaitGroup
	writerErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			next := map[string]model.ProviderReceipt{}
			runfixture.BAdd(next, runfixture.BRunA, fmt.Sprintf("t%d", i), 1, "pending", 1)
			if err := jsonio.AtomicJSON(ledgerPath, observer.ProviderLedger{Receipts: next}); err != nil {
				writerErr <- err
				return
			}
		}
	}()
	// Even a failed assertion must join the writer before temporary-file cleanup.
	defer wg.Wait()
	check := func() {
		t.Helper()
		raw, _, code := observer.BudgetReadFile(ledgerPath, 64<<20)
		if code != "" {
			if code != "changed" {
				t.Fatalf("concurrent replacement must expose only the safe changed code, got %q", code)
			}
			return
		}
		var l observer.ProviderLedger
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatalf("torn ledger read: %v", err)
		}
		if len(l.Receipts) != 1 {
			t.Fatalf("stable ledger must have exactly one receipt: %+v", l)
		}
		for _, r := range l.Receipts {
			if r.Status != "pending" || r.Reserved != 1 || r.Attribution == nil || r.Attribution.RunID != runfixture.BRunA {
				t.Fatalf("unexpected receipt: %+v", r)
			}
		}
	}
	for i := 0; i < 40; i++ {
		check()
	}
	wg.Wait()
	select {
	case err := <-writerErr:
		t.Fatal(err)
	default:
	}
	check()
	final := bReadLedger(t, dir)
	receipt, ok := final.Receipts[model.OperationID(runfixture.BRunA, "t39", 1)]
	if len(final.Receipts) != 1 || !ok || receipt.Status != "pending" || receipt.Reserved != 1 || receipt.Attribution == nil || *receipt.Attribution != (model.ProviderAttribution{RunID: runfixture.BRunA, TaskID: "t39", Generation: 1}) {
		t.Fatalf("last complete writer receipt not preserved: %+v", final)
	}
}
