//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func campaignFixture(t *testing.T) (string, model.Plan, string) {
	t.Helper()
	base := runfixture.CompositionTempDir(t)
	dir := filepath.Join(base, "campaign")
	if e := campaign.CampaignInit(dir, "1"); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, runfixture.MailboxFixture(t))
	p := s.Plan
	p.Execution.RunBudgetUSD = "1"
	return dir, p, base
}

func TestCampaignCloneMissingLedgerAndClose(t *testing.T) {
	dir, p, base := campaignFixture(t)
	run := filepath.Join(base, "run")
	if _, e := campaign.CampaignAdmit(dir, p, run); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, run)
	if e := checkpoint.ValidateCampaignAuthority(filepath.Join(base, "clone"), s.Plan); e == nil {
		t.Fatal("cloned run inherited campaign authority")
	}
	ledger := filepath.Join(dir, "campaign.json")
	saved := ledger + ".saved"
	if e := os.Rename(ledger, saved); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.SetControl(run, "run"); e == nil {
		t.Fatal("missing ledger permitted mutation")
	}
	if e := os.Rename(saved, ledger); e != nil {
		t.Fatal(e)
	}
	if e := campaign.CampaignClose(run); e == nil {
		t.Fatal("nonterminal grant released")
	}
	if e := checkpoint.Update(run, func(s *model.State) error { s.Tasks["a"].Status = "cancelled"; return nil }); e != nil {
		t.Fatal(e)
	}
	if e := campaign.CampaignClose(run); e != nil {
		t.Fatal(e)
	}
	if e := retryNode(run, "a", false, true); e == nil {
		t.Fatal("closed grant allowed retry")
	}
	if _, e := runCommand([]string{"resume", run}); e == nil {
		t.Fatal("closed grant allowed resume")
	}
	if e := checkpoint.SetControl(run, "run"); e == nil {
		t.Fatal("closed grant allowed mutation")
	}
	if _, e := campaign.CampaignAdmit(dir, p, filepath.Join(base, "next")); e != nil {
		t.Fatalf("settled zero-cost grant failed to release allowance: %v", e)
	}
}
