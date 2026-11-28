package checkpoint

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestReviewJournalPointerFieldAllocationAndClearing(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "allocate_usage", true: "clear_usage"}[initial], func(t *testing.T) {
			root := fixture(t, task("a", 0))
			if initial {
				if err := Update(root, func(s *model.State) error { s.Tasks["a"].Usage = &model.TaskUsage{OutputTokens: 2}; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if err := EnableJournal(root); err != nil {
				t.Fatal(err)
			}
			if err := Update(root, func(s *model.State) error {
				if initial {
					s.Tasks["a"].Usage = nil
				} else {
					s.Tasks["a"].Usage = &model.TaskUsage{OutputTokens: 3}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s, err := ReadState(root)
			if err != nil {
				t.Fatal("acknowledged usage mutation cannot be replayed:", err)
			}
			if initial && s.Tasks["a"].Usage != nil {
				t.Fatal("cleared pointer remained allocated after replay")
			}
			if !initial && (s.Tasks["a"].Usage == nil || s.Tasks["a"].Usage.OutputTokens != 3) {
				t.Fatal("allocated usage missing after replay")
			}
		})
	}
}

func TestReviewJournalWarmCacheIsDetachedAndChecksSourceBytes(t *testing.T) {
	root := journalFixture(t)
	if err := Update(root, func(s *model.State) error {
		s.Tasks["a"].Notes = map[string]string{"source": "kept"}
		s.Control = "pause"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s, err := ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	s.Tasks["a"].Notes["source"] = "poisoned by reader"
	s.Plan.Tasks[0].Prompt = "poisoned plan"
	next, err := ReadState(root)
	if err != nil || next.Tasks["a"].Notes["source"] != "kept" || next.Plan.Tasks[0].Prompt == "poisoned plan" {
		t.Fatalf("reader mutated cached authority: %+v %v", next, err)
	}
	h, _ := journalMarker(t, root)
	path := filepath.Join(root, h.Storage.Journal)
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		t.Fatal("missing committed journal", err)
	}
	raw[0] = '!'
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(root); err == nil {
		t.Fatal("warm cache concealed committed log corruption")
	}
}

func TestReviewJournalWarmCacheDoesNotConcealSnapshotCorruption(t *testing.T) {
	root := journalFixture(t)
	if _, err := ReadState(root); err != nil {
		t.Fatal(err)
	}
	h, _ := journalMarker(t, root)
	path := filepath.Join(root, h.Storage.Snapshot)
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		t.Fatal("missing snapshot", err)
	}
	raw[0] = '!'
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(root); err == nil {
		t.Fatal("warm cache concealed snapshot corruption")
	}
}

// A warm cache reuses a prefix only when it still describes the same snapshot. A
// marker whose snapshot_sequence changed must go through the cold path, which
// rejects it, instead of being accepted because the cached chain matches.
func TestReviewJournalWarmCacheComparesSnapshotSequence(t *testing.T) {
	root := journalFixture(t)
	if err := (FileCheckpointStore{Root: root}).Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Event("note", "", map[string]string{"k": "v"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(root); err != nil {
		t.Fatal(err)
	}
	h, raw := journalMarker(t, root)
	if h.Storage.Sequence != 1 || h.Storage.SnapshotSequence != 0 {
		t.Fatalf("unexpected fixture sequences %d/%d", h.Storage.Sequence, h.Storage.SnapshotSequence)
	}
	edited := regexp.MustCompile(`"snapshot_sequence":\s*0`).ReplaceAll(raw, []byte(`"snapshot_sequence": 1`))
	if bytes.Equal(edited, raw) {
		t.Fatal("marker has no snapshot_sequence to edit", string(raw))
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), edited, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(root); err == nil {
		t.Fatal("warm cache accepted a marker whose snapshot_sequence changed")
	}
}
