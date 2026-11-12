package branchreview

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/session"
)

func reviewTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(session.ReviewEnv(), "GIT_AUTHOR_NAME=Review Test", "GIT_AUTHOR_EMAIL=review@example.test", "GIT_COMMITTER_NAME=Review Test", "GIT_COMMITTER_EMAIL=review@example.test")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestLocalReviewUnchangedLargeTrackedFileDoesNotBlockCapture(t *testing.T) {
	repo := reviewTestRepo(t)
	os.WriteFile(filepath.Join(repo, "asset.bin"), make([]byte, 3<<20), 0600)
	reviewTestGit(t, repo, "add", "asset.bin")
	reviewTestGit(t, repo, "commit", "-qm", "asset")
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("one\nthree\n"), 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 1 || s.Files[0].Path != "main.txt" {
		t.Fatalf("only the edited file should change: %+v", s.Files)
	}
}

func TestLocalReviewRetainsOnlyChangedContents(t *testing.T) {
	repo := reviewTestRepo(t)
	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("clean\n"), 0600)
	reviewTestGit(t, repo, "add", "other.txt")
	reviewTestGit(t, repo, "commit", "-qm", "other")
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("edited\n"), 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range s.Inventory {
		switch b.Path {
		case "other.txt", ".gitignore":
			if len(b.Data) != 0 || b.Object == "" || b.SHA256 == "" {
				t.Fatalf("unchanged file should keep only its identity: %+v", b)
			}
		case "main.txt":
			if string(b.Data) != "edited\n" || b.Object != "" {
				t.Fatalf("changed file contents must be retained: %+v", b)
			}
		}
	}
}

func TestLocalReviewBaselineReadsUnchangedContentFromGit(t *testing.T) {
	repo := reviewTestRepo(t)
	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("original\n"), 0600)
	reviewTestGit(t, repo, "add", "other.txt")
	reviewTestGit(t, repo, "commit", "-qm", "other")
	base, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	baseDir := reviewTestOut(t)
	if err = writeReview(baseDir, base); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "other.txt"), []byte("original\nagent edit\n"), 0600)
	s, err := captureReview(repo, baseDir)
	if err != nil {
		t.Fatal(err)
	}
	f := reviewFind(t, s, "other.txt")
	if f.Before != "original\n" || f.After != "original\nagent edit\n" || !strings.Contains(f.Diff, "+agent edit") {
		t.Fatalf("baseline content must come from its Git object: %+v", f)
	}
}

func TestLocalReviewSkipsNestedRepositories(t *testing.T) {
	repo := reviewTestRepo(t)
	nested := filepath.Join(repo, "nested")
	os.Mkdir(nested, 0700)
	reviewTestGit(t, nested, "init", "-q")
	os.WriteFile(filepath.Join(nested, "inner.txt"), []byte("inner\n"), 0600)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range s.Files {
		if strings.HasPrefix(f.Path, "nested") {
			t.Fatalf("nested repository captured: %s", f.Path)
		}
	}
}

func TestLocalReviewExecuteBitFollowsGit(t *testing.T) {
	repo := reviewTestRepo(t)
	script := filepath.Join(repo, "run.sh")
	os.WriteFile(script, []byte("#!/bin/sh\n"), 0755)
	reviewTestGit(t, repo, "add", "run.sh")
	reviewTestGit(t, repo, "commit", "-qm", "script")
	// Git treats any owner-executable file as 100755.
	os.Chmod(script, 0700)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 0 {
		t.Fatalf("owner-only execute bit reported as a change: %+v", s.Files)
	}
}

func TestLocalReviewDefaultOutputStaysOutOfLaterCaptures(t *testing.T) {
	repo := reviewTestRepo(t)
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte(strings.Repeat("edited line\n", 120000)), 0600)
	first, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := writeReviewOut(first, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, filepath.Join(repo, ".irrigator", "reviews")+string(filepath.Separator)) {
		t.Fatalf("default snapshot outside workspace review directory: %s", out)
	}
	// An identical capture reuses the saved snapshot instead of failing.
	if again, err := writeReviewOut(first, ""); err != nil || again != out {
		t.Fatalf("identical recapture: %s %v", again, err)
	}
	os.WriteFile(filepath.Join(repo, "main.txt"), []byte("small\n"), 0600)
	second, err := captureReview(repo, "")
	if err != nil {
		t.Fatalf("saved snapshot must not block a later capture: %v", err)
	}
	for _, f := range second.Files {
		if strings.HasPrefix(f.Path, ".irrigator") {
			t.Fatalf("saved snapshot entered review: %s", f.Path)
		}
	}
}

func TestLocalReviewMissingOutputParentIsExplained(t *testing.T) {
	repo := reviewTestRepo(t)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	err = writeReview(filepath.Join(repo, "missing", "snapshot"), s)
	if err == nil || !strings.Contains(err.Error(), "parent directory does not exist") {
		t.Fatalf("unclear missing-parent error: %v", err)
	}
}

func TestLocalReviewNamedOutputCreatesWorkspaceReviewDirectory(t *testing.T) {
	repo := reviewTestRepo(t)
	s, err := captureReview(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(repo, ".irrigator", "reviews", "start")
	if got, err := writeReviewOut(s, out); err != nil || got != out {
		t.Fatalf("documented --out .irrigator/reviews/NAME must work in a fresh workspace: %s %v", got, err)
	}
	if _, err = readReview(out); err != nil {
		t.Fatal(err)
	}
}
