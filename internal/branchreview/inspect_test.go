package branchreview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectQueuedReviewReadsBranchDiffAndDocumentWithoutWrites(t *testing.T) {
	ws := changeTestRepo(t)
	got, err := InspectReview(ws, "agent-1", "main", "", "app.go")
	if err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "branch" || !strings.Contains(got["diff"].(string), "+B") {
		t.Fatalf("branch detail: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(ws, ".irrigator")); !os.IsNotExist(err) {
		t.Fatal("branch inspection created review state")
	}
	body := filepath.Join(ws, "proposal.txt")
	if err := os.WriteFile(body, []byte("Review this proposal."), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := requestReview(ws, "plan", "Proposal", "proposal.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := writeReviewOut(snapshot, "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(out, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = InspectReview(ws, "", "", snapshot.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "document" || got["review"].(map[string]any)["body"] != "Review this proposal." {
		t.Fatalf("document detail: %+v", got)
	}
	after, err := os.ReadFile(filepath.Join(out, "snapshot.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("document inspection changed review evidence")
	}
	for _, args := range [][4]string{{"", "", "", ""}, {"agent-1", "", snapshot.ID, ""}, {"", "", "../proposal.txt", ""}, {"", "main", snapshot.ID, ""}, {"", "", strings.Repeat("0", 64), ""}} {
		if _, err := InspectReview(ws, args[0], args[1], args[2], args[3]); err == nil {
			t.Fatalf("invalid review selection accepted: %+v", args)
		}
	}
}

func TestInspectQueuedReviewKeepsPinnedComparisonWhenBaseMoves(t *testing.T) {
	ws := changeTestRepo(t)
	v, _, err := loadChange(ws, "agent-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate main merging the branch between metadata and diff reads.
	reviewTestGit(t, ws, "update-ref", "refs/heads/main", v.Head)
	got, err := inspectBranchReview(ws, v, "app.go")
	if err != nil || !strings.Contains(got["diff"].(string), "+B") || got["review"].(*changeView).MergeBase != v.MergeBase {
		t.Fatalf("pinned comparison lost: result=%+v err=%v", got, err)
	}
	for _, branch := range []string{"main", "agent-1"} {
		if _, err := InspectReview(ws, branch, "main", "", ""); err == nil {
			t.Fatalf("nonqueued branch %s was accepted", branch)
		}
	}
}

func TestInspectQueuedDocumentNeedsNoImplicitDefaultBranch(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "branch", "-m", "main", "trunk")
	if err := os.WriteFile(filepath.Join(ws, "plan.txt"), []byte("Trunk proposal"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := requestReview(ws, "plan", "Trunk plan", "plan.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeReviewOut(snapshot, ""); err != nil {
		t.Fatal(err)
	}
	q, err := ListReviews(ws, "trunk")
	if err != nil || len(q.Documents) != 1 {
		t.Fatalf("document not listed: %+v %v", q, err)
	}
	got, err := InspectReview(ws, "", "", q.Documents[0].Snapshot, "")
	if err != nil || got["review"].(map[string]any)["body"] != "Trunk proposal" {
		t.Fatalf("listed document unreadable without default base: %+v %v", got, err)
	}
}

func TestInspectQueuedReviewBranchTagCollision(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "branch", "topic", "agent-1")
	reviewTestGit(t, ws, "tag", "topic", "agent-1")
	queue, err := ListReviews(ws, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range queue.Changes {
		if row.Branch != "topic" {
			continue
		}
		found = true
		if _, err := InspectReview(ws, row.Branch, "main", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if !found {
		t.Fatalf("canonical topic selector missing: %+v", queue.Changes)
	}
}

func TestInspectQueuedReviewRenameFilterKeepsLiteralBothSides(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "switch", "-qc", "rename", "main")
	reviewTestGit(t, ws, "mv", "app.go", "renamed[1].go")
	reviewTestGit(t, ws, "commit", "-qm", "rename")
	got, err := InspectReview(ws, "rename", "main", "", "renamed[1].go")
	if err != nil {
		t.Fatal(err)
	}
	diff := got["diff"].(string)
	if !strings.Contains(diff, "rename from app.go") || !strings.Contains(diff, "rename to renamed[1].go") || strings.Contains(diff, "new file mode") {
		t.Fatalf("rename metadata lost: %s", diff)
	}
	if _, err := InspectReview(ws, "rename", "main", "", "go.mod"); err == nil {
		t.Fatal("unchanged selector accepted")
	}
}

func TestReviewCurrentBranchUsesHeadRefNotSameNamedTag(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "branch", "topic", "agent-1")
	reviewTestGit(t, ws, "tag", "topic", "main")
	reviewTestGit(t, ws, "switch", "-q", "topic")
	got, _, err := loadChange(ws, "", "main")
	if err != nil {
		t.Fatal(err)
	}
	want, err := changeRev(ws, "refs/heads/topic")
	if err != nil || got.Branch != "topic" || got.Head != want {
		t.Fatalf("wrong current branch: %+v %v", got, err)
	}
}

func TestReviewDefaultMainBaseCannotResolveTag(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "tag", "main", "agent-1")
	q, err := ListReviews(ws, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range q.Changes {
		if row.Branch == "agent-1" {
			found = true
		}
	}
	if !found || q.Base != "main" {
		t.Fatalf("local main hidden by tag: %+v", q)
	}
	for _, base := range []string{"", q.Base} {
		if _, err := InspectReview(ws, "agent-1", base, "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInspectQueuedReviewFiltersAnExactChangeRecord(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "switch", "-qc", "replacement", "main")
	if err := os.WriteFile(filepath.Join(ws, "notes"), []byte("old notes"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", "notes")
	reviewTestGit(t, ws, "commit", "-qm", "old notes")
	reviewTestGit(t, ws, "branch", "comparison")
	if err := os.Remove(filepath.Join(ws, "notes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "notes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes", "entry"), []byte("unrelated child"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", "-A")
	reviewTestGit(t, ws, "commit", "-qm", "replace notes by directory")
	got, err := InspectReview(ws, "replacement", "comparison", "", "notes")
	if err != nil {
		t.Fatal(err)
	}
	diff := got["diff"].(string)
	if !strings.Contains(diff, "deleted file mode") || strings.Contains(diff, "notes/entry") || strings.Contains(diff, "unrelated child") {
		t.Fatalf("single file leaked descendant: %s", diff)
	}
}

func TestInspectQueuedRenameCanRecreateItsSourcePath(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "switch", "-qc", "rename-replace", "main")
	reviewTestGit(t, ws, "mv", "app.go", "renamed.go")
	if err := os.WriteFile(filepath.Join(ws, "app.go"), []byte("completely new source"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", "-A")
	reviewTestGit(t, ws, "commit", "-qm", "rename and replace source")
	got, err := InspectReview(ws, "rename-replace", "main", "", "renamed.go")
	if err != nil {
		t.Fatal(err)
	}
	diff := got["diff"].(string)
	if !strings.Contains(diff, "renamed.go") || strings.Contains(diff, "completely new source") {
		t.Fatalf("wrong rename record: %s", diff)
	}
	got, err = InspectReview(ws, "rename-replace", "main", "", "app.go")
	if err != nil || !strings.Contains(got["diff"].(string), "completely new source") || strings.Contains(got["diff"].(string), "rename to") {
		t.Fatalf("wrong replacement record: %+v %v", got, err)
	}
}

func TestReviewExplicitFullBaseRefCannotBeShadowedByBranchName(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "branch", "refs/heads/main", "agent-1")
	v, _, err := loadChange(ws, "agent-1", "refs/heads/main")
	if err != nil || len(v.Commits) == 0 {
		t.Fatalf("explicit full ref shadowed: %+v %v", v, err)
	}
}

func TestInspectQueuedReviewHandlesTypeChangeAndUnrelatedEdit(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "switch", "-qc", "type-change", "main")
	if err := os.Remove(filepath.Join(ws, "app.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(ws, "app.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module x\n// unrelated edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "add", "-A")
	reviewTestGit(t, ws, "commit", "-qm", "change file type")
	changed, err := InspectReview(ws, "type-change", "main", "", "app.go")
	if err != nil {
		t.Fatal(err)
	}
	diff := changed["diff"].(string)
	if strings.Count(diff, "diff --git ") != 2 || !strings.Contains(diff, "120000") || strings.Contains(diff, "unrelated edit") {
		t.Fatalf("type change lost or mixed: %s", diff)
	}
	other, err := InspectReview(ws, "type-change", "main", "", "go.mod")
	if err != nil || !strings.Contains(other["diff"].(string), "unrelated edit") || strings.Contains(other["diff"].(string), "app.go") {
		t.Fatalf("ordinary edit rejected/mixed: %+v %v", other, err)
	}
}

func TestReviewRemoteDefaultBaseRoundTripsWithoutLocalShadow(t *testing.T) {
	ws := changeTestRepo(t)
	reviewTestGit(t, ws, "update-ref", "refs/remotes/origin/main", "refs/heads/main")
	reviewTestGit(t, ws, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	reviewTestGit(t, ws, "branch", "origin/main", "agent-1")
	queue, err := ListReviews(ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if queue.Base != "refs/remotes/origin/main" {
		t.Fatalf("ambiguous remote selector: %s", queue.Base)
	}
	if _, err := InspectReview(ws, "agent-1", queue.Base, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestReviewFullCommitObjectIDCannotBeShadowedByBranch(t *testing.T) {
	ws := changeTestRepo(t)
	base, err := changeRev(ws, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	reviewTestGit(t, ws, "branch", base, "agent-1")
	view, _, err := loadChange(ws, "agent-1", base)
	if err != nil || view.MergeBase != base || len(view.Commits) == 0 {
		t.Fatalf("explicit commit selected branch: %+v %v", view, err)
	}
}

func TestReviewLocalBaseSelectorsRoundTripSpecialBranchNames(t *testing.T) {
	for _, oidName := range []bool{false, true} {
		t.Run(fmt.Sprint(oidName), func(t *testing.T) {
			ws := changeTestRepo(t)
			a, err := changeRev(ws, "refs/heads/main")
			if err != nil {
				t.Fatal(err)
			}
			b, err := changeRev(ws, "refs/heads/agent-1")
			if err != nil {
				t.Fatal(err)
			}
			reviewTestGit(t, ws, "update-ref", "refs/heads/main", b)
			name := "refs/heads/main"
			if oidName {
				name = b
			}
			reviewTestGit(t, ws, "branch", name, a)
			full := "refs/heads/" + name
			q, err := ListReviews(ws, full)
			if err != nil {
				t.Fatal(err)
			}
			if q.Base != full {
				t.Fatalf("ambiguous local selector returned: %s", q.Base)
			}
			if _, err := InspectReview(ws, "agent-1", q.Base, "", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewListedLongBranchCanBeInspected(t *testing.T) {
	ws := changeTestRepo(t)
	name := strings.Repeat("a", 250)
	reviewTestGit(t, ws, "branch", name, "agent-1")
	q, err := ListReviews(ws, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range q.Changes {
		if row.Branch == name {
			found = true
		}
	}
	if !found {
		t.Fatal("long branch not listed")
	}
	if _, err := InspectReview(ws, name, q.Base, "", ""); err != nil {
		t.Fatal(err)
	}
}
