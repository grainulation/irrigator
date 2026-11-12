package executor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func billingFixture(t *testing.T) (string, model.BillingReceipt) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "registry"))
	old := runfixture.UnknownCostFixture(t)
	s, e := checkpoint.ReadState(old)
	if e != nil {
		t.Fatal(e)
	}
	p := s.Plan
	p.APIVersion = launch.ArtifactPlanVersion
	root := filepath.Join(t.TempDir(), "billing")
	if e = checkpoint.Initialize(p, root, "managed"); e != nil {
		t.Fatal(e)
	}
	if e = checkpoint.SetControl(root, "run"); e != nil {
		t.Fatal(e)
	}
	c := CheckpointCells{Root: root}
	a, e := c.BeginAttempt("a", "", 123)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); e != nil {
		t.Fatal(e)
	}
	if e = c.RecordValidation(CellValidation{Fence: a.Fence, Level: "validated"}); e != nil {
		t.Fatal(e)
	}
	s, e = checkpoint.ReadState(root)
	if e != nil {
		t.Fatal(e)
	}
	r := s.Execution.Reservations[a.Fence.OperationID]
	return root, model.BillingReceipt{Schema: "irrigator.billing/v1", RunID: s.RunID, TaskID: "a", OperationID: r.OperationID, Attempt: r.Attempt, BindingDigest: r.BindingDigest, ReceiptID: "fixture-only-invoice", Currency: "USD", AmountMicros: 12000, EvidenceSHA256: strings.Repeat("a", 64), EvidenceReference: "test fixture, not an actual invoice", Attestation: "operator_verified_external_billing"}
}
func TestBillingReconciliationPreservesProviderEvidence(t *testing.T) {
	root, r := billingFixture(t)
	if e := provider.ReconcileBilling(root, r); e != nil {
		t.Fatal(e)
	}
	if e := provider.ReconcileBilling(root, r); e != nil {
		t.Fatal(e)
	}
	s, e := checkpoint.ReadState(root)
	if e != nil {
		t.Fatal(e)
	}
	held := s.Execution.Reservations[r.OperationID]
	if held.Status != "unknown_cost" || held.ReportedMicros != 0 || !s.Tasks["a"].CostUnknown {
		t.Fatal("provider evidence rewritten")
	}
	used, e := checkpoint.ManagedBudgetUsed(s.Execution)
	if e != nil || used != 12000 {
		t.Fatalf("used %d %v", used, e)
	}
	c := provider.ProjectCost(s)
	if c.UnknownOperations != 0 || c.KnownReportedUSD != 0 || c.ExternallyReconciledUSD != .012 {
		t.Fatalf("projection %+v", c)
	}
	r.AmountMicros++
	if e = provider.ReconcileBilling(root, r); e == nil {
		t.Fatal("immutable receipt replaced")
	}
}
func TestBillingReconciliationOverrunFreezes(t *testing.T) {
	root, r := billingFixture(t)
	r.AmountMicros = 300000
	if e := provider.ReconcileBilling(root, r); e != nil {
		t.Fatal(e)
	}
	s, e := checkpoint.ReadState(root)
	if e != nil || !s.Execution.Frozen {
		t.Fatalf("missing freeze %v", e)
	}
}
func TestBillingReconciliationUncertaintyRetainsHold(t *testing.T) {
	root, r := billingFixture(t)
	if e := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Status = "uncertain"
		return managed.ManagedHoldUncertain(s, s.Tasks["a"])
	}); e != nil {
		t.Fatal(e)
	}
	if e := provider.ReconcileBilling(root, r); e != nil {
		t.Fatal(e)
	}
	s, e := checkpoint.ReadState(root)
	if e != nil {
		t.Fatal(e)
	}
	used, e := checkpoint.ManagedBudgetUsed(s.Execution)
	if e != nil || used != 250000 || s.Tasks["a"].Status != "uncertain" {
		t.Fatalf("uncertainty refunded %d %v", used, e)
	}
}
func TestBillingReconciliationRejectsFence(t *testing.T) {
	root, r := billingFixture(t)
	r.Attempt++
	if e := provider.ReconcileBilling(root, r); e == nil {
		t.Fatal("accepted stale fence")
	}
}
