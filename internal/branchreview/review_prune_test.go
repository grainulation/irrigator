package branchreview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	subject := "snapshot:" + strings.Repeat("d", 64)
	state := filepath.Join(changes, changeStateID(subject)+".json")
	data, err := json.Marshal(changeState{Version: changeStateVersion, Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, data, 0600); err != nil {
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

func TestPruneRejectsOverflowAndPreservesFreshEvidence(t *testing.T) {
	ws := changeTestRepo(t)
	dir := filepath.Join(ws, ".irrigator", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "fresh.jsonl")
	if err := os.WriteFile(fresh, []byte("fresh evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, days := range []int{maxPruneDays, maxPruneDays + 1, 365000} {
		for _, apply := range []bool{false, true} {
			report, err := PruneReviews(ws, days, apply)
			if days == maxPruneDays {
				if err != nil || len(report.Items) != 0 {
					t.Fatalf("supported retention: %+v %v", report, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe retention accepted: %d", days)
			}
			if data, err := os.ReadFile(fresh); err != nil || string(data) != "fresh evidence\n" {
				t.Fatalf("fresh evidence changed: %q %v", data, err)
			}
		}
	}
}

func TestPrunePreservesUnrecognizedStateBesideRealOrphan(t *testing.T) {
	ws := changeTestRepo(t)
	if _, _, err := setChangeVerdict(ws, "agent-2", "main", "approved", "orphan"); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "branch", "-qD", "agent-2")
	dir := filepath.Join(ws, ".irrigator", "changes")
	preserved := map[string][]byte{}
	for _, state := range []changeState{
		{Version: 2, Subject: "future-branch"},
		{Version: 0, Subject: "legacy-unrecognized"},
		{Version: 1, Subject: "snapshot:invalid"},
		{Version: 1, Subject: "invalid branch name"},
	} {
		name := filepath.Join(dir, changeStateID(state.Subject)+".json")
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
		preserved[name] = data
	}
	for _, apply := range []bool{false, true} {
		report, err := PruneReviews(ws, 30, apply)
		if err != nil || len(report.Items) != 1 || !strings.Contains(report.Items[0].Reason, "agent-2") {
			t.Fatalf("orphan selection: %+v %v", report, err)
		}
		for name, want := range preserved {
			if got, err := os.ReadFile(name); err != nil || string(got) != string(want) {
				t.Fatalf("unrecognized state changed: %s %v", name, err)
			}
		}
	}
}

func TestPrunePartialFailureReportsCompletedAndFailedPaths(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	report := &pruneReport{Items: []pruneItem{}}
	var candidates []pruneCandidate
	for _, name := range []string{"first.jsonl", "second.jsonl", "third.jsonl"} {
		if err := root.WriteFile(name, []byte("evidence"), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := root.Lstat(name)
		if err != nil {
			t.Fatal(err)
		}
		report.Items = append(report.Items, pruneItem{Path: filepath.Join(dir, name)})
		candidates = append(candidates, pruneCandidate{root: root, name: name, info: info})
	}
	// Change the second selected item before apply; the first still succeeds.
	if err := root.Rename("second.jsonl", "retained.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("second.jsonl", []byte("new evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	err = applyPruneCandidates(report, candidates)
	if err == nil || report.Applied || len(report.Removed) != 1 || report.Removed[0].Path != report.Items[0].Path || report.FailedPath != report.Items[1].Path || report.Error == "" {
		t.Fatalf("partial report: %+v %v", report, err)
	}
	if _, err := root.Stat("first.jsonl"); !os.IsNotExist(err) {
		t.Fatal("first item not removed")
	}
	for _, name := range []string{"second.jsonl", "third.jsonl", "retained.jsonl"} {
		if _, err := root.Stat(name); err != nil {
			t.Fatal("uncompleted item lost:", name, err)
		}
	}
	data, err := json.Marshal(report)
	if err != nil || !strings.Contains(string(data), "removed") || !strings.Contains(string(data), "failed_path") {
		t.Fatalf("partial JSON: %s %v", data, err)
	}
}

func TestReviewQueueSurvivesStoragePreviewError(t *testing.T) {
	ws := changeTestRepo(t)
	if err := os.MkdirAll(filepath.Join(ws, ".irrigator"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(ws, ".irrigator", "sessions")); err != nil {
		t.Fatal(err)
	}
	if code, err := changeCLI("list", []string{"--workspace", ws, "--json"}); code != 0 || err != nil {
		t.Fatalf("queue unavailable: %d %v", code, err)
	}
	report, err := PruneReviews(ws, 30, true)
	if err == nil || report.Error == "" || report.Applied {
		t.Fatalf("unsafe cleanup accepted: %+v %v", report, err)
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
