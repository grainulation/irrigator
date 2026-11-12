package branchreview

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/session"
)

func reviewTestRepo(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping git integration test in short mode")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(session.ReviewEnv(), "GIT_AUTHOR_NAME=Review Test", "GIT_AUTHOR_EMAIL=review@example.test", "GIT_COMMITTER_NAME=Review Test", "GIT_COMMITTER_EMAIL=review@example.test")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	if err = os.WriteFile(filepath.Join(dir, "main.txt"), []byte("one\ntwo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	return dir
}
func reviewTestOut(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(p, "snapshot")
}
func reviewFind(t *testing.T, s *reviewSnapshot, path string) reviewFile {
	t.Helper()
	for _, f := range s.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("missing review file %s", path)
	return reviewFile{}
}
func TestLocalReviewCapturesStableDirtyBaseline(t *testing.T) {
	repo := reviewTestRepo(t)
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("existing edit\n"), 0600)
	base, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	baseDir := reviewTestOut(t)
	if err = writeReview(baseDir, base); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("existing edit\nagent edit\n"), 0600)
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new file\n"), 0600)
	os.WriteFile(filepath.Join(repo, "secret.txt"), []byte("do not capture\n"), 0600)
	s, err := captureReview(repo, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	f := reviewFind(t, s, "main.txt")
	if f.Before != "existing edit\n" || f.After != "existing edit\nagent edit\n" || s.BaselineID != base.ID {
		t.Fatalf("dirty baseline lost: %+v", f)
	}
	if reviewFind(t, s, "new.txt").Status != "added" {
		t.Fatal("new file not marked added")
	}
	for _, b := range s.Inventory {
		if b.Path == "secret.txt" {
			t.Fatal("ignored file captured")
		}
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "main.txt")); string(data) != f.After {
		t.Fatal("capture changed workspace")
	}
	out := reviewTestOut(t)
	if err = writeReview(out, s); err != nil {
		t.Fatal(err)
	}
	if err = writeReview(out, s); err == nil {
		t.Fatal("snapshot overwritten")
	}
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("later edit\n"), 0600)
	saved, err := readReview(out)
	if err != nil {
		t.Fatal(err)
	}
	if reviewFind(t, saved, "main.txt").After != f.After {
		t.Fatal("saved review followed workspace changes")
	}
}
func TestLocalReviewCommentsAndTamper(t *testing.T) {
	repo := reviewTestRepo(t)
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("changed\n"), 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	out := reviewTestOut(t)
	if err = writeReview(out, s); err != nil {
		t.Fatal(err)
	}
	if _, err = addReviewComment(out, "../escape", 1, "bad"); err == nil {
		t.Fatal("escaped comment accepted")
	}
	if _, err = addReviewComment(out, "main.txt", 3, "bad"); err == nil {
		t.Fatal("out of range line accepted")
	}
	c, err := addReviewComment(out, "main.txt", 1, "Please preserve validation.")
	if err != nil {
		t.Fatal(err)
	}
	if c.SnapshotID != s.ID {
		t.Fatal("comment unbound")
	}
	comments, err := reviewComments(out)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := reviewFeedback(s, comments)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(feedback, "> Please preserve validation.") || !strings.Contains(feedback, s.ID) {
		t.Fatal(feedback)
	}
	data, _ := os.ReadFile(filepath.Join(out, "snapshot.json"))
	var altered reviewSnapshot
	json.Unmarshal(data, &altered)
	altered.Files[0].After = "tamper"
	data, _ = json.Marshal(altered)
	os.WriteFile(filepath.Join(out, "snapshot.json"), data, 0600)
	if _, err = readReview(out); err == nil {
		t.Fatal("tampered review accepted")
	}
}
func TestLocalReviewConcurrentCommentJournal(t *testing.T) {
	repo := reviewTestRepo(t)
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("changed\n"), 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	out := reviewTestOut(t)
	if err = writeReview(out, s); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := addReviewComment(out, "main.txt", 1, "A concurrent comment.")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	comments, err := reviewComments(out)
	if err != nil || len(comments) != 8 {
		t.Fatalf("lost comments %d: %v", len(comments), err)
	}
}
func TestLocalReviewRecordsLinksWithoutFollowingAndLargeFilesByHash(t *testing.T) {
	repo := reviewTestRepo(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(repo, "linked")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "large"), make([]byte, reviewFileLimit+1), 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	link := reviewFind(t, s, "linked")
	if !link.Link || link.After != "/etc/passwd" || link.Status != "added" {
		t.Fatalf("symlink must be recorded as its target text, not followed: %+v", link)
	}
	large := reviewFind(t, s, "large")
	if !large.Large || !large.Binary || large.After != "" || large.AfterSHA256 == "" {
		t.Fatalf("large file must be compared by hash without contents: %+v", large)
	}
	for _, b := range s.Inventory {
		if b.Path == "large" && len(b.Data) != 0 {
			t.Fatal("large file contents retained")
		}
	}
}
func TestLocalReviewBinaryAndDeletion(t *testing.T) {
	repo := reviewTestRepo(t)
	os.Remove(filepath.Join(repo, "main.txt"))
	os.WriteFile(filepath.Join(repo, "image.bin"), []byte{0, 1, 2}, 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if f := reviewFind(t, s, "main.txt"); f.Status != "deleted" || f.Before != "one\ntwo\n" {
		t.Fatalf("deletion %+v", f)
	}
	if f := reviewFind(t, s, "image.bin"); !f.Binary || f.After != "" {
		t.Fatalf("binary %+v", f)
	}
}
func TestLocalReviewUnbornRepo(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("first\n"), 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.BaseCommit != "unborn" || reviewFind(t, s, "new.txt").Status != "added" {
		t.Fatal("unborn repository not captured")
	}
}

func TestLocalReviewSeesAssumeUnchangedEdits(t *testing.T) {
	repo := reviewTestRepo(t)
	cmd := exec.Command("git", "update-index", "--assume-unchanged", "main.txt")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("hidden change\n"), 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if f := reviewFind(t, s, "main.txt"); f.Before != "one\ntwo\n" || f.After != "hidden change\n" {
		t.Fatalf("Git index hint hid actual changes: %+v", f)
	}
}
func TestLocalReviewRejectsLinkedOutputAncestor(t *testing.T) {
	repo := reviewTestRepo(t)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(other, filepath.Join(parent, "linked")); err != nil {
		t.Fatal(err)
	}
	if err = writeReview(filepath.Join(parent, "linked", "snapshot"), s); err == nil {
		t.Fatal("linked output ancestor accepted")
	}
	if _, err = os.Stat(filepath.Join(other, "snapshot")); !os.IsNotExist(err) {
		t.Fatal("created output through linked ancestor")
	}
}
