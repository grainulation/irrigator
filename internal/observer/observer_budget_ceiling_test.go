package observer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func bReadCeiling(t *testing.T, dir string) ProviderCeilingRecord {
	t.Helper()
	var r ProviderCeilingRecord
	raw, err := os.ReadFile(filepath.Join(dir, BudgetCeilingFile))
	if err != nil || json.Unmarshal(raw, &r) != nil {
		t.Fatalf("read ceiling: %v", err)
	}
	return r
}

func TestPrepareProviderBudgetIdentityInitializesAndPreserves(t *testing.T) {
	dir := t.TempDir()
	if err := PrepareProviderBudgetIdentity(dir, 7); err != nil {
		t.Fatal(err)
	}
	if r := bReadCeiling(t, dir); r.CeilingUSD != 7 || r.Revision != 1 {
		t.Fatalf("fresh start must publish the ceiling: %+v", r)
	}
	// An explicit initialized-empty ledger (not an absent file) is written so a
	// missing ledger can later be told apart from zero spend.
	if l := bReadLedger(t, dir); l.Receipts == nil || len(l.Receipts) != 0 {
		t.Fatalf("fresh start must write an explicit empty ledger: %+v", l)
	}

	recs := map[string]model.ProviderReceipt{}
	runfixture.BAdd(recs, runfixture.BRunA, "t1", 1, "pending", 2)
	bWrite(t, dir, recs)
	if err := PrepareProviderBudgetIdentity(dir, 7); err != nil {
		t.Fatal(err)
	}
	if l := bReadLedger(t, dir); len(l.Receipts) != 1 {
		t.Fatalf("existing ledger must never be overwritten: %+v", l)
	}
	if err := PrepareProviderBudgetIdentity(dir, 9); err != nil {
		t.Fatal(err)
	}
	if r := bReadCeiling(t, dir); r.CeilingUSD != 9 || r.Revision != 2 {
		t.Fatalf("changed budget must bump revision: %+v", r)
	}
	if l := bReadLedger(t, dir); len(l.Receipts) != 1 {
		t.Fatalf("ceiling change must not touch the ledger: %+v", l)
	}
}

func TestPrepareProviderBudgetIdentityFailsClosedOnStaleCeilingWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not block root")
	}
	dir := t.TempDir()
	if err := PrepareProviderBudgetIdentity(dir, 10); err != nil { // persisted old ceiling 10 + ledger
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}

	// Same-budget restart needs no write and must keep working.
	if err := PrepareProviderBudgetIdentity(dir, 10); err != nil {
		t.Fatalf("unchanged ceiling must not require a write: %v", err)
	}

	// Changed-budget restart: the ceiling write fails, so startup must be refused.
	err := PrepareProviderBudgetIdentity(dir, 12)
	if err == nil {
		t.Skip("filesystem did not block the write; cannot simulate write failure")
	}
	_ = os.Chmod(dir, 0700)
	if r := bReadCeiling(t, dir); r.CeilingUSD != 10 || r.Revision != 1 {
		t.Fatalf("stale ceiling file must be left unmodified, not rewritten or guessed: %+v", r)
	}
}

func bWrite(t *testing.T, dir string, recs map[string]model.ProviderReceipt) {
	t.Helper()
	data, err := json.Marshal(ProviderLedger{Receipts: recs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
