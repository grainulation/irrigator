package runops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/campaign"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestCampaignCompetingAdmissions(t *testing.T) {
	dir, p, base := campaignFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, e := campaign.CampaignAdmit(dir, p, filepath.Join(base, fmt.Sprintf("run%d", i)))
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	admitted := 0
	for e := range results {
		if e == nil {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("expected exactly one whole-budget grant, got %d", admitted)
	}
	c, e := checkpoint.OpenCampaign(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	l, e := c.Read()
	if e != nil {
		t.Fatal(e)
	}
	if len(l.Grants) != 1 {
		t.Fatalf("failed admissions mutated grants: %d", len(l.Grants))
	}
}

func TestCampaignUnknownHoldRetained(t *testing.T) {
	dir, p, base := campaignFixture(t)
	run := filepath.Join(base, "run")
	if _, e := campaign.CampaignAdmit(dir, p, run); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.SetControl(run, "run"); e != nil {
		t.Fatal(e)
	}
	cells := executor.CheckpointCells{Root: run}
	a, e := cells.BeginAttempt("a", "", 123)
	if e != nil {
		t.Fatal(e)
	}
	if e = cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); e != nil {
		t.Fatal(e)
	}
	if e = cells.RecordValidation(executor.CellValidation{Fence: a.Fence, Level: "validated"}); e != nil {
		t.Fatal(e)
	}
	if e = campaign.CampaignClose(run); e == nil {
		t.Fatal("unknown cost released campaign grant")
	}
	if _, e = campaign.CampaignAdmit(dir, p, filepath.Join(base, "next")); e == nil {
		t.Fatal("unknown cost allowance reused")
	}
	s := runfixture.Read(t, run)
	if !s.Tasks["a"].CostUnknown || s.Execution.Reservations[s.Tasks["a"].OperationID].Status != "unknown_cost" {
		t.Fatal("unknown cost evidence altered")
	}
}

func TestMailboxEveryFenceAndAtomicReplay(t *testing.T) {
	for _, field := range []string{"run", "task", "operation", "generation", "attempt"} {
		t.Run(field, func(t *testing.T) {
			root := runfixture.MailboxFixture(t)
			before := runfixture.Read(t, root)
			m := runfixture.MailboxMessageFor(before, "stale", "cancel")
			switch field {
			case "run":
				m.RunID += "other"
			case "task":
				m.TaskID = "other"
			case "operation":
				m.OperationID = strings.Repeat("0", 64)
			case "generation":
				m.Generation++
			case "attempt":
				m.Attempt++
			}
			if _, e := messaging.EnqueueMailbox(root, m); e != nil {
				t.Fatal(e)
			}
			if e := messaging.ProcessMailbox(root, 1); e != nil {
				t.Fatal(e)
			}
			after := runfixture.Read(t, root)
			if !reflect.DeepEqual(before.Tasks, after.Tasks) || after.Mailbox.Outbox[0].Status != "stale" {
				t.Fatal("stale command altered work")
			}
			if e := messaging.AcknowledgeMailbox(root, 1); e != nil {
				t.Fatal(e)
			}
			if seq, e := messaging.EnqueueMailbox(root, m); e != nil || seq != 1 {
				t.Fatal("acknowledged tombstone lost")
			}
			if e := messaging.ProcessMailbox(root, 1); e != nil {
				t.Fatal(e)
			}
			final := runfixture.Read(t, root)
			if len(final.Mailbox.Outbox) != 1 || !final.Mailbox.Outbox[0].Acknowledged {
				t.Fatal("replay duplicated receipt")
			}
		})
	}
}

func TestMailboxConcurrentReducersLateUnknownOutcome(t *testing.T) {
	root := runfixture.MailboxFixture(t)
	if e := checkpoint.SetControl(root, "run"); e != nil {
		t.Fatal(e)
	}
	cells := executor.CheckpointCells{Root: root}
	a, e := cells.BeginAttempt("a", "", 123)
	if e != nil {
		t.Fatal(e)
	}
	m := runfixture.MailboxMessageFor(runfixture.Read(t, root), "cancel_running", "cancel")
	if _, e = messaging.EnqueueMailbox(root, m); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- messaging.ProcessMailbox(root, 1) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	s := runfixture.Read(t, root)
	if len(s.Mailbox.Outbox) != 1 || s.Tasks["a"].Status != "running" || !s.Tasks["a"].CancelRequested {
		t.Fatal("running cancel altered evidence or duplicated effects")
	}
	if e = cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "late result"}}); e != nil {
		t.Fatal(e)
	}
	s = runfixture.Read(t, root)
	row := s.Tasks["a"]
	if row.Result["answer"] != "late result" || !row.CostUnknown || s.Execution.Reservations[row.OperationID].Status != "unknown_cost" {
		t.Fatal("late cancellation erased result or unknown exposure")
	}
}

func TestMailboxCorruptDiskFailsClosed(t *testing.T) {
	cases := map[string]func(*model.State){
		"cursor":        func(s *model.State) { s.Mailbox.Cursor = 0 },
		"outbox_digest": func(s *model.State) { s.Mailbox.Outbox[0].Digest = "wrong" },
		"entry_message": func(s *model.State) { s.Mailbox.Entries[0].Message.Payload = "changed" },
		"receipt":       func(s *model.State) { s.Mailbox.Entries[0].Receipt = nil },
		"legacy_schema": func(s *model.State) { s.Version = 3 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			root := runfixture.MailboxFixture(t)
			s := runfixture.Read(t, root)
			if _, e := messaging.EnqueueMailbox(root, runfixture.MailboxMessageFor(s, "one", "signal")); e != nil {
				t.Fatal(e)
			}
			if e := messaging.ProcessMailbox(root, 1); e != nil {
				t.Fatal(e)
			}
			s = runfixture.Read(t, root)
			mutate(s)
			if e := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); e != nil {
				t.Fatal(e)
			}
			if _, e := checkpoint.ReadState(root); e == nil {
				t.Fatal("corrupt durable mailbox accepted")
			}
			if e := messaging.ProcessMailbox(root, 1); e == nil {
				t.Fatal("corrupt durable mailbox processed")
			}
		})
	}
}

func TestClosedCampaignGCPreservesFiles(t *testing.T) {
	dir, p, base := campaignFixture(t)
	run := filepath.Join(base, "run")
	if _, e := campaign.CampaignAdmit(dir, p, run); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.Update(run, func(s *model.State) error { s.Tasks["a"].Attempt = 3; s.Tasks["a"].Status = "cancelled"; return nil }); e != nil {
		t.Fatal(e)
	}
	victim := filepath.Join(run, "cells", "a", "1.stdout.json")
	if e := os.WriteFile(victim, []byte("retained"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := campaign.CampaignClose(run); e != nil {
		t.Fatal(e)
	}
	if _, e := GarbageCollect(run, 1, true); e == nil {
		t.Fatal("closed campaign gc accepted")
	}
	b, e := os.ReadFile(victim)
	if e != nil || string(b) != "retained" {
		t.Fatalf("closed campaign gc deleted evidence before authority refusal: %v", e)
	}
}

func TestCampaignBackupPreservesExternalEvidenceOnly(t *testing.T) {
	dir, p, base := campaignFixture(t)
	run := filepath.Join(base, "run")
	if _, e := campaign.CampaignAdmit(dir, p, run); e != nil {
		t.Fatal(e)
	}
	if _, e := messaging.EnqueueMailbox(run, runfixture.MailboxMessageFor(runfixture.Read(t, run), "pending_signal", "signal")); e != nil {
		t.Fatal(e)
	}
	ledgerBefore, e := os.ReadFile(filepath.Join(dir, "campaign.json"))
	if e != nil {
		t.Fatal(e)
	}
	bundle := filepath.Join(base, "bundle.json")
	if e = backup.BackupCreate(run, bundle); e != nil {
		t.Fatal(e)
	}
	m, e := backup.BackupRead(bundle)
	if e != nil {
		t.Fatal(e)
	}
	if m.Schema != "irrigator.backup/v2" || m.Campaign == nil {
		t.Fatal("campaign evidence absent")
	}
	clone := filepath.Join(base, "clone")
	if e = backup.BackupRestore(bundle, clone); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(clone, "CAMPAIGN_EVIDENCE.json")); e != nil {
		t.Fatal(e)
	}
	restored := runfixture.Read(t, filepath.Join(clone, "evidence"))
	if len(restored.Mailbox.Entries) != 1 || restored.Mailbox.Cursor != 0 {
		t.Fatal("pending mailbox lost")
	}
	if _, e := messaging.EnqueueMailbox(filepath.Join(clone, "evidence"), runfixture.MailboxMessageFor(restored, "new", "signal")); e == nil {
		t.Fatal("restored mailbox mutation accepted")
	}
	ledgerAfter, e := os.ReadFile(filepath.Join(dir, "campaign.json"))
	if e != nil || !reflect.DeepEqual(ledgerBefore, ledgerAfter) {
		t.Fatal("restore mutated source campaign authority")
	}
	m.Campaign = nil
	raw, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	missing := filepath.Join(base, "missing.json")
	if e = os.WriteFile(missing, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = backup.BackupRead(missing); e == nil {
		t.Fatal("missing external campaign evidence accepted")
	}
}

func TestGCRetainsHistoricalUnsettledAttempts(t *testing.T) {
	for _, status := range []string{"held", "uncertain", "unknown_cost"} {
		t.Run(status, func(t *testing.T) {
			root := runfixture.MailboxFixture(t)
			if e := checkpoint.SetControl(root, "run"); e != nil {
				t.Fatal(e)
			}
			cells := executor.CheckpointCells{Root: root}
			first, e := cells.BeginAttempt("a", "", 123)
			if e != nil {
				t.Fatal(e)
			}
			if status != "held" {
				o := managed.CellOutcome{Fence: first.Fence, Result: map[string]string{"answer": "first"}}
				if status == "uncertain" {
					o.CleanupUnresolved = true
				} else {
					o.CostUnknown = true
				}
				if e = cells.RecordOutcome(o); e != nil {
					t.Fatal(e)
				}
			}
			if e = retryNode(root, "a", false, true); e != nil {
				t.Fatal(e)
			}
			second, e := cells.BeginAttempt("a", "", 123)
			if e != nil {
				t.Fatal(e)
			}
			if e = cells.RecordOutcome(managed.CellOutcome{Fence: second.Fence, Result: map[string]string{"answer": "second"}}); e != nil {
				t.Fatal(e)
			}
			if e = cells.RecordValidation(executor.CellValidation{Fence: second.Fence, Level: "validated"}); e != nil {
				t.Fatal(e)
			}
			s := runfixture.Read(t, root)
			if s.Tasks["a"].Attempt != 2 {
				t.Fatal("fixture did not produce old attempt")
			}
			old := filepath.Join(root, "cells", "a", "1.stderr.log")
			if e = os.WriteFile(old, []byte("unresolved operation evidence"), 0600); e != nil {
				t.Fatal(e)
			}
			if _, e = GarbageCollect(root, 1, true); e != nil {
				t.Fatal(e)
			}
			b, e := os.ReadFile(old)
			if e != nil || string(b) != "unresolved operation evidence" {
				t.Fatalf("gc erased %s historical attempt: %v", status, e)
			}
		})
	}
}

func TestCampaignCloseRequiresMailboxDrain(t *testing.T) {
	dir, p, base := campaignFixture(t)
	run := filepath.Join(base, "run")
	if _, e := campaign.CampaignAdmit(dir, p, run); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, run)
	if _, e := messaging.EnqueueMailbox(run, runfixture.MailboxMessageFor(s, "terminal_signal", "signal")); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.Update(run, func(s *model.State) error { s.Tasks["a"].Status = "cancelled"; return nil }); e != nil {
		t.Fatal(e)
	}
	if e := campaign.CampaignClose(run); e == nil {
		t.Fatal("campaign closed with accepted unprocessed mailbox command")
	}
	if e := messaging.ProcessMailbox(run, 1); e != nil {
		t.Fatalf("pending message lost processing authority: %v", e)
	}
	if e := campaign.CampaignClose(run); e == nil {
		t.Fatal("campaign closed with unacknowledged durable outbox")
	}
	if e := messaging.AcknowledgeMailbox(run, 1); e != nil {
		t.Fatalf("outbox lost acknowledgement authority: %v", e)
	}
	if e := campaign.CampaignClose(run); e != nil {
		t.Fatalf("fully drained terminal campaign could not close: %v", e)
	}
}
