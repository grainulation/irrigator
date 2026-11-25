package branchreview

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneRejectsSymlinkedStorageAncestors(t *testing.T) {
	for _, linked := range []string{".irrigator", ".irrigator/reviews", ".irrigator/changes", ".irrigator/sessions"} {
		t.Run(linked, func(t *testing.T) {
			ws := changeTestRepo(t)
			outside := t.TempDir()
			old := filepath.Join(outside, "old")
			if err := os.Mkdir(old, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(old, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-60 * 24 * time.Hour)
			if err := os.Chtimes(old, past, past); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(ws, linked)
			if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			for _, apply := range []bool{false, true} {
				if _, err := PruneReviews(ws, 30, apply); err == nil {
					t.Fatal("symlinked storage accepted")
				}
				if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
					t.Fatalf("external sentinel: %q %v", data, err)
				}
			}
		})
	}
}

func TestPrunePinnedParentCannotRedirectDeletion(t *testing.T) {
	ws := changeTestRepo(t)
	storage := filepath.Join(ws, ".irrigator", "reviews")
	if err := os.MkdirAll(filepath.Join(storage, "old"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(storage)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Lstat("old")
	if err != nil {
		t.Fatal(err)
	}
	candidate := pruneCandidate{root: root, name: "old", info: info}
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "old"), 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "old", "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(storage, storage+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, storage); err != nil {
		t.Fatal(err)
	}
	if err := candidate.remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("replacement redirected deletion:", err)
	}
	if _, err := os.Stat(filepath.Join(storage+"-moved", "old")); !os.IsNotExist(err) {
		t.Fatal("pinned owned candidate was not removed")
	}
}

func TestPrunePreservesStateWhenGitLookupFails(t *testing.T) {
	ws := changeTestRepo(t)
	if _, _, err := setChangeVerdict(ws, "agent-2", "", "approved", ""); err != nil {
		t.Fatal(err)
	}
	changes := filepath.Join(ws, ".irrigator", "changes")
	before, err := os.ReadDir(changes)
	if err != nil {
		t.Fatal(err)
	}
	// Quiet exact-ref lookup calls this absence (exit 1); enumerating refs
	// diagnoses the corruption. Preserve metadata in both preview and apply.
	if err := os.WriteFile(filepath.Join(ws, ".git", "refs", "heads", "agent-2"), []byte("invalid ref\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []bool{false, true} {
		if _, err := PruneReviews(ws, 30, apply); err == nil {
			t.Fatal("inconclusive Git lookup accepted as absence")
		}
		for _, entry := range before {
			if _, err := os.Stat(filepath.Join(changes, entry.Name())); err != nil {
				t.Fatal("review state lost:", err)
			}
		}
	}
}

func TestPrunePreservesStateWhenSnapshotCannotBeRead(t *testing.T) {
	ws := changeTestRepo(t)
	changes := filepath.Join(ws, ".irrigator", "changes")
	reviews := filepath.Join(ws, ".irrigator", "reviews", "damaged")
	if err := os.MkdirAll(changes, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reviews, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(changes, "document.json")
	if err := os.WriteFile(state, []byte(`{"subject":"snapshot:document"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"", "{", `{"version":1,"id":"document"}`} {
		if err := os.WriteFile(filepath.Join(reviews, "snapshot.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		for _, apply := range []bool{false, true} {
			if _, err := PruneReviews(ws, 30, apply); err == nil {
				t.Fatal("invalid snapshot accepted as absence")
			}
			if _, err := os.Stat(state); err != nil {
				t.Fatal("review state lost:", err)
			}
		}
	}
}

func TestPruneRecomputesSelectionAfterPreview(t *testing.T) {
	ws := changeTestRepo(t)
	if _, _, err := setChangeVerdict(ws, "agent-2", "", "approved", ""); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "branch", "-qD", "agent-2")
	preview, err := PruneReviews(ws, 30, false)
	if err != nil || len(preview.Items) != 1 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	reviewTestGit(t, ws, "branch", "agent-2", "main")
	result, err := PruneReviews(ws, 30, true)
	if err != nil || len(result.Items) != 0 {
		t.Fatalf("apply: %+v %v", result, err)
	}
	if _, err := os.Stat(preview.Items[0].Path); err != nil {
		t.Fatal("recreated branch's state was removed:", err)
	}
}

func TestPruneRemovesAgedOwnedStorageWithoutFollowingNestedLinks(t *testing.T) {
	ws := changeTestRepo(t)
	private := filepath.Join(ws, ".irrigator")
	old := filepath.Join(private, "reviews", "old")
	if err := os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(private, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(old, "link")); err != nil {
		t.Fatal(err)
	}
	staleLog := filepath.Join(private, "sessions", "old.jsonl")
	recentLog := filepath.Join(private, "sessions", "recent.jsonl")
	for _, name := range []string{staleLog, recentLog} {
		if err := os.WriteFile(name, []byte("entry\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-60 * 24 * time.Hour)
	for _, name := range []string{old, staleLog} {
		if err := os.Chtimes(name, past, past); err != nil {
			t.Fatal(err)
		}
	}
	preview, err := PruneReviews(ws, 30, false)
	if err != nil || len(preview.Items) != 2 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if _, err := os.Stat(staleLog); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneReviews(ws, 30, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{old, staleLog} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("eligible storage retained: %s", name)
		}
	}
	for _, name := range []string{sentinel, recentLog} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("unselected data lost: %s: %v", name, err)
		}
	}
}
