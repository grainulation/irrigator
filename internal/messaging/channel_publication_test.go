package messaging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestChannelPublicationDoesNotCreateThroughSymlink(t *testing.T) {
	campaign, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.Symlink(outside, filepath.Join(campaign, "channels")); err != nil {
		t.Fatal(err)
	}
	s := runfixture.MessagingState(t, &model.MessagingExperiment{Enabled: true, Publish: []model.ChannelPublish{{Channel: "updates", Task: "research", Field: "finding"}}})
	s.Plan.Campaign = &model.CampaignBinding{Directory: campaign, CampaignID: "c", GrantID: "g"}
	s.Tasks["research"].Status = "verified"
	s.Tasks["research"].Result = map[string]string{"finding": "verified"}
	encoded, _ := json.Marshal(s.Plan)
	s.PlanHash = NoteDigest(string(encoded))
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if err := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if err := PublishChannels(root); err == nil {
		t.Fatal("accepted linked channels directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("created external directories: %v %v", entries, err)
	}
}

func TestChannelPublicationIsAtomicAndNeverReplaces(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	// An interrupted temporary write is ignored; publication remains repairable.
	if err := os.WriteFile(filepath.Join(dir, ".publish-interrupted"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("verified", 8192))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := publishChannelItem(dir, "item.json", payload); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := publishChannelItem(dir, "item.json", []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "item.json"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("publication changed or incomplete: %d bytes %v", len(got), err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temporary publications leaked: %v", entries)
	}
}
