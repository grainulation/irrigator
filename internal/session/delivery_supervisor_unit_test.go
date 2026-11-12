package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// integrationHold takes an exclusive nonblocking flock; the returned release is idempotent.
func integrationHold(t *testing.T, path string) func() {
	t.Helper()
	f, err := checkpoint.FileLock(path, true)
	if err != nil {
		t.Fatal(err)
	}
	return func() { checkpoint.Unlock(f) }
}

func TestDeliveryResumeLocalRequiresEveryNodeVerified(t *testing.T) {
	verified := map[string]*model.TaskState{"a": {Status: "verified"}}
	for name, tasks := range map[string]map[string]*model.TaskState{
		"pending":   {"a": {Status: "verified"}, "b": {Status: "pending"}},
		"all new":   {"a": {Status: "pending"}},
		"produced":  {"a": {Status: "verified"}, "b": {Status: "produced"}},
		"running":   {"a": {Status: "running"}},
		"nil entry": {"a": {Status: "verified"}, "b": nil},
	} {
		for _, status := range []string{"running", "graph_failed"} {
			root := t.TempDir()
			dstatWriteJournal(t, root, graph.DeliveryRecord{RunID: "r", Status: status, Check: []string{"c"}})
			got := graph.ProjectDelivery(root, &model.State{RunID: "r", Control: "run", Tasks: tasks})
			if got.Recovery != "inspect" || got.Acceptance != "pending" {
				t.Fatalf("%s/%s: unverified work labeled local-only: %+v", name, status, got)
			}
		}
	}
	for _, status := range []string{"running", "graph_failed"} {
		root := t.TempDir()
		dstatWriteJournal(t, root, graph.DeliveryRecord{RunID: "r", Status: status, Check: []string{"c"}})
		got := graph.ProjectDelivery(root, &model.State{RunID: "r", Control: "run", Tasks: verified})
		if got.Recovery != "resume_local" || got.Acceptance != "pending" {
			t.Fatalf("%s: verified graph should be local-only: %+v", status, got)
		}
	}
}

func TestRetiredRunWithLiveWorkerLeaseCountsAtCapacityBoundary(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "registry.json")
	old, id := runfixture.V2Run(t, dir, "old")
	if err := PublishSession(reg, old, id); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireSession(reg, id, "superseded", ""); err != nil {
		t.Fatal(err)
	}
	// The run is resumed outside the registry: a live worker lease, no controller lock.
	s, err := checkpoint.ReadState(old)
	if err != nil {
		t.Fatal(err)
	}
	release := integrationHold(t, filepath.Join(old, "cells", s.AllTasks()[0].ID, "lease"))
	defer release()
	for i := 0; i < registryMaxActive-1; i++ {
		r, rid := runfixture.V2Run(t, dir, fmt.Sprintf("other%d", i))
		if err := PublishSession(reg, r, rid); err != nil {
			t.Fatalf("other%d rejected below the cap: %v", i, err)
		}
	}
	r, rid := runfixture.V2Run(t, dir, "overflow")
	if err := PublishSession(reg, r, rid); !errors.Is(err, errRegistryActiveFull) {
		t.Fatalf("live worker of a retired run bypassed the active cap: %v", err)
	}
	rr, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	if i := IndexOfRun(rr.Entries, id); i < 0 || rr.Entries[i].Retirement == nil || len(rr.Entries[i].History) == 0 {
		t.Fatal("durable retirement history was dropped for the live run")
	}
	if rr.Overflow.ActiveRejected != 1 || rr.Overflow.LastRejectedRunID != rid {
		t.Fatalf("overflow not recorded: %+v", rr.Overflow)
	}
	// Once the worker stops the run is genuinely retired and capacity is free again.
	release()
	if err := PublishSession(reg, r, rid); err != nil {
		t.Fatalf("stopped retired run still consumed capacity: %v", err)
	}
}

func TestPublishNeverEvictsLiveRetiredRunAtRetiredBound(t *testing.T) {
	dir := t.TempDir()
	regPath := filepath.Join(dir, "registry.json")
	now := time.Now().UTC().Truncate(time.Second)
	mk := func(name string, retiredAt time.Time) registryEntry {
		root, id := runfixture.V2Run(t, dir, name)
		abs, err := filepath.Abs(root)
		if err != nil {
			t.Fatal(err)
		}
		return registryEntry{RunID: id, RunPath: abs, StartedAt: now, LastStartedAt: now, Generation: 1,
			Retirement: &registryRetirement{Reason: "superseded", RetiredAt: retiredAt},
			History:    []registryHistory{{Action: "retired", At: retiredAt, Reason: "superseded"}}}
	}
	// The live run has the OLDEST retirement, so a raw-field bound would evict it first.
	live := mk("live", now.Add(-100*time.Hour))
	release := integrationHold(t, filepath.Join(live.RunPath, "controller.lock"))
	defer release()
	reg := &sessionRegistryV2{Version: registryV2Version, Generation: 1, UpdatedAt: now, Entries: []registryEntry{live}}
	var oldestStopped registryEntry
	for i := 0; i < registryMaxRetired+1; i++ {
		e := mk(fmt.Sprintf("stopped%d", i), now.Add(-time.Duration(i+1)*time.Minute))
		reg.Entries = append(reg.Entries, e)
		oldestStopped = e
	}
	reg.CurrentRunID = reg.Entries[1].RunID
	if err := writeRegistry(regPath, reg); err != nil {
		t.Fatal(err)
	}
	root, id := runfixture.V2Run(t, dir, "new")
	if err := PublishSession(regPath, root, id); err != nil {
		t.Fatal(err)
	}
	rr, err := ReadRegistryAny(regPath)
	if err != nil {
		t.Fatal(err)
	}
	li := IndexOfRun(rr.Entries, live.RunID)
	if li < 0 || rr.Entries[li].Retirement == nil || len(rr.Entries[li].History) != 1 {
		t.Fatal("live run with a retirement record was evicted or lost its history")
	}
	if IndexOfRun(rr.Entries, oldestStopped.RunID) >= 0 {
		t.Fatal("oldest genuinely retired run was not evicted")
	}
	if rr.Overflow.RetiredEvicted != 1 || rr.Overflow.LastRetiredRunID != oldestStopped.RunID {
		t.Fatalf("unexpected eviction accounting: %+v", rr.Overflow)
	}
	// The direct bound has the same rule.
	bound := &sessionRegistryV2{Version: registryV2Version, Generation: 1, CurrentRunID: rr.Entries[0].RunID, Entries: append([]registryEntry(nil), reg.Entries...)}
	bound.CurrentRunID = bound.Entries[1].RunID
	enforceRetiredBound(bound)
	if IndexOfRun(bound.Entries, live.RunID) < 0 || bound.Overflow.RetiredEvicted != 1 {
		t.Fatalf("enforceRetiredBound evicted live work: %+v", bound.Overflow)
	}
}
