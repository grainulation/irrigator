package managed

import (
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func snapshotAdmissionFixture(t *testing.T, tracking bool) *model.State {
	t.Helper()
	catalog := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema,
		Adapters:  map[string]model.ExecutionAdapter{"local": {Kind: "ollama_http", Protocol: model.ProtocolVersion, Endpoint: "http://127.0.0.1:11434"}},
		Instances: map[string]model.ExecutionInstance{"local": {Adapter: "local", CredentialRefs: []model.ExecutionCredentialRef{}}},
		Profiles:  map[string]model.ExecutionProfile{"local": {Instance: "local", Model: "fixture:latest", ModelDigest: strings.Repeat("a", 64), Effort: "none", RequiredCapabilities: []string{"structured_strings", "outcome_certainty"}, Limits: model.ExecutionLimits{InputBytes: 1024, OutputBytes: 1024}, Tools: []string{}, OperationBudgetUSD: "0.25"}}, DefaultProfile: "local"}
	policy := "retain_unknown"
	if tracking {
		policy = "track"
	}
	p := model.Plan{APIVersion: launch.ArtifactPlanVersion, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"local"}, CostPolicy: policy, RunBudgetUSD: "1"}}
	e, err := checkpoint.InitializeManagedExecution(p)
	if err != nil {
		t.Fatal(err)
	}
	s := &model.State{Version: 5, RunID: "snapshot", Plan: p, Backend: "managed", Execution: e, Tasks: map[string]*model.TaskState{}}
	for _, id := range []string{"first", "second"} {
		task := model.Task{ID: id, Profile: "local"}
		s.Tasks[id] = s.NewTask(task)
	}
	return s
}

func TestSnapshotAdmissionMatchesFreshChecks(t *testing.T) {
	for _, tracking := range []bool{false, true} {
		for _, name := range []string{"allowed", "frozen", "exhausted", "existing-held", "existing-unknown", "existing-uncertain", "bad-binding", "missing-binding", "billing-authority"} {
			t.Run(name+map[bool]string{false: "/enforced", true: "/tracking"}[tracking], func(t *testing.T) {
				s := snapshotAdmissionFixture(t, tracking)
				row := s.Tasks["first"]
				switch name {
				case "frozen":
					s.Execution.Frozen = true
				case "exhausted":
					s.Execution.CeilingMicros = 1
				case "existing-held", "existing-unknown", "existing-uncertain":
					status := map[string]string{"existing-held": "held", "existing-unknown": "unknown_cost", "existing-uncertain": "uncertain"}[name]
					s.Execution.Reservations[row.OperationID] = model.ManagedReservation{OperationID: row.OperationID, TaskID: row.Spec.ID, Attempt: 1, BindingDigest: row.BindingDigest, ReservedMicros: 250000, Status: status}
				case "bad-binding":
					b := s.Execution.Bindings["local"]
					b.Profile.Model = "modified"
					s.Execution.Bindings["local"] = b
				case "missing-binding":
					row.BindingDigest = "unknown"
				case "billing-authority":
					s.Execution.BillingReceipts = map[string]model.BillingReceipt{"receipt": {ReceiptID: "r"}}
				}
				check := SnapshotAdmissionCheck(s)
				for _, id := range []string{"first", "second", "first"} {
					want, got := ManagedCanBegin(s, s.Tasks[id]), check(s.Tasks[id])
					if (want == nil) != (got == nil) || want != nil && want.Error() != got.Error() {
						t.Fatalf("%s mismatch: fresh=%v snapshot=%v", id, want, got)
					}
				}
			})
		}
	}
}
