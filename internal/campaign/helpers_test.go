package campaign

import (
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Reconstruct a historical admitted snapshot directly, because new admission
// rejects ambiguous declarations. The snapshot keeps valid campaign authority,
// managed bindings and the original item-name scheme so controller resume
// exercises compatibility.
func historicalChannelPublishRun(t *testing.T, fields ...string) (string, *model.State) {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	campaign, root := filepath.Join(dir, "campaign"), filepath.Join(dir, "run")
	if err := CampaignInit(campaign, "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := CampaignAdmit(campaign, runfixture.CampaignTestPlan(t, "0.25"), root); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	spec := &s.Plan.Tasks[0]
	spec.Requires = append(spec.Requires, "summary")
	s.Tasks[spec.ID].Spec = *spec
	s.Tasks[spec.ID].Status = "verified"
	s.Tasks[spec.ID].Result = map[string]string{"answer": "first field", "summary": "second field"}
	s.Plan.Experiments = &model.PlanExperiments{Messaging: &model.MessagingExperiment{Enabled: true, CrossSwarmGate: "review"}}
	for _, field := range fields {
		s.Plan.Experiments.Messaging.Publish = append(s.Plan.Experiments.Messaging.Publish, model.ChannelPublish{Channel: "updates", Task: spec.ID, Field: field})
	}
	s.PlanHash = launch.ExecutionDigest(s.Plan)
	store, err := checkpoint.OpenCampaign(campaign)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := store.Read()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	grant := ledger.Grants[s.Plan.Campaign.GrantID]
	grant.PlanHash = s.PlanHash
	ledger.Grants[grant.ID] = grant
	err = store.Write(ledger)
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	// Generic validation is deliberately historical-compatible.
	if err := checkpoint.Validate(s.Plan); err != nil {
		t.Fatalf("historical plan rejected by general validation: %v", err)
	}
	if err := checkpoint.ValidateCampaignAuthority(root, s.Plan); err != nil {
		t.Fatal(err)
	}
	return root, runfixture.Read(t, root)
}
