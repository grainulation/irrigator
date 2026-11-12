package branchreview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestLocalReviewRequestKindsOutsideGit(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	workspace := runfixture.CanonicalContextDir(t)
	body := "# Proposed direction\nKeep terminal primary.\nOpen supporting views intentionally.\n"
	if err := os.WriteFile(filepath.Join(workspace, "direction.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"plan", "direction", "decision"} {
		t.Run(kind, func(t *testing.T) {
			out := filepath.Join(workspace, kind)
			s, err := requestReview(workspace, kind, "Terminal workspace", "direction.md", out)
			if err != nil {
				t.Fatal(err)
			}
			if s.Kind != kind || s.Title != "Terminal workspace" || s.Body != body || s.BaseCommit != "" {
				t.Fatalf("wrong request: %+v", s)
			}
			if err = writeReview(out, s); err != nil {
				t.Fatal(err)
			}
			saved, err := readReview(out)
			if err != nil || saved.ID != s.ID {
				t.Fatalf("request not immutable/readable: %+v %v", saved, err)
			}
			comment, err := addReviewComment(out, "review.md", 2, "Show Review only when requested.")
			if err != nil {
				t.Fatal(err)
			}
			feedback, err := reviewFeedback(saved, []reviewComment{*comment})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(feedback, "Kind: "+kind) || !strings.Contains(feedback, "Title: Terminal workspace") || !strings.Contains(feedback, s.ID) || !strings.Contains(feedback, "> Show Review only when requested.") {
				t.Fatal(feedback)
			}
			if err = writeReview(out, s); err == nil {
				t.Fatal("request overwritten")
			}
		})
	}
	// The selected file can change later; the existing review remains pinned.
	os.WriteFile(filepath.Join(workspace, "direction.md"), []byte("different direction"), 0600)
	saved, err := readReview(filepath.Join(workspace, "plan"))
	if err != nil || saved.Body != body {
		t.Fatal("request followed later file edits", err)
	}
}
func TestLocalReviewRequestRejectsUnsafeInput(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	workspace := runfixture.CanonicalContextDir(t)
	file := filepath.Join(workspace, "plan.md")
	os.WriteFile(file, []byte("A direction for review.\n"), 0600)
	out := filepath.Join(workspace, "snapshot")
	for _, kind := range []string{"", "code", "shell", "approval", "PLAN"} {
		if _, err := requestReview(workspace, kind, "Title", file, out); err == nil {
			t.Fatalf("accepted kind %q", kind)
		}
	}
	for _, title := range []string{"", " ", "title\ncommand", strings.Repeat("x", 201), string([]byte{0xff})} {
		if _, err := requestReview(workspace, "plan", title, file, out); err == nil {
			t.Fatalf("accepted title %q", title)
		}
	}
	outside := filepath.Join(runfixture.CanonicalContextDir(t), "outside.md")
	os.WriteFile(outside, []byte("outside"), 0600)
	if _, err := requestReview(workspace, "plan", "Title", outside, out); err == nil {
		t.Fatal("outside body accepted")
	}
	link := filepath.Join(workspace, "linked.md")
	os.Symlink(file, link)
	if _, err := requestReview(workspace, "plan", "Title", link, out); err == nil {
		t.Fatal("linked body accepted")
	}
	linkedWorkspace := filepath.Join(runfixture.CanonicalContextDir(t), "workspace")
	os.Symlink(workspace, linkedWorkspace)
	if _, err := requestReview(linkedWorkspace, "plan", "Title", "plan.md", out); err == nil {
		t.Fatal("linked workspace accepted")
	}
	for _, data := range [][]byte{[]byte(" \n"), {0, 1}, {0xff}, make([]byte, (1<<20)+1)} {
		os.WriteFile(file, data, 0600)
		if _, err := requestReview(workspace, "plan", "Title", file, out); err == nil {
			t.Fatal("malformed or oversized body accepted")
		}
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("invalid request created snapshot output")
	}
}
func TestLocalReviewRequestHashBindsKindAndTitle(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	workspace := runfixture.CanonicalContextDir(t)
	file := filepath.Join(workspace, "plan.md")
	os.WriteFile(file, []byte("One proposed change.\n"), 0600)
	out := filepath.Join(workspace, "snapshot")
	s, err := requestReview(workspace, "decision", "Choose a layout", file, out)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeReview(out, s); err != nil {
		t.Fatal(err)
	}
	s.Kind = "direction"
	data, _ := json.Marshal(s)
	os.WriteFile(filepath.Join(out, "snapshot.json"), data, 0600)
	if _, err = readReview(out); err == nil {
		t.Fatal("changed kind preserved snapshot identity")
	}
	s.Kind = "decision"
	s.Title = "Different choice"
	data, _ = json.Marshal(s)
	os.WriteFile(filepath.Join(out, "snapshot.json"), data, 0600)
	if _, err = readReview(out); err == nil {
		t.Fatal("changed title preserved snapshot identity")
	}
}
func TestLocalReviewRequestRespectsLaunchWorkspace(t *testing.T) {
	file, c := runfixture.ContextCapsuleFixture(t)
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", file)
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", c.Registry)
	bodyFile := filepath.Join(c.Workspace, "plan.md")
	os.WriteFile(bodyFile, []byte("Please review this plan."), 0600)
	outside := runfixture.CanonicalContextDir(t)
	if _, err := requestReview(c.Workspace, "plan", "Plan", bodyFile, filepath.Join(outside, "snapshot")); err == nil {
		t.Fatal("request output escaped launch workspace")
	}
	os.WriteFile(filepath.Join(outside, "plan.md"), []byte("outside"), 0600)
	if _, err := requestReview(outside, "plan", "Plan", "plan.md", filepath.Join(c.Workspace, "snapshot")); err == nil {
		t.Fatal("request workspace escaped launch workspace")
	}
}
