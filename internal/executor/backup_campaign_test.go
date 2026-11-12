package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func backupCampaignFixture(t *testing.T) (string, string) {
	t.Helper()
	d := runfixture.BackupTemp(t)
	c := filepath.Join(d, "campaign")
	r := filepath.Join(d, "run")
	if e := campaign.CampaignInit(c, "1"); e != nil {
		t.Fatal(e)
	}
	if _, e := campaign.CampaignAdmit(c, runfixture.CampaignTestPlan(t, "0.25"), r); e != nil {
		t.Fatal(e)
	}
	return r, c
}
func TestBackupCampaignMailboxAndUnknownExposure(t *testing.T) {
	root, campaign := backupCampaignFixture(t)
	s := runfixture.Read(t, root)
	if _, err := messaging.EnqueueMailbox(root, runfixture.MailboxMessageFor(s, "one", "signal")); err != nil {
		t.Fatal(err)
	}
	if err := messaging.ProcessMailbox(root, 10); err != nil {
		t.Fatal(err)
	}
	cells := CheckpointCells{Root: root}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	a, err := cells.BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	if err = cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
	if err = cells.RecordValidation(CellValidation{Fence: a.Fence, Level: "validated"}); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root)
	ledgerBefore, err := os.ReadFile(filepath.Join(campaign, "campaign.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if err = backup.BackupCreate(root, bundle); err != nil {
		t.Fatal(err)
	}
	m, err := backup.BackupRead(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != "irrigator.backup/v2" || m.StateVersion != 4 || m.Campaign == nil {
		t.Fatal("missing versioned evidence")
	}
	var l checkpoint.CampaignLedger
	if err = json.Unmarshal(m.Campaign.Ledger.Data, &l); err != nil {
		t.Fatal(err)
	}
	g := l.Grants[before.Plan.Campaign.GrantID]
	if g.ReservedMicros != 250000 || g.Status != "reserved" {
		t.Fatal("grant exposure lost")
	}
	// Both original run and authority may be offline when inspecting/restoring.
	if err = os.Rename(campaign, campaign+"-offline"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(runfixture.BackupTemp(t), "restore")
	if err = backup.BackupRestore(bundle, dest); err != nil {
		t.Fatal(err)
	}
	after := runfixture.Read(t, filepath.Join(dest, "evidence"))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("checkpoint/mailbox/unknown exposure altered")
	}
	if _, err = os.Stat(filepath.Join(dest, "CAMPAIGN_EVIDENCE.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(campaign); !os.IsNotExist(err) {
		t.Fatal("original authority recreated")
	}
	ledgerAfter, _ := os.ReadFile(filepath.Join(campaign+"-offline", "campaign.json"))
	if string(ledgerBefore) != string(ledgerAfter) {
		t.Fatal("authority altered")
	}
	if _, err = messaging.EnqueueMailbox(filepath.Join(dest, "evidence"), runfixture.MailboxMessageFor(after, "two", "signal")); err == nil {
		t.Fatal("quarantine mailbox mutation allowed")
	}
}
func TestBackupCampaignEvidenceFailsClosed(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt", "wrong-grant", "wrong-root", "wrong-hash"} {
		t.Run(kind, func(t *testing.T) {
			r, c := backupCampaignFixture(t)
			p := filepath.Join(c, "campaign.json")
			switch kind {
			case "missing":
				os.Remove(p)
			case "corrupt":
				os.WriteFile(p, []byte("{"), 0600)
			default:
				store, e := checkpoint.OpenCampaign(c)
				if e != nil {
					t.Fatal(e)
				}
				l, e := store.Read()
				if e != nil {
					t.Fatal(e)
				}
				for id, g := range l.Grants {
					switch kind {
					case "wrong-grant":
						delete(l.Grants, id)
					case "wrong-root":
						g.RunDirectory = filepath.Join(runfixture.BackupTemp(t), "other")
						l.Grants[id] = g
					case "wrong-hash":
						g.PlanHash = string(make([]byte, 64))
						l.Grants[id] = g
					}
				}
				e = store.Write(l)
				store.Close()
				if e != nil {
					t.Fatal(e)
				}
			}
			if backup.BackupCreate(r, filepath.Join(runfixture.BackupTemp(t), "bundle")) == nil {
				t.Fatal("accepted invalid external ledger")
			}
		})
	}
	r, _ := backupCampaignFixture(t)
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if e := backup.BackupCreate(r, bundle); e != nil {
		t.Fatal(e)
	}
	m, e := backup.BackupRead(bundle)
	if e != nil {
		t.Fatal(e)
	}
	m.Campaign = nil
	b, _ := json.Marshal(m)
	os.WriteFile(bundle, b, 0600)
	if _, e = backup.BackupRead(bundle); e == nil {
		t.Fatal("accepted removed campaign evidence")
	}
}
