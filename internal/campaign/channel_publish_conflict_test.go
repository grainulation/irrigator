package campaign

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestChannelPublishAdmissionRejectsBeforeMutation(t *testing.T) {
	p := runfixture.CampaignTestPlan(t, "0.25")
	p.Tasks[0].Requires = append(p.Tasks[0].Requires, "summary")
	p.Experiments = &model.PlanExperiments{Messaging: &model.MessagingExperiment{Enabled: true, Publish: []model.ChannelPublish{
		{Channel: "updates", Task: p.Tasks[0].ID, Field: "answer"},
		{Channel: "updates", Task: p.Tasks[0].ID, Field: "summary"},
	}}}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	campaign, root := filepath.Join(dir, "campaign"), filepath.Join(dir, "run")
	if err := CampaignInit(campaign, "1"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(campaign, "campaign.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = CampaignAdmit(campaign, p, root); err == nil || !strings.Contains(err.Error(), "multiple publications") {
		t.Fatalf("ambiguous campaign admission: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(campaign, "campaign.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected admission changed campaign grant ledger: %v", err)
	}
	if err = checkpoint.Initialize(p, root, "managed"); err == nil || !strings.Contains(err.Error(), "multiple publications") {
		t.Fatalf("direct initialization accepted ambiguous declarations: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 || entries[0].Name() != "campaign" {
		t.Fatalf("rejected admission created run or lock artifacts: %v %v", entries, err)
	}
	input := filepath.Join(dir, "plan.json")
	raw, _ := json.Marshal(p)
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.LoadPlan(input); err == nil || !strings.Contains(err.Error(), "multiple publications") {
		t.Fatalf("new plan load accepted ambiguous declarations: %v", err)
	}
	got, _ := os.ReadFile(input)
	if !bytes.Equal(raw, got) {
		t.Fatal("new-plan rejection rewrote input")
	}
}

func TestChannelPublishConflictingExistingPayloadIsPreserved(t *testing.T) {
	root, s := historicalChannelPublishRun(t, "answer")
	itemPath := runfixture.ChannelPublicationPath(s)
	if err := os.MkdirAll(filepath.Dir(itemPath), 0700); err != nil {
		t.Fatal(err)
	}
	// Same historical item name and valid digest, different declared field.
	conflict := messaging.ChannelItem{Channel: "updates", ID: strings.TrimSuffix(filepath.Base(itemPath), ".json"), RunID: s.RunID, Task: s.Plan.Tasks[0].ID, Field: "summary", Text: "second field", SHA256: messaging.NoteDigest("second field"), Status: "open"}
	original, _ := json.Marshal(conflict)
	if err := os.WriteFile(itemPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if err := messaging.PublishChannels(root); err == nil || !strings.Contains(err.Error(), "channel publication conflict") {
		t.Fatalf("existing different output silently accepted: %v", err)
	}
	got, _ := os.ReadFile(itemPath)
	if !bytes.Equal(original, got) {
		t.Fatal("existing conflicting publication overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if !bytes.Equal(checkpoint, after) {
		t.Fatal("publication conflict changed checkpoint")
	}
}
