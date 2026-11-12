package messaging

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestMailboxTransactionalReplayAndAcknowledgement(t *testing.T) {
	root := runfixture.MailboxFixture(t)
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	m := runfixture.MailboxMessageFor(s, "signalOne", "signal")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seq, e := EnqueueMailbox(root, m)
			if e == nil && seq != 1 {
				e = fmt.Errorf("duplicate admitted sequence %d", seq)
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if err = ProcessMailbox(root, 10); err != nil {
		t.Fatal(err)
	}
	first, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Mailbox.Entries) != 1 || first.Mailbox.Cursor != 1 || len(first.Mailbox.Outbox) != 1 || first.Tasks["a"].Status != "pending" {
		t.Fatal("signal changed task or broke atomic receipt")
	}
	if err = ProcessMailbox(root, 10); err != nil {
		t.Fatal(err)
	}
	second, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("processing replay changed state")
	}
	if err = AcknowledgeMailbox(root, 1); err != nil {
		t.Fatal(err)
	}
	if err = AcknowledgeMailbox(root, 1); err != nil {
		t.Fatal(err)
	}
	m.Payload = "changed"
	if _, err = EnqueueMailbox(root, m); err == nil {
		t.Fatal("conflicting duplicate accepted after ack")
	}
	if err = AcknowledgeMailbox(root, 2); err == nil {
		t.Fatal("future acknowledgement accepted")
	}
}
func TestMailboxStaleFenceAndLateCancellation(t *testing.T) {
	for _, status := range []string{"pending", "running", "produced", "uncertain", "verified"} {
		t.Run(status, func(t *testing.T) {
			s := &model.State{Version: 4, RunID: "run", Plan: model.Plan{APIVersion: model.GovernedPlanVersion}, Mailbox: checkpoint.NewMailboxState(), Tasks: map[string]*model.TaskState{"a": {Spec: model.Task{ID: "a"}, Status: status, Generation: 1, OperationID: model.OperationID("run", "a", 1), Attempt: 2, Result: map[string]string{"answer": "retained"}, CostUnknown: true}}}
			stale := runfixture.MailboxMessageFor(s, "old", "cancel")
			stale.Attempt--
			if _, e := admitMailbox(s, stale); e != nil {
				t.Fatal(e)
			}
			live := runfixture.MailboxMessageFor(s, "live", "cancel")
			if _, e := admitMailbox(s, live); e != nil {
				t.Fatal(e)
			}
			if e := reduceMailbox(s, 1); e != nil {
				t.Fatal(e)
			}
			if s.Tasks["a"].CancelRequested || s.Mailbox.Entries[0].Receipt.Status != "stale" {
				t.Fatal("stale message mutated task")
			}
			if e := reduceMailbox(s, 1); e != nil {
				t.Fatal(e)
			}
			r := s.Tasks["a"]
			if status == "pending" && r.Status != "cancelled" {
				t.Fatal("pending cancellation not applied")
			}
			if status != "pending" && r.Status != status {
				t.Fatal("late cancel destroyed outcome state")
			}
			if r.Result["answer"] != "retained" || !r.CostUnknown {
				t.Fatal("cancel erased uncertainty/evidence")
			}
			if status == "verified" && r.CancelRequested {
				t.Fatal("verified task mutated")
			}
		})
	}
}
func TestMailboxCapacityTombstonesAndCorruption(t *testing.T) {
	s := &model.State{Version: 4, RunID: "run", Plan: model.Plan{APIVersion: model.GovernedPlanVersion}, Mailbox: checkpoint.NewMailboxState(), Tasks: map[string]*model.TaskState{"a": {Spec: model.Task{ID: "a"}, Generation: 1, OperationID: model.OperationID("run", "a", 1), Status: "pending"}}}
	for i := 0; i < checkpoint.MailboxCapacity; i++ {
		if _, err := admitMailbox(s, runfixture.MailboxMessageFor(s, fmt.Sprintf("key%d", i), "signal")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admitMailbox(s, runfixture.MailboxMessageFor(s, "overflow", "signal")); err == nil {
		t.Fatal("capacity expanded")
	}
	if err := reduceMailbox(s, checkpoint.MailboxCapacity); err != nil {
		t.Fatal(err)
	}
	if seq, err := admitMailbox(s, runfixture.MailboxMessageFor(s, "key0", "signal")); err != nil || seq != 1 {
		t.Fatal("retained duplicate failed", seq, err)
	}
	s.Mailbox.Outbox[0].Digest = "corrupt"
	if err := checkpoint.ValidateMailboxState(s); err == nil {
		t.Fatal("outbox tampering accepted")
	}
	s.Plan.APIVersion = launch.UnknownCostPlanVersion
	if err := checkpoint.ValidateMailboxState(s); err == nil {
		t.Fatal("legacy mailbox accepted")
	}
}
