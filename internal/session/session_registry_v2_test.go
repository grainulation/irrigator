package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func v2Complete(t *testing.T, root string) {
	t.Helper()
	if err := checkpoint.Update(root, func(s *model.State) error { s.Control = "complete"; s.Tasks["a"].Status = "verified"; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryV2RetainsBothStarts(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, ".irrigator", "current-session.json")
	rA, idA := runfixture.V2Run(t, dir, "ui")
	rB, idB := runfixture.V2Run(t, dir, "eq")
	if err := PublishSession(reg, rA, idA); err != nil {
		t.Fatal(err)
	}
	if err := PublishSession(reg, rB, idB); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil || got.Version != 2 || len(got.Entries) != 2 || got.CurrentRunID != idB {
		t.Fatalf("unexpected registry %+v err=%v", got, err)
	}
	if IndexOfRun(got.Entries, idA) < 0 {
		t.Fatal("first start was dropped by the later start")
	}
	if cur, err := readSessionRegistry(reg); err != nil || cur.RunID != idB {
		t.Fatalf("current compat read: %+v %v", cur, err)
	}
}

func TestRegistryV1MigratesPreservingRun(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "current-session.json")
	rA, idA := runfixture.V2Run(t, dir, "ui")
	rB, idB := runfixture.V2Run(t, dir, "eq")
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	v1, _ := json.Marshal(sessionRegistry{Version: 1, RunID: idA, RunPath: rA, Generation: 3, StartedAt: started})
	if err := os.WriteFile(reg, v1, 0600); err != nil {
		t.Fatal(err)
	}
	if r, err := ReadRegistryAny(reg); err != nil || !r.Migrated || len(r.Entries) != 1 {
		t.Fatalf("v1 must be readable: %+v %v", r, err)
	}
	if err := PublishSession(reg, rB, idB); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil || got.Version != 2 || got.Migrated || len(got.Entries) != 2 || got.Generation != 4 {
		t.Fatalf("migration: %+v %v", got, err)
	}
	i := IndexOfRun(got.Entries, idA)
	if i < 0 || !got.Entries[i].StartedAt.Equal(started) {
		t.Fatal("v1 run or its start time not preserved")
	}
	// Same-run start against v1 also migrates.
	v1b, _ := json.Marshal(sessionRegistry{Version: 1, RunID: idA, RunPath: rA, Generation: 1, StartedAt: started})
	_ = os.WriteFile(reg, v1b, 0600)
	if err := PublishSession(reg, rA, idA); err != nil {
		t.Fatal(err)
	}
	if r, _ := ReadRegistryAny(reg); r == nil || r.Version != 2 || r.CurrentRunID != idA {
		t.Fatal("same-run start must still rewrite v2")
	}
}

func TestRegistryRecentBoundAndExplicitOverflow(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	var first string
	for i := 0; i < 18; i++ {
		root, id := runfixture.V2Run(t, dir, "run"+string(rune('a'+i)))
		if i == 0 {
			first = id
		}
		v2Complete(t, root)
		time.Sleep(2 * time.Millisecond)
		if err := PublishSession(reg, root, id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != registryMaxRecent+1 || got.Overflow.RecentEvicted != 1 || IndexOfRun(got.Entries, first) >= 0 {
		t.Fatalf("bound/overflow wrong: n=%d overflow=%+v", len(got.Entries), got.Overflow)
	}
}

func TestRegistryActiveNeverEvictedAndFullIsExplicit(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	var last error
	for i := 0; i < registryMaxActive+1; i++ {
		root, id := runfixture.V2Run(t, dir, "act"+strconv.Itoa(i))
		last = PublishSession(reg, root, id)
	}
	if !errors.Is(last, errRegistryActiveFull) {
		t.Fatalf("want active-full error, got %v", last)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil || len(got.Entries) != registryMaxActive || got.Overflow.ActiveRejected != 1 || got.Overflow.RecentEvicted != 0 {
		t.Fatalf("active set must stay intact: n=%d %+v %v", len(got.Entries), got.Overflow, err)
	}
}

func TestDerivedStatusFromStateAndLeases(t *testing.T) {
	dir := t.TempDir()
	root, id := runfixture.V2Run(t, dir, "s")
	entry := registryEntry{RunID: id, RunPath: root, StartedAt: time.Now(), LastStartedAt: time.Now()}
	if got := summarizeRegisteredRun(entry, false).Status; got != "ready" {
		t.Fatalf("ready expected, got %s", got)
	}
	if err := checkpoint.Update(root, func(s *model.State) error { s.Control = "complete"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := summarizeRegisteredRun(entry, false).Status; got != "inconsistent" {
		t.Fatalf("complete without verified tasks must not read complete, got %s", got)
	}
	v2Complete(t, root)
	if got := summarizeRegisteredRun(entry, false).Status; got != "complete" {
		t.Fatalf("complete expected, got %s", got)
	}
	lease, err := checkpoint.FileLock(filepath.Join(root, "cells", "a", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lease)
	sum := summarizeRegisteredRun(entry, false)
	if sum.Status != "running" || sum.WorkersActive == nil || *sum.WorkersActive != 1 {
		t.Fatalf("live lease must read running: %+v", sum)
	}
}

func TestRegistrySummariesAvailableAndUnavailable(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	rA, idA := runfixture.V2Run(t, dir, "ui")
	rB, idB := runfixture.V2Run(t, dir, "eq")
	for _, x := range [][2]string{{rA, idA}, {rB, idB}} {
		if err := PublishSession(reg, x[0], x[1]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadRegistryAny(reg)
	if err != nil || got.CurrentRunID != idB || len(got.Entries) != 2 {
		t.Fatalf("unexpected registry %+v %v", got, err)
	}
	rows := func() map[string]panelSessionSummary {
		t.Helper()
		out := map[string]panelSessionSummary{}
		for _, e := range got.Entries {
			sum := summarizeRegisteredRun(e, true)
			raw, merr := json.Marshal(sum)
			if merr != nil {
				t.Fatal(merr)
			}
			if strings.Contains(string(raw), "SECRET-PROMPT-MARKER") {
				t.Fatal("prompt leaked into session summary")
			}
			out[e.RunID] = sum
		}
		return out
	}
	if m := rows(); !m[idA].Available || !m[idB].Available {
		t.Fatalf("both registered runs must be available: %+v", m)
	}
	_ = os.Remove(filepath.Join(rB, "state.json"))
	m := rows()
	if m[idB].Available || m[idB].Status != "missing" || m[idB].Reason != "state_missing" || m[idB].RunName != "" || len(m[idB].Agents) != 0 {
		t.Fatalf("missing state must be honestly unavailable, no stale fallback: %+v", m[idB])
	}
	if !m[idA].Available {
		t.Fatal("other registered run should remain visible")
	}
}
