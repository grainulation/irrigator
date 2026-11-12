//go:build integration

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	campaignpkg "github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

// Reconstruct a historical admitted snapshot directly, because new admission
// rejects ambiguous declarations. The snapshot keeps valid campaign authority,
// managed bindings and the original item-name scheme so controller resume
// exercises compatibility.
func historicalChannelPublishRun(t *testing.T, fields ...string) (string, *model.State) {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	campaign, root := filepath.Join(dir, "campaign"), filepath.Join(dir, "run")
	if err := campaignpkg.CampaignInit(campaign, "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := campaignpkg.CampaignAdmit(campaign, runfixture.CampaignTestPlan(t, "0.25"), root); err != nil {
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

func TestChannelPublishHistoricalAmbiguityFailsClosedOnResume(t *testing.T) {
	root, before := historicalChannelPublishRun(t, "answer", "summary")
	itemPath := runfixture.ChannelPublicationPath(before)
	if err := os.MkdirAll(filepath.Dir(itemPath), 0700); err != nil {
		t.Fatal(err)
	}
	// An old publisher may already have saved the first field. Keep its exact
	// bytes, accepted result and plan hash even though the next field conflicts.
	first := messaging.ChannelItem{Channel: "updates", ID: strings.TrimSuffix(filepath.Base(itemPath), ".json"), RunID: before.RunID, Task: before.Plan.Tasks[0].ID, Field: "answer", Text: "first field", SHA256: messaging.NoteDigest("first field"), Status: "pending", Created: "2025-01-01T00:00:00Z"}
	original, _ := json.Marshal(first)
	if err := os.WriteFile(itemPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runops.Run(root, model.Options{Workers: 1, Timeout: time.Second, Budget: 1}); err == nil || !strings.Contains(err.Error(), "channel publication conflict") {
		t.Fatalf("historical resume silently accepted output loss: %v", err)
	}
	after := runfixture.Read(t, root)
	if after.PlanHash != before.PlanHash || !reflect.DeepEqual(after.Plan, before.Plan) || !reflect.DeepEqual(after.Tasks, before.Tasks) {
		t.Fatal("historical conflict changed immutable plan or accepted tasks")
	}
	got, _ := os.ReadFile(itemPath)
	if !bytes.Equal(original, got) {
		t.Fatal("historical conflict replaced first publication")
	}
	items, err := os.ReadDir(filepath.Dir(itemPath))
	if err != nil || len(items) != 1 {
		t.Fatalf("historical conflict renamed or added publications: %v %v", items, err)
	}
}

func TestChannelPublishHistoricalRepairPreservesApproval(t *testing.T) {
	root, before := historicalChannelPublishRun(t, "answer")
	if _, err := runops.Run(root, model.Options{Workers: 1, Timeout: time.Second, Budget: 1}); err != nil {
		t.Fatalf("historical missed publication was not repaired on resume: %v", err)
	}
	itemPath := runfixture.ChannelPublicationPath(before)
	id := strings.TrimSuffix(filepath.Base(itemPath), ".json")
	if err := messaging.ApproveChannelItem(before.Plan.Campaign.Directory, "updates", id); err != nil {
		t.Fatal(err)
	}
	approved, _ := os.ReadFile(itemPath)
	if _, err := runops.Run(root, model.Options{Workers: 1, Timeout: time.Second, Budget: 1}); err != nil {
		t.Fatalf("approved publication rejected by idempotent resume: %v", err)
	}
	got, _ := os.ReadFile(itemPath)
	if !bytes.Equal(approved, got) {
		t.Fatal("repair changed publication timestamp or approval")
	}
	after := runfixture.Read(t, root)
	if after.PlanHash != before.PlanHash || !reflect.DeepEqual(after.Plan, before.Plan) || !reflect.DeepEqual(after.Tasks, before.Tasks) {
		t.Fatal("repair rewrote admitted plan or accepted tasks")
	}
}

func TestChannelPublishHistoricalRepeatedSameFieldResumes(t *testing.T) {
	root, before := historicalChannelPublishRun(t, "answer", "answer")
	if _, err := runops.Run(root, model.Options{Workers: 1, Timeout: time.Second, Budget: 1}); err != nil {
		t.Fatalf("harmless repeated declaration broke historical resume: %v", err)
	}
	items, err := messaging.ReadChannel(before.Plan.Campaign.Directory, "updates")
	if err != nil || len(items) != 1 || items[0].Field != "answer" || items[0].Text != "first field" {
		t.Fatalf("repeated same-field publication was not idempotent: %v %v", items, err)
	}
	after := runfixture.Read(t, root)
	if after.PlanHash != before.PlanHash || !reflect.DeepEqual(after.Plan, before.Plan) || !reflect.DeepEqual(after.Tasks, before.Tasks) {
		t.Fatal("same-field repair rewrote admitted plan or accepted tasks")
	}
}

func channelCampaignAdmissionPlan(t *testing.T) model.Plan {
	t.Helper()
	p := runfixture.CampaignTestPlan(t, "0.25")
	p.Experiments = &model.PlanExperiments{Messaging: &model.MessagingExperiment{Enabled: true,
		Publish:   []model.ChannelPublish{{Channel: "updates", Task: p.Tasks[0].ID, Field: "answer"}},
		Subscribe: []model.ChannelSub{{Channel: "updates", Task: p.Tasks[0].ID}},
	}}
	return p
}

func TestChannelCampaignAdmissionCLIAndDirectPreserveInput(t *testing.T) {
	p := channelCampaignAdmissionPlan(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	campaign, input := filepath.Join(dir, "campaign"), filepath.Join(dir, "plan.json")
	if err := campaignpkg.CampaignInit(campaign, "1"); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(p)
	if err := os.WriteFile(input, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Validate(p); err == nil {
		t.Fatal("standalone validation accepted an unbound channel plan")
	}
	if _, err := checkpoint.LoadPlan(input); err == nil {
		t.Fatal("standalone plan loading accepted an unbound channel plan")
	}
	standalone := filepath.Join(dir, "standalone")
	if code, err := runCommand([]string{"prepare", input, "--run", standalone, "--backend", "managed"}); code == 0 || err == nil {
		t.Fatalf("standalone prepare accepted an unbound channel plan: %d %v", code, err)
	}
	if _, err := os.Lstat(standalone); !os.IsNotExist(err) {
		t.Fatalf("standalone rejection created a run: %v", err)
	}
	// Exercise actual CLI dispatch, parsing, plan loading and campaign admission,
	// not a pre-bound fixture or a mocked validation helper.
	cliRun := filepath.Join(dir, "cli-run")
	if code, err := runCommand([]string{"campaign", "admit", campaign, input, "--run", cliRun}); code != 0 || err != nil {
		t.Fatalf("valid unbound channel CLI admission failed: %d %v", code, err)
	}
	directRun := filepath.Join(dir, "direct-run")
	if _, err := campaignpkg.CampaignAdmit(campaign, p, directRun); err != nil {
		t.Fatalf("valid unbound channel direct admission failed: %v", err)
	}
	for _, root := range []string{cliRun, directRun} {
		s := runfixture.Read(t, root)
		if s.Plan.Campaign == nil || s.Plan.Campaign.Directory != campaign || s.PlanHash != launch.ExecutionDigest(s.Plan) || !reflect.DeepEqual(s.Plan.Experiments, p.Experiments) {
			t.Fatal("admitted plan lacks its real binding/hash or changed messaging declarations")
		}
		if err := checkpoint.Validate(s.Plan); err != nil {
			t.Fatalf("actual bound plan fails ordinary validation: %v", err)
		}
		if err := checkpoint.ValidateCampaignAuthority(root, s.Plan); err != nil {
			t.Fatalf("admitted run lacks real campaign authority: %v", err)
		}
	}
	got, _ := os.ReadFile(input)
	after, _ := json.Marshal(p)
	if !bytes.Equal(original, got) || !bytes.Equal(original, after) || p.Campaign != nil {
		t.Fatal("campaign admission mutated input plan file/object or injected placeholder binding")
	}
	store, err := checkpoint.OpenCampaign(campaign)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ledger, err := store.Read()
	if err != nil || len(ledger.Grants) != 2 {
		t.Fatalf("expected exactly two real grants: %+v %v", ledger, err)
	}
}

func TestChannelCampaignAdmissionInvalidPlansDoNotReserve(t *testing.T) {
	cases := map[string]func(*model.Plan){
		"invalid publish channel": func(p *model.Plan) { p.Experiments.Messaging.Publish[0].Channel = "Bad Name" },
		"unknown output field":    func(p *model.Plan) { p.Experiments.Messaging.Publish[0].Field = "missing" },
		"unknown publishing task": func(p *model.Plan) { p.Experiments.Messaging.Publish[0].Task = "missing" },
		"invalid subscription":    func(p *model.Plan) { p.Experiments.Messaging.Subscribe[0].Channel = "../escape" },
		"self note edge": func(p *model.Plan) {
			p.Experiments.Messaging.Edges = []model.MessageEdge{{From: p.Tasks[0].ID, To: p.Tasks[0].ID, Field: "answer"}}
		},
		"undeclared profile": func(p *model.Plan) { p.Execution.AllowedProfiles = []string{"missing"} },
		"invalid budget":     func(p *model.Plan) { p.Execution.RunBudgetUSD = "NaN" },
		"invalid root turn":  func(p *model.Plan) { p.Tasks[0].Turn.Index = 2 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p := channelCampaignAdmissionPlan(t)
			change(&p)
			dir, _ := filepath.EvalSymlinks(t.TempDir())
			campaign, input := filepath.Join(dir, "campaign"), filepath.Join(dir, "plan.json")
			if err := campaignpkg.CampaignInit(campaign, "1"); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(campaign, "campaign.json"))
			original, _ := json.Marshal(p)
			if err := os.WriteFile(input, original, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := campaignpkg.CampaignAdmit(campaign, p, filepath.Join(dir, "direct-run")); err == nil {
				t.Fatal("direct admission accepted invalid plan")
			}
			if code, err := runCommand([]string{"campaign", "admit", campaign, input, "--run", filepath.Join(dir, "cli-run")}); code == 0 || err == nil {
				t.Fatalf("CLI admission accepted invalid plan: %d %v", code, err)
			}
			after, _ := os.ReadFile(filepath.Join(campaign, "campaign.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("invalid campaign plan allocated or changed grant")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 2 {
				t.Fatalf("invalid admission created run or lock artifacts: %v %v", entries, err)
			}
			got, _ := os.ReadFile(input)
			if !bytes.Equal(original, got) {
				t.Fatal("invalid admission rewrote input plan")
			}
		})
	}
}
