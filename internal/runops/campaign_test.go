package runops

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestCampaignConcurrentGrantCeiling(t *testing.T) {
	d := t.TempDir()
	c := filepath.Join(d, "campaign")
	if e := campaign.CampaignInit(c, "0.25"); e != nil {
		t.Fatal(e)
	}
	p := runfixture.CampaignTestPlan(t, "0.25")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			_, e := campaign.CampaignAdmit(c, p, filepath.Join(d, n))
			results <- e
		}(name)
	}
	wg.Wait()
	close(results)
	count := 0
	for e := range results {
		if e == nil {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("admissions %d", count)
	}
	store, e := checkpoint.OpenCampaign(c)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	l, e := store.Read()
	if e != nil || len(l.Grants) != 1 {
		t.Fatalf("ledger %+v %v", l, e)
	}
}
func TestCampaignCloseRevokesExecutionAndClone(t *testing.T) {
	d := t.TempDir()
	c := filepath.Join(d, "campaign")
	r := filepath.Join(d, "run")
	if e := campaign.CampaignInit(c, "1"); e != nil {
		t.Fatal(e)
	}
	if _, e := campaign.CampaignAdmit(c, runfixture.CampaignTestPlan(t, "0.25"), r); e != nil {
		t.Fatal(e)
	}
	s, e := checkpoint.ReadState(r)
	if e != nil {
		t.Fatal(e)
	}
	if s.Version != 4 || s.Mailbox == nil {
		t.Fatal("v5 missing mailbox")
	}
	if e = checkpoint.ValidateCampaignAuthority(filepath.Join(d, "clone"), s.Plan); e == nil {
		t.Fatal("clone allowed")
	}
	if e = campaign.CampaignClose(r); e == nil {
		t.Fatal("closed pending work")
	}
	if e = CancelNode(r, "a"); e != nil {
		t.Fatal(e)
	}
	if e = campaign.CampaignClose(r); e != nil {
		t.Fatal(e)
	}
	if e = campaign.CampaignClose(r); e != nil {
		t.Fatal("idempotent close", e)
	}
	if e = checkpoint.SetControl(r, "run"); e == nil {
		t.Fatal("closed grant mutated")
	}
	if e = retryNode(r, "a", false, false); e == nil {
		t.Fatal("closed grant retried")
	}
	if _, e = campaign.CampaignAdmit(c, runfixture.CampaignTestPlan(t, "0.25"), r); e == nil {
		t.Fatal("existing run admitted")
	}
	if _, e = campaign.CampaignAdmit(c, runfixture.CampaignTestPlan(t, "1"), filepath.Join(d, "after")); e != nil {
		t.Fatal("released unused capacity not available", e)
	}
}
func TestCampaignRetainsUnknownAndMissingAuthority(t *testing.T) {
	d := t.TempDir()
	c := filepath.Join(d, "campaign")
	r := filepath.Join(d, "run")
	if e := campaign.CampaignInit(c, "1"); e != nil {
		t.Fatal(e)
	}
	if _, e := campaign.CampaignAdmit(c, runfixture.CampaignTestPlan(t, "0.25"), r); e != nil {
		t.Fatal(e)
	}
	if e := CancelNode(r, "a"); e != nil {
		t.Fatal(e)
	}
	// A valid managed uncertain reservation is not releasable even with a local
	// stopped process. Use a zero-budget mock reservation to test the policy.
	if e := checkpoint.Update(r, func(s *model.State) error {
		row := s.Tasks["a"]
		row.Status = "uncertain"
		row.Attempt = 1
		s.Execution.Reservations[row.OperationID] = model.ManagedReservation{TaskID: "a", OperationID: row.OperationID, Attempt: 1, BindingDigest: row.BindingDigest, Status: "uncertain"}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if e := campaign.CampaignClose(r); e == nil {
		t.Fatal("uncertainty released grant")
	}
	if e := os.Rename(filepath.Join(c, "campaign.json"), filepath.Join(c, "lost.json")); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.SetControl(r, "run"); e == nil {
		t.Fatal("missing authority admitted mutation")
	}
}
func TestCampaignLedgerStrictAndSymlink(t *testing.T) {
	d := t.TempDir()
	c := filepath.Join(d, "campaign")
	if e := campaign.CampaignInit(c, "1"); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(d, "alias")
	if e := os.Symlink(c, alias); e != nil {
		t.Fatal(e)
	}
	if s, e := checkpoint.OpenCampaign(alias); e == nil {
		s.Close()
		t.Fatal("symlink ledger allowed")
	}
	if e := os.WriteFile(filepath.Join(c, "campaign.json"), []byte(`{"schema":"irrigator.campaign/v1","schema":"other"}`), 0600); e != nil {
		t.Fatal(e)
	}
	store, e := checkpoint.OpenCampaign(c)
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	if _, e = store.Read(); e == nil {
		t.Fatal("duplicate JSON accepted")
	}
}
