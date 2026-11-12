package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func retFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sessionOf projects the registry entry for id through the live
// summarizeRegisteredRun path and returns its JSON shape as a generic map.
func sessionOf(t *testing.T, reg, id string) map[string]any {
	t.Helper()
	r, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	i := IndexOfRun(r.Entries, id)
	if i < 0 {
		t.Fatalf("session %s not listed", id)
	}
	raw, err := json.Marshal(summarizeRegisteredRun(r.Entries[i], true))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err = json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestRetirePausedRunIsExplicitAndRegistryOnly(t *testing.T) {
	_, reg, rA, idA, _, idB := retTwo(t)
	if err := checkpoint.SetControl(rA, "pause"); err != nil {
		t.Fatal(err)
	}
	row := sessionOf(t, reg, idA)
	if row["status"] != "paused" || row["visibility"] != "active" || row["retirement"] != nil {
		t.Fatalf("paused must stay visible and active until explicitly retired: %v", row)
	}
	if _, ok := row["delivery"].(map[string]any); !ok {
		t.Fatalf("delivery projection missing from session summary: %v", row)
	}
	stateBefore := retFile(t, filepath.Join(rA, "state.json"))
	rec, err := RetireSession(reg, idA, "  superseded by rewrite  ", idB)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reason != "superseded by rewrite" || rec.SupersededBy != idB || rec.RetiredAt.IsZero() {
		t.Fatalf("unexpected retirement record %+v", rec)
	}
	if !bytes.Equal(stateBefore, retFile(t, filepath.Join(rA, "state.json"))) {
		t.Fatal("retire mutated the run checkpoint")
	}
	row = sessionOf(t, reg, idA)
	if row["status"] != "paused" || row["visibility"] != "retired" {
		t.Fatalf("execution status must be unchanged and visibility retired: %v", row)
	}
	ret, _ := row["retirement"].(map[string]any)
	if ret["reason"] != "superseded by rewrite" || ret["superseded_by"] != idB || ret["retired_at"] == nil {
		t.Fatalf("retirement not projected: %v", row)
	}
	if sessionOf(t, reg, idB)["visibility"] != "active" {
		t.Fatal("other runs must stay isolated and active")
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	e := got.Entries[IndexOfRun(got.Entries, idA)]
	if len(e.History) != 1 || e.History[0].Action != "retired" || e.History[0].SupersededBy != idB {
		t.Fatalf("durable history missing: %+v", e.History)
	}
}

func TestRetireRefusesHeldLocksAndLeavesRegistryUntouched(t *testing.T) {
	dir, reg, rA, idA, _, _ := retTwo(t)
	before := retFile(t, reg)
	for name, lockPath := range map[string]string{"controller": filepath.Join(rA, "controller.lock"), "worker": filepath.Join(rA, "cells", "a", "lease")} {
		f, err := checkpoint.FileLock(lockPath, true)
		if err != nil {
			t.Fatal(err)
		}
		_, rerr := RetireSession(reg, idA, "stop", "")
		checkpoint.Unlock(f)
		if !errors.Is(rerr, errRetireBusy) {
			t.Fatalf("%s lock held must refuse, got %v", name, rerr)
		}
		if strings.Contains(rerr.Error(), dir) {
			t.Fatalf("error leaks a path: %v", rerr)
		}
		if !bytes.Equal(before, retFile(t, reg)) {
			t.Fatalf("%s refusal changed the registry", name)
		}
	}
	if _, err := RetireSession(reg, idA, "stop", ""); err != nil {
		t.Fatalf("retire after the locks were released: %v", err)
	}
}

func TestRetireRefusesUnreconciledNodes(t *testing.T) {
	_, reg, rA, idA, _, _ := retTwo(t)
	before := retFile(t, reg)
	for _, status := range []string{"running", "produced", "uncertain"} {
		st := status
		if err := checkpoint.Update(rA, func(s *model.State) error { s.Tasks["a"].Status = st; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := RetireSession(reg, idA, "x", ""); !errors.Is(err, errRetireUnreconciled) {
			t.Fatalf("%s must refuse, got %v", st, err)
		}
	}
	if !bytes.Equal(before, retFile(t, reg)) {
		t.Fatal("refusals changed the registry")
	}
}

func TestRetireHoldsEveryLockThroughTheRegistryWrite(t *testing.T) {
	_, reg, rA, idA, _, _ := retTwo(t)
	called := false
	paths := func(run string) []string {
		return []string{reg + ".lock", filepath.Join(run, "controller.lock"), filepath.Join(run, "cells", "a", "lease")}
	}
	retireHoldHook = func(run string) {
		called = true
		for _, p := range paths(run) {
			if f, err := checkpoint.FileLock(p, true); err == nil {
				checkpoint.Unlock(f)
				t.Errorf("%s was not held at write time", filepath.Base(p))
			}
		}
	}
	defer func() { retireHoldHook = nil }()
	if _, err := RetireSession(reg, idA, "why", ""); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("hold hook was not reached")
	}
	for _, p := range paths(rA) {
		f, err := checkpoint.FileLock(p, true)
		if err != nil {
			t.Fatalf("%s still held after retire: %v", filepath.Base(p), err)
		}
		checkpoint.Unlock(f)
	}
}

func TestRetireAndPublishFailClosedOnMalformedRegistry(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	root, id := runfixture.V2Run(t, dir, "x")
	bad := []byte(`{"version":2,"surprise":true}`)
	if err := os.WriteFile(reg, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireSession(reg, id, "r", ""); !errors.Is(err, errRegistryUnreadable) {
		t.Fatalf("retire must fail closed, got %v", err)
	}
	if err := PublishSession(reg, root, id); !errors.Is(err, errRegistryUnreadable) {
		t.Fatalf("publish must not overwrite a malformed registry, got %v", err)
	}
	if !bytes.Equal(bad, retFile(t, reg)) {
		t.Fatal("malformed registry was overwritten")
	}
	if _, err := RetireSession(filepath.Join(dir, "absent.json"), id, "r", ""); !errors.Is(err, errRetireNoRegistry) {
		t.Fatalf("missing registry: %v", err)
	}
}

func TestRetireRejectsBadIdentityReasonAndSuperseder(t *testing.T) {
	_, reg, _, idA, _, idB := retTwo(t)
	before := retFile(t, reg)
	cases := []struct{ id, reason, by string }{
		{idA, "", ""}, {idA, "   ", ""}, {idA, "bad" + string(rune(27)) + "reason", ""}, {idA, strings.Repeat("x", 161), ""},
		{"nope", "r", ""}, {"../x", "r", ""}, {idA, "r", idA}, {idA, "r", "unregistered"}, {idA, "r", "../x"},
	}
	for _, c := range cases {
		if _, err := RetireSession(reg, c.id, c.reason, c.by); err == nil {
			t.Errorf("must refuse %+v", c)
		}
	}
	if !bytes.Equal(before, retFile(t, reg)) {
		t.Fatal("refused requests changed the registry")
	}
	if _, err := RetireSession(reg, idB, "r", ""); err != nil {
		t.Fatal(err) // the current run may be retired explicitly; it stays current and visible
	}
	if _, err := RetireSession(reg, idA, "r", idB); !errors.Is(err, errRetireSuperseder) {
		t.Fatalf("a retired superseder must be refused, got %v", err)
	}
	if _, err := RetireSession(reg, idB, "again", ""); !errors.Is(err, errRetireAlready) {
		t.Fatalf("double retire must be refused, got %v", err)
	}
}

func TestResumeReactivatesRetiredRunWithDurableHistory(t *testing.T) {
	_, reg, _, idA, rB, idB := retTwo(t)
	if _, err := RetireSession(reg, idB, "wrong branch", idA); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil || got.CurrentRunID != idB {
		t.Fatalf("retiring must not move current: %+v %v", got, err)
	}
	gen := got.Generation
	if err = PublishSession(reg, rB, idB); err != nil { // what resume does
		t.Fatal(err)
	}
	got, err = ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	e := got.Entries[IndexOfRun(got.Entries, idB)]
	if e.Retirement != nil || len(e.History) != 2 || e.History[0].Action != "retired" || e.History[0].Reason != "wrong branch" || e.History[1].Action != "reactivated" || got.Generation != gen+1 {
		t.Fatalf("reactivation must keep history and count as a start: gen %d->%d %+v", gen, got.Generation, e)
	}
}

func TestRetireHistoryIsBoundedAndCounted(t *testing.T) {
	_, reg, rA, idA, _, _ := retTwo(t)
	for i := 0; i < 5; i++ {
		if _, err := RetireSession(reg, idA, "cycle "+strconv.Itoa(i), ""); err != nil {
			t.Fatal(err)
		}
		if err := PublishSession(reg, rA, idA); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	e := got.Entries[IndexOfRun(got.Entries, idA)]
	if len(e.History) != registryMaxHistory || e.HistoryDropped != 4 {
		t.Fatalf("history len=%d dropped=%d", len(e.History), e.HistoryDropped)
	}
}

func TestRetiredRunsDoNotConsumeActiveCapacity(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	var ids []string
	for i := 0; i < registryMaxActive; i++ {
		root, id := runfixture.V2Run(t, dir, "act"+strconv.Itoa(i))
		if err := PublishSession(reg, root, id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	extraRoot, extraID := runfixture.V2Run(t, dir, "extra")
	if err := PublishSession(reg, extraRoot, extraID); !errors.Is(err, errRegistryActiveFull) {
		t.Fatalf("want active-full, got %v", err)
	}
	if _, err := RetireSession(reg, ids[0], "finished experiment", ""); err != nil {
		t.Fatal(err)
	}
	if err := PublishSession(reg, extraRoot, extraID); err != nil {
		t.Fatalf("retired run must free its active slot: %v", err)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil || len(got.Entries) != registryMaxActive+1 || got.CurrentRunID != extraID {
		t.Fatalf("unexpected registry %+v %v", got, err)
	}
	if e := got.Entries[IndexOfRun(got.Entries, ids[0])]; e.Retirement == nil {
		t.Fatal("retired run must stay discoverable in the registry")
	}
}

func TestRetiredRecordsAreBoundedWithExplicitOverflow(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	var ids []string
	for i := 0; i < registryMaxRetired+2; i++ {
		root, id := runfixture.V2Run(t, dir, "r"+strconv.Itoa(i))
		if err := PublishSession(reg, root, id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for i := 0; i < registryMaxRetired+1; i++ {
		if _, err := RetireSession(reg, ids[i], "batch "+strconv.Itoa(i), ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	retired := 0
	for _, e := range got.Entries {
		if e.Retirement != nil {
			retired++
		}
	}
	if retired != registryMaxRetired || got.Overflow.RetiredEvicted != 1 || got.Overflow.LastRetiredRunID != ids[0] || IndexOfRun(got.Entries, ids[0]) >= 0 {
		t.Fatalf("retired bound/overflow wrong: retired=%d overflow=%+v", retired, got.Overflow)
	}
}

func TestPausedRunsStayVisibleAndRetained(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "r.json")
	var ids []string
	for i := 0; i < 20; i++ {
		root, id := runfixture.V2Run(t, dir, "p"+strconv.Itoa(i))
		if err := checkpoint.SetControl(root, "pause"); err != nil {
			t.Fatal(err)
		}
		if err := PublishSession(reg, root, id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		row := sessionOf(t, reg, id)
		if row["status"] != "paused" || row["visibility"] != "active" || row["retirement"] != nil {
			t.Fatalf("paused run silently hidden or relabelled: %v", row)
		}
	}
}

func TestConcurrentRetireHasExactlyOneWinner(t *testing.T) {
	_, reg, _, idA, _, _ := retTwo(t)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = RetireSession(reg, idA, "race "+strconv.Itoa(i), "")
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, errRetireAlready) && !errors.Is(err, errRetireBusy) {
			t.Fatalf("unexpected error %v", err)
		}
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	if e := got.Entries[IndexOfRun(got.Entries, idA)]; wins != 1 || len(e.History) != 1 {
		t.Fatalf("wins=%d history=%+v", wins, e.History)
	}
}

func TestRetiredRunWithLiveWorkIsNotHidden(t *testing.T) {
	_, reg, rA, idA, _, _ := retTwo(t)
	if _, err := RetireSession(reg, idA, "parked", ""); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil {
		t.Fatal(err)
	}
	e := got.Entries[IndexOfRun(got.Entries, idA)]
	if s := summarizeRegisteredRun(e, false); s.Visibility != "retired" || s.Retirement == nil {
		t.Fatalf("stopped retired run should read retired: %+v", s)
	}
	lease, err := checkpoint.FileLock(filepath.Join(rA, "cells", "a", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lease)
	s := summarizeRegisteredRun(e, false)
	if s.Visibility != "active" || s.Status != "running" || s.VisibilityNote == "" || s.Retirement != nil || len(s.History) != 1 {
		t.Fatalf("live work after retirement must not be hidden or mislabelled: %+v", s)
	}
}

func TestRetireMigratesV1AndKeepsRunIdentity(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "current-session.json")
	rA, idA := runfixture.V2Run(t, dir, "ui")
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	v1, _ := json.Marshal(sessionRegistry{Version: 1, RunID: idA, RunPath: rA, Generation: 3, StartedAt: started})
	if err := os.WriteFile(reg, v1, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireSession(reg, idA, "done with it", ""); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRegistryAny(reg)
	if err != nil || got.Version != 2 || got.Migrated || got.Generation != 3 || got.CurrentRunID != idA || !got.Entries[0].StartedAt.Equal(started) || got.Entries[0].Retirement == nil {
		t.Fatalf("v1 retire migration wrong: %+v %v", got, err)
	}
}
