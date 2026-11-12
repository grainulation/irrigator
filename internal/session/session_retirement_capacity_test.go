package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Tests for session transitions and history. They reuse the helpers in
// session_retirement_test.go and session_registry_v2_test.go (retTwo, v2Run,
// v2Complete, retFile, sessionOf).

// A run whose state cannot be read is uncertain: it stays active, counts toward
// capacity, and explicit retirement is refused rather than guessed.
func TestSessionRetirementUnreadableStateStaysActiveAndIsNotRetirable(t *testing.T) {
	dir, reg, rA, idA, _, _ := retTwo(t)
	statePath := filepath.Join(rA, "state.json")
	bad := []byte("{not json")
	if err := os.WriteFile(statePath, bad, 0600); err != nil {
		t.Fatal(err)
	}
	before := retFile(t, reg)
	if _, err := RetireSession(reg, idA, "cleanup", ""); !errors.Is(err, errRetireUncertain) {
		t.Fatalf("unreadable state must refuse retirement, got %v", err)
	}
	if !bytes.Equal(before, retFile(t, reg)) || !bytes.Equal(bad, retFile(t, statePath)) {
		t.Fatal("refusal changed the registry or the checkpoint")
	}
	root3, id3 := runfixture.V2Run(t, dir, "third")
	if err := PublishSession(reg, root3, id3); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	i := IndexOfRun(got.Entries, idA)
	if i < 0 || got.Entries[i].Retirement != nil {
		t.Fatalf("uncertain run must stay registered and unretired: %+v", got.Entries)
	}
	sum := summarizeRegisteredRun(got.Entries[i], false)
	if sum.Available || sum.Visibility != "active" || !sum.registryActive() {
		t.Fatalf("uncertain run must count as active: %+v", sum)
	}
}

// A retired run whose worker lease is live still occupies protected active
// capacity, its retirement record is kept, and the slot frees only after the
// work has actually stopped.
func TestSessionRetirementRetiredButLiveRunKeepsProtectedCapacity(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	var roots, ids []string
	for i := 0; i < registryMaxActive; i++ {
		root, id := runfixture.V2Run(t, dir, "cap"+strconv.Itoa(i))
		if err := PublishSession(reg, root, id); err != nil {
			t.Fatal(err)
		}
		roots, ids = append(roots, root), append(ids, id)
	}
	if _, err := RetireSession(reg, ids[0], "parked", ""); err != nil {
		t.Fatal(err)
	}
	lease, err := checkpoint.FileLock(filepath.Join(roots[0], "cells", "a", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	extraRoot, extraID := runfixture.V2Run(t, dir, "extra")
	perr := PublishSession(reg, extraRoot, extraID)
	checkpoint.Unlock(lease)
	if !errors.Is(perr, errRegistryActiveFull) {
		t.Fatalf("live retired run must keep its active slot, got %v", perr)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	idx := IndexOfRun(got.Entries, ids[0])
	if idx < 0 || got.Entries[idx].Retirement == nil {
		t.Fatal("retirement record must be kept while the run is live")
	}
	if len(got.Entries) != registryMaxActive || got.Overflow.ActiveRejected != 1 || got.Overflow.LastRejectedRunID != extraID {
		t.Fatalf("rejection must be explicit and nothing evicted: n=%d %+v", len(got.Entries), got.Overflow)
	}
	if err = PublishSession(reg, extraRoot, extraID); err != nil {
		t.Fatalf("slot must free once the retired run is stopped: %v", err)
	}
}

// Capacity eviction only drops registry rows. It never deletes the checkpoint
// and never creates a retirement record.
func TestSessionRetirementRecentEvictionNeverDeletesCheckpointOrRetires(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	var ids []string
	var firstState []byte
	var firstPath string
	for i := 0; i < registryMaxRecent+2; i++ {
		root, id := runfixture.V2Run(t, dir, "ev"+strconv.Itoa(i))
		v2Complete(t, root)
		if i == 0 {
			firstPath = filepath.Join(root, "state.json")
			firstState = retFile(t, firstPath)
		}
		time.Sleep(2 * time.Millisecond)
		if err := PublishSession(reg, root, id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	if IndexOfRun(got.Entries, ids[0]) >= 0 || got.Overflow.RecentEvicted != 1 || got.Overflow.LastEvictedRunID != ids[0] {
		t.Fatalf("oldest completed run should be evicted from the registry with a record: %+v", got.Overflow)
	}
	if !bytes.Equal(firstState, retFile(t, firstPath)) {
		t.Fatal("registry eviction touched the run checkpoint")
	}
	for _, e := range got.Entries {
		if e.Retirement != nil || len(e.History) != 0 {
			t.Fatalf("no run may be retired implicitly: %+v", e)
		}
	}
	if got.Overflow.RetiredEvicted != 0 {
		t.Fatalf("nothing was retired, so nothing may be retired-evicted: %+v", got.Overflow)
	}
}

// Retire then resume: execution status stays paused, the checkpoint is
// byte-identical, and the panel shows both history records.
func TestSessionRetirementReactivationKeepsPausedStatusCheckpointAndHistory(t *testing.T) {
	_, reg, rA, idA, _, _ := retTwo(t)
	if err := checkpoint.SetControl(rA, "pause"); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(rA, "state.json")
	before := retFile(t, statePath)
	if _, err := RetireSession(reg, idA, "parked while rewriting", ""); err != nil {
		t.Fatal(err)
	}
	if err := PublishSession(reg, rA, idA); err != nil { // what resume does
		t.Fatal(err)
	}
	if !bytes.Equal(before, retFile(t, statePath)) {
		t.Fatal("retire/reactivate changed the run checkpoint")
	}
	row := sessionOf(t, reg, idA)
	if row["status"] != "paused" || row["visibility"] != "active" || row["retirement"] != nil {
		t.Fatalf("reactivated paused run must stay paused and active: %v", row)
	}
	hist, _ := row["history"].([]any)
	if len(hist) != 2 {
		t.Fatalf("history must keep retired and reactivated records: %v", row["history"])
	}
}
