package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func journalFixture(t *testing.T) string {
	t.Helper()
	root := fixture(t, task("a", 0), task("b", 0))
	if err := EnableJournal(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func journalMarker(t *testing.T, root string) (journalHead, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := parseJournalHead(raw)
	if err != nil {
		t.Fatal(err)
	}
	return h, raw
}

func TestJournalMigratesWithBackupAndRoundTrips(t *testing.T) {
	root := fixture(t, task("a", 0), task("b", 0))
	before, err := ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = EnableJournal(root); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(filepath.Join(root, "state.json.bak"))
	if err != nil || !bytes.Equal(original, backup) {
		t.Fatalf("original backup: %v", err)
	}
	after, err := ReadState(root)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("migration changed state: %v", err)
	}
	_, marker := journalMarker(t, root)
	if len(marker) > 2000 {
		t.Fatal("commit marker includes full state")
	}
	if err = Update(root, func(s *model.State) error {
		s.Control = "pause"
		s.Tasks["a"].Error = "bounded error"
		s.Tasks["b"].Notes = map[string]string{"review": "retained"}
		s.Event("paused", "", nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err = ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if after.Control != "pause" || after.Tasks["a"].Error != "bounded error" || after.Tasks["b"].Notes["review"] != "retained" || len(after.Events) != len(before.Events)+1 {
		t.Fatal("committed deltas missing")
	}
}

func TestCheckpointExplicitNoopDoesNotPublishOrMigrate(t *testing.T) {
	for _, journal := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "journal"}[journal], func(t *testing.T) {
			root := fixture(t, task("a", 0))
			if journal {
				if err := EnableJournal(root); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(root, "state.json")
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := os.ReadFile(path)
			calls := 0
			if err := (FileCheckpointStore{root}).Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error { calls++; s.Control = "bad-private-change"; return ErrUnchanged }); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(path)
			next, _ := os.ReadFile(path)
			if calls != 1 || !os.SameFile(before, after) || !bytes.Equal(raw, next) {
				t.Fatal("no-op published or replayed reducer")
			}
		})
	}
	root := journalFixture(t)
	_, before := journalMarker(t, root)
	if err := Update(root, func(*model.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	_, after := journalMarker(t, root)
	if !bytes.Equal(before, after) {
		t.Fatal("empty journal delta published")
	}
}

func TestJournalUncommittedTailAndCommittedCorruption(t *testing.T) {
	root := journalFixture(t)
	if err := Update(root, func(s *model.State) error { s.Control = "pause"; return nil }); err != nil {
		t.Fatal(err)
	}
	h, _ := journalMarker(t, root)
	path := filepath.Join(root, h.Storage.Journal)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("{incomplete-crash-tail")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := ReadState(root)
	if err != nil || s.Control != "pause" {
		t.Fatalf("uncommitted tail not ignored: %v", err)
	}
	if err = Update(root, func(s *model.State) error { s.Control = "ready"; return nil }); err != nil {
		t.Fatal(err)
	}
	s, err = ReadState(root)
	if err != nil || s.Control != "ready" {
		t.Fatalf("writer did not recover tail: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = '!'
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadState(root); err == nil {
		t.Fatal("interior corruption was skipped")
	}
}

func TestJournalCompactionRetainsHistoryAndBoundedRecoverySet(t *testing.T) {
	root := journalFixture(t)
	for i := 0; i < journalTransactionLimit+5; i++ {
		if err := Update(root, func(s *model.State) error { s.Event("tick", "a", i); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	s, err := ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Events) != journalTransactionLimit+6 {
		t.Fatalf("events=%d", len(s.Events))
	}
	entries, err := os.ReadDir(filepath.Join(root, journalDirectory))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("derived generations not retired: %d", len(entries))
	}
	h, _ := journalMarker(t, root)
	if h.Storage.SnapshotSequence == 0 {
		t.Fatal("snapshot never advanced")
	}
	files, err := CheckpointStorageFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	archive := map[string][]byte{}
	for file := range files {
		archive[file], err = os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, raw := journalMarker(t, root)
	logical, err := ExpandCheckpointFiles(raw, archive)
	if err != nil {
		t.Fatal(err)
	}
	var restored model.State
	if err = json.Unmarshal(logical, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, &restored) {
		t.Fatal("backup expansion lost state")
	}
}

func TestJournalConcurrentReadersAndCompaction(t *testing.T) {
	root := journalFixture(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				if _, err := ReadState(root); err != nil {
					t.Errorf("concurrent read: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < journalTransactionLimit+2; i++ {
		if err := Update(root, func(s *model.State) error { s.Event("tick", "a", i); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestJournalStillRejectsChangedImmutablePlan(t *testing.T) {
	root := journalFixture(t)
	if err := Update(root, func(s *model.State) error { s.Plan.Name = "changed without new identity"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(root); err == nil {
		t.Fatal("journal bypassed admitted-plan validation")
	}
}
