package executor

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func trackingCostFixture(t *testing.T, count int) string {
	t.Helper()
	prior := runfixture.ManagedStateFixture(t, "claude_cli", "0", count)
	p := runfixture.Read(t, prior).Plan
	p.APIVersion = launch.UnknownCostPlanVersion
	p.Execution.CostPolicy = "track"
	root := filepath.Join(t.TempDir(), "tracking")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestManagedTrackingUnknownCostDoesNotReserveOrBlock(t *testing.T) {
	root := trackingCostFixture(t, 3)
	cells := CheckpointCells{Root: root}
	for _, id := range []string{"a", "b", "c"} {
		a, err := cells.BeginAttempt(id, "", 123)
		if err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
		during := runfixture.Read(t, root)
		if r := during.Execution.Reservations[a.Fence.OperationID]; r.ReservedMicros != 0 {
			t.Fatalf("tracking reserved funds: %+v", r)
		}
		if err := cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); err != nil {
			t.Fatal(err)
		}
		if err := cells.RecordValidation(CellValidation{Fence: a.Fence, Level: "validated"}); err != nil {
			t.Fatal(err)
		}
	}
	s := runfixture.Read(t, root)
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 0 || s.Execution.Frozen {
		t.Fatalf("tracking held budget: %d %v %+v", used, err, s.Execution)
	}
	for _, r := range s.Tasks {
		if r.Status != "verified" || !r.CostUnknown {
			t.Fatalf("lost completed unknown result: %+v", r)
		}
	}
}

func TestManagedTrackingReportedCostAndUncertaintyRemainDistinct(t *testing.T) {
	root := trackingCostFixture(t, 3)
	cells := CheckpointCells{Root: root}
	a, err := cells.BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	// A provider-reported amount above both thresholds remains evidence, not an admission lock.
	if err := cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: .75, Result: map[string]string{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
	if err := cells.RecordValidation(CellValidation{Fence: a.Fence, Level: "validated"}); err != nil {
		t.Fatal(err)
	}
	b, err := cells.BeginAttempt("b", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	if err := cells.RecordOutcome(managed.CellOutcome{Fence: b.Fence, CostUnknown: true, Err: model.WithOutcome(model.OutcomeUncertain, errors.New("interrupted"))}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.Tasks["b"].Status != "uncertain" || s.Execution.Frozen {
		t.Fatalf("execution certainty confused with cost: %+v", s.Tasks["b"])
	}
	if _, err := cells.BeginAttempt("b", "", 123); err == nil {
		t.Fatal("uncertain operation automatically redispatched")
	}
	if _, err := cells.BeginAttempt("c", "", 123); err != nil {
		t.Fatalf("unrelated task blocked by cost uncertainty: %v", err)
	}
	s = runfixture.Read(t, root)
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 750000 {
		t.Fatalf("lost reported evidence: %d %v", used, err)
	}
	if err := checkpoint.ValidateManagedState(s); err != nil {
		t.Fatal(err)
	}
	r := s.Execution.Reservations[a.Fence.OperationID]
	r.ReservedMicros = 250000
	s.Execution.Reservations[a.Fence.OperationID] = r
	if err := checkpoint.ValidateManagedState(s); err == nil {
		t.Fatal("tracking receipt fabricated a financial hold")
	}
}

func TestManagedTrackingCannotBypassCampaignBudget(t *testing.T) {
	root := trackingCostFixture(t, 1)
	p := runfixture.Read(t, root).Plan
	if _, err := campaign.CampaignAdmit(filepath.Join(t.TempDir(), "absent"), p, filepath.Join(t.TempDir(), "run")); err == nil || !strings.Contains(err.Error(), "retain_unknown") {
		t.Fatalf("campaign tracking admission: %v", err)
	}
	p.APIVersion = model.GovernedPlanVersion
	p.Campaign = &model.CampaignBinding{Directory: t.TempDir(), CampaignID: strings.Repeat("a", 32), GrantID: strings.Repeat("b", 32)}
	if err := checkpoint.ValidateCampaignPlan(p); err == nil || !strings.Contains(err.Error(), "retain_unknown") {
		t.Fatalf("campaign tracking validation: %v", err)
	}
}

func TestManagedTrackingBillingReceiptDoesNotFreeze(t *testing.T) {
	t.Setenv("IRRIGATOR_BILLING_REGISTRY", filepath.Join(t.TempDir(), "registry"))
	previous := trackingCostFixture(t, 2)
	p := runfixture.Read(t, previous).Plan
	p.APIVersion = launch.ArtifactPlanVersion
	root := filepath.Join(t.TempDir(), "billing-track")
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
	if err := cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	held := s.Execution.Reservations[a.Fence.OperationID]
	receipt := model.BillingReceipt{Schema: "irrigator.billing/v1", RunID: s.RunID, TaskID: "a", OperationID: held.OperationID, Attempt: held.Attempt, BindingDigest: held.BindingDigest, ReceiptID: "tracking-fixture", Currency: "USD", AmountMicros: 500000, EvidenceSHA256: strings.Repeat("a", 64), EvidenceReference: "synthetic fixture", Attestation: "operator_verified_external_billing"}
	if err := provider.ReconcileBilling(root, receipt); err != nil {
		t.Fatal(err)
	}
	s = runfixture.Read(t, root)
	if s.Execution.Frozen || s.Execution.Reservations[held.OperationID].ReservedMicros != 0 {
		t.Fatal("tracking billing created financial gate")
	}
	if _, err := cells.BeginAttempt("b", "", 123); err != nil {
		t.Fatalf("verified charge blocked admission: %v", err)
	}
}

func TestManagedTrackingWorkerAcceptsMissingCost(t *testing.T) {
	b, _, _, _ := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"}}'`)
	catalog := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{b.AdapterName: b.Adapter}, Instances: map[string]model.ExecutionInstance{b.InstanceName: b.Instance}, Profiles: map[string]model.ExecutionProfile{b.ProfileName: b.Profile}, DefaultProfile: b.ProfileName}
	p := model.Plan{APIVersion: launch.UnknownCostPlanVersion, Name: "tracking worker fixture", MaxTasks: 2, TurnShape: &model.TurnShape{Roots: 2, MainTurns: 1}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{b.ProfileName}, CostPolicy: "track", RunBudgetUSD: "0"}}
	for _, id := range []string{"a", "b"} {
		p.Tasks = append(p.Tasks, model.Task{ID: id, Prompt: "Return ok", Requires: []string{"answer"}, Equals: map[string]string{"answer": "ok"}, Turn: &model.TurnIdentity{AgentID: id, Role: "main", Index: 1}})
	}
	root := filepath.Join(t.TempDir(), "worker-track")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := worker(root, id, model.Options{Timeout: 5 * time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	s := runfixture.Read(t, root)
	for _, row := range s.Tasks {
		if row.Status != "verified" || !row.CostUnknown {
			t.Fatalf("worker lost completed unknown-cost result: %+v", row)
		}
		if r := s.Execution.Reservations[row.OperationID]; r.Status != "unknown_cost" || r.ReservedMicros != 0 {
			t.Fatalf("worker held tracking allowance: %+v", r)
		}
	}
}
