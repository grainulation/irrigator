package executor

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func billingRegistryFixture(t *testing.T) (string, model.BillingReceipt) {
	t.Helper()
	old := runfixture.UnknownCostFixture(t)
	p := runfixture.Read(t, old).Plan
	p.APIVersion = launch.ArtifactPlanVersion
	return billingRegistryFromPlan(t, p)
}
func billingRegistryFromPlan(t *testing.T, p model.Plan) (string, model.BillingReceipt) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "billing-review")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	cells := CheckpointCells{Root: root}
	a, err := cells.BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	if err = cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "accepted"}}); err != nil {
		t.Fatal(err)
	}
	if err = cells.RecordValidation(CellValidation{Fence: a.Fence, Level: "validated"}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	r := s.Execution.Reservations[a.Fence.OperationID]
	return root, model.BillingReceipt{Schema: "irrigator.billing/v1", RunID: s.RunID, TaskID: r.TaskID, OperationID: r.OperationID, Attempt: r.Attempt, BindingDigest: r.BindingDigest, ReceiptID: "provider-charge-unique", Currency: "USD", AmountMicros: 50000, EvidenceSHA256: strings.Repeat("a", 64), EvidenceReference: "invoice:review-001", Attestation: "operator_verified_external_billing"}
}

func TestBillingRegistryRejectsNullOrMissingAmount(t *testing.T) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "billing-registry"))
	for _, kind := range []string{"null", "missing"} {
		t.Run(kind, func(t *testing.T) {
			root, r := billingRegistryFixture(t)
			raw, _ := json.Marshal(r)
			var m map[string]any
			json.Unmarshal(raw, &m)
			if kind == "null" {
				m["amount_micros"] = nil
			} else {
				delete(m, "amount_micros")
			}
			raw, _ = json.Marshal(m)
			path := filepath.Join(t.TempDir(), "receipt.json")
			os.WriteFile(path, raw, 0600)
			if _, err := provider.BillingCLI([]string{"reconcile", root, path, "--attest-external-billing"}); err == nil {
				t.Fatal("billing accepted absent numeric amount as zero and released hold")
			}
		})
	}
}
func TestBillingRegistryRejectsCrossRunChargeReuse(t *testing.T) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "billing-registry"))
	first, a := billingRegistryFixture(t)
	second, b := billingRegistryFromPlan(t, runfixture.Read(t, first).Plan)
	if err := provider.ReconcileBilling(first, a); err != nil {
		t.Fatal(err)
	}
	if err := provider.ReconcileBilling(second, b); err == nil {
		t.Fatal("same external receipt and evidence accepted for distinct runs")
	}
}
func TestBillingRegistryHistoricEvidenceAndFences(t *testing.T) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "billing-registry"))
	root, r := billingRegistryFixture(t)
	before := runfixture.Read(t, root)
	if err := provider.ReconcileBilling(root, r); err != nil {
		t.Fatal(err)
	}
	after := runfixture.Read(t, root)
	if !reflect.DeepEqual(before.Plan, after.Plan) || before.PlanHash != after.PlanHash || !reflect.DeepEqual(before.Tasks, after.Tasks) || !reflect.DeepEqual(before.Execution.Reservations, after.Execution.Reservations) {
		t.Fatal("billing rewrote historic plan, tasks, or provider reservation")
	}
	if err := provider.ReconcileBilling(root, r); err != nil {
		t.Fatal("identical retry is not idempotent", err)
	}
	changed := r
	changed.AmountMicros++
	if err := provider.ReconcileBilling(root, changed); err == nil {
		t.Fatal("receipt rewritten")
	}
	for _, kind := range []string{"run", "task", "operation", "attempt", "binding", "negative", "currency"} {
		t.Run(kind, func(t *testing.T) {
			bad := r
			switch kind {
			case "run":
				bad.RunID = "other"
			case "task":
				bad.TaskID = "b"
			case "operation":
				bad.OperationID = "other"
			case "attempt":
				bad.Attempt++
			case "binding":
				bad.BindingDigest = "other"
			case "negative":
				bad.AmountMicros = -1
			case "currency":
				bad.Currency = "EUR"
			}
			if err := provider.ReconcileBilling(root, bad); err == nil {
				t.Fatal("invalid billing fence accepted")
			}
		})
	}
}
func TestBillingRegistryUncertaintyAndOverflow(t *testing.T) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "billing-registry"))
	r := model.ManagedReservation{OperationID: "op", ReservedMicros: 250000, ReportedMicros: 0, Status: "uncertain"}
	e := &model.ManagedExecutionState{Reservations: map[string]model.ManagedReservation{"op": r}, BillingReceipts: map[string]model.BillingReceipt{"op": {AmountMicros: 1}}}
	used, err := checkpoint.ManagedBudgetUsed(e)
	if err != nil || used != 250000 {
		t.Fatalf("uncertain hold refunded: %d %v", used, err)
	}
	if _, settled := checkpoint.ManagedAccountedAmount(e, r); settled {
		t.Fatal("billing resolved execution uncertainty")
	}
	r.Status = "unknown_cost"
	e.Reservations["op"] = r
	e.BillingReceipts["op"] = model.BillingReceipt{AmountMicros: math.MaxInt64}
	e.Reservations["second"] = model.ManagedReservation{OperationID: "second", Status: "settled", ReportedMicros: 1}
	if _, err = checkpoint.ManagedBudgetUsed(e); err == nil {
		t.Fatal("billing overflow accepted")
	}
}

func TestBillingRegistryVersionGateAndOverrun(t *testing.T) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "billing-registry"))
	root, r := billingRegistryFixture(t)
	s := runfixture.Read(t, root)
	for _, version := range []int{1, 2, 3, 4} {
		s.Version = version
		if checkpoint.BillingReceiptValid(s, r) == nil {
			t.Errorf("billing admitted checkpoint%d", version)
		}
	}
	r.AmountMicros = 500001
	if err := provider.ReconcileBilling(root, r); err != nil {
		t.Fatal(err)
	}
	after := runfixture.Read(t, root)
	if !after.Execution.Frozen {
		t.Fatal("external billing overrun did not freeze admission")
	}
	used, err := checkpoint.ManagedBudgetUsed(after.Execution)
	if err != nil || used != 500001 {
		t.Fatalf("overrun accounting: %d %v", used, err)
	}
}

func TestBillingRegistryMissingRegistryFailsClosed(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "billing-registry")
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", registry)
	root, r := billingRegistryFixture(t)
	if err := provider.ReconcileBilling(root, r); err != nil {
		t.Fatal(err)
	}
	moved := registry + "-offline"
	if err := os.Rename(registry, moved); err != nil {
		t.Fatal(err)
	}
	offline, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal("offline evidence became unreadable", err)
	}
	if err = checkpoint.SetControl(root, "paused"); err == nil {
		t.Fatal("reconciled checkpoint mutation allowed with registry unavailable")
	}
	if err = managed.ManagedCanBegin(offline, offline.Tasks["b"]); err == nil {
		t.Fatal("dispatch allowed with registry unavailable")
	}
	if err := os.Rename(moved, registry); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "paused"); err != nil {
		t.Fatal("restored registry did not restore mutation authority", err)
	}
}

func TestBillingRegistryConcurrentAllocation(t *testing.T) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "billing-registry"))
	first, a := billingRegistryFixture(t)
	second, b := billingRegistryFromPlan(t, runfixture.Read(t, first).Plan)
	results := make(chan error, 2)
	go func() { results <- provider.ReconcileBilling(first, a) }()
	go func() { results <- provider.ReconcileBilling(second, b) }()
	successes := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent receipt allocated %d times", successes)
	}
}
func TestBillingRegistryCrashAllocationRemainsReserved(t *testing.T) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "billing-registry"))
	first, a := billingRegistryFixture(t)
	second, b := billingRegistryFromPlan(t, runfixture.Read(t, first).Plan)
	snapshot := runfixture.Read(t, first)
	// Simulate death after durable registry reservation but before state commit.
	if err := provider.ReserveBillingAllocation(snapshot, a); err != nil {
		t.Fatal(err)
	}
	if len(runfixture.Read(t, first).Execution.BillingReceipts) != 0 {
		t.Fatal("precommit reservation mutated checkpoint")
	}
	if err := provider.ReconcileBilling(second, b); err == nil {
		t.Fatal("crash orphan allocation reused by another run")
	}
	if err := provider.ReconcileBilling(first, a); err != nil {
		t.Fatal("identical crash recovery receipt could not resume", err)
	}
}

func TestBillingRegistryTamperedRegistryBlocksMutation(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "billing-registry")
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", registry)
	root, r := billingRegistryFixture(t)
	if err := provider.ReconcileBilling(root, r); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(registry, checkpoint.BillingAllocationName(r))
	if err := os.WriteFile(path, []byte(strings.Repeat("0", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "paused"); err == nil {
		t.Fatal("tampered billing allocation granted mutation authority")
	}
}
