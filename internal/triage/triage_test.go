//go:build unix

package triage

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

type env struct {
	t                   *testing.T
	root, run, repo, wt string
	runID               string
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newEnv makes a run with two cells: "a" from `irrigator code` (provider
// coder) and "b" from another command provider, both left uncertain.
func newEnv(t *testing.T) *env {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	e := &env{t: t, root: root, run: filepath.Join(root, "run"), repo: filepath.Join(root, "repo"), wt: filepath.Join(root, "wt")}
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "T")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	gitT(t, root, "init", "-q", "-b", "main", e.repo)
	gitT(t, e.repo, "commit", "-q", "--allow-empty", "-m", "base")
	plan := model.Plan{APIVersion: "swarm/v1", Name: "t", MaxTasks: 2,
		Providers: map[string]model.CommandSpec{
			"coder": {Command: []string{"/bin/irrigator", "_code-worker", "--agent", "claude", "--repo", e.repo, "--base", "main", "--worktrees", e.wt}},
			"other": {Command: []string{"/bin/true"}},
		},
		Tasks: []model.Task{{ID: "a", Provider: "coder", Prompt: "p", Requires: []string{"branch", "commit", "diffstat", "summary"}},
			{ID: "b", Provider: "other", Prompt: "p", Requires: []string{"answer"}}}}
	if err := checkpoint.Initialize(plan, e.run, "coder"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(e.run, func(s *model.State) error {
		e.runID = s.RunID
		for _, id := range []string{"a", "b"} {
			s.Tasks[id].Status, s.Tasks[id].Attempt, s.Tasks[id].Error = "uncertain", 1, "agent timed out"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// codeWork gives cell a a worktree with one commit and, optionally, an
// uncommitted file.
func (e *env) codeWork(dirty bool) string {
	run8 := e.runID[:8]
	wt := filepath.Join(e.wt, run8, "a-a1")
	gitT(e.t, e.repo, "worktree", "add", "-q", "-b", "irrigator/"+run8+"/a", wt, "main")
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("work\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	gitT(e.t, wt, "add", "-A")
	gitT(e.t, wt, "commit", "-q", "-m", "partial work")
	if dirty {
		if err := os.WriteFile(filepath.Join(wt, "g.txt"), []byte("more\n"), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	return wt
}

func (e *env) stopped(id string) {
	dir := filepath.Join(e.run, "cells", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1.process.json"), []byte(`{"status":"stopped","supervisor_pid":1}`), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func byID(r *report, id string) *cell {
	for _, c := range r.Cells {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func TestAnalyzeCodeCellEvidenceAndAdvice(t *testing.T) {
	e := newEnv(t)
	wt := e.codeWork(true)
	e.stopped("a")
	r, err := analyze(e.run)
	if err != nil {
		t.Fatal(err)
	}
	a, b := byID(r, "a"), byID(r, "b")
	if a == nil || b == nil || len(r.Cells) != 2 || r.ControllerActive {
		t.Fatalf("%+v", r)
	}
	if a.Process != "stopped" || b.Process != "none" {
		t.Fatalf("process a=%s b=%s", a.Process, b.Process)
	}
	if a.Code == nil || a.Code.Worktree != wt || !a.Code.WorktreeOK || a.Code.Commits != 1 || len(a.Code.Uncommitted) != 1 ||
		a.Code.Branch != "irrigator/"+e.runID[:8]+"/a" || a.Code.Diffstat != "1 file changed, 1 insertion(+)" {
		t.Fatalf("code evidence %+v", a.Code)
	}
	if !strings.Contains(a.Advice.Next, "uncommitted changes") || b.Code != nil || !strings.Contains(strings.Join(b.Advice.Commands, "\n"), "resolve") {
		t.Fatalf("advice a=%+v b=%+v", a.Advice, b.Advice)
	}
	if !strings.Contains(strings.Join(b.Advice.Commands, "\n"), "--confirm-stopped") || strings.Contains(strings.Join(a.Advice.Commands, "\n"), "--confirm-stopped") {
		t.Fatal("--confirm-stopped must appear only when the exit is unconfirmed")
	}
	var out bytes.Buffer
	printReport(&out, r)
	for _, want := range []string{"agent process: stopped (confirmed)", "uncommitted: ?? g.txt", "Nothing was changed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("print lacks %q:\n%s", want, out.String())
		}
	}
}

func TestAdviceForCleanBranchAndEmptyBranch(t *testing.T) {
	e := newEnv(t)
	r, _ := analyze(e.run)
	if a := byID(r, "a"); a.Code == nil || a.Code.BranchOK || !strings.Contains(a.Advice.Next, "no work") {
		t.Fatalf("missing branch: %+v %+v", a.Code, a.Advice)
	}
	e.codeWork(false)
	r, _ = analyze(e.run)
	if a := byID(r, "a"); a.Code.Commits != 1 || !strings.Contains(a.Advice.Next, "accept it") {
		t.Fatalf("%+v", a.Advice)
	}
}

func TestGuideCommitsLeftoversThenAcceptsBranch(t *testing.T) {
	e := newEnv(t)
	e.codeWork(true)
	r, err := analyze(e.run)
	if err != nil {
		t.Fatal(err)
	}
	var ran [][]string
	run := func(args []string) (int, error) { ran = append(ran, args); return 0, nil }
	// a: commit leftovers, then accept (exit unconfirmed: type stopped); b: skip; no resume.
	in := strings.NewReader("c\na\nstopped\ns\nn\n")
	var out bytes.Buffer
	if code, err := guide(r, in, &out, run); code != 0 || err != nil {
		t.Fatalf("%d %v\n%s", code, err, out.String())
	}
	if len(ran) != 1 || ran[0][0] != "resolve" || ran[0][2] != "a" || ran[0][len(ran[0])-1] != "--confirm-stopped" {
		t.Fatalf("ran %q\n%s", ran, out.String())
	}
	raw, err := os.ReadFile(ran[0][4])
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	branch := "irrigator/" + e.runID[:8] + "/a"
	if result["branch"] != branch || result["commit"] != gitT(t, e.repo, "rev-parse", branch) || !strings.Contains(result["summary"], "2 commit(s)") {
		t.Fatalf("result %v", result)
	}
	if n := gitT(t, e.repo, "rev-list", "--count", "main.."+branch); n != "2" {
		t.Fatalf("leftovers not committed: %s commits", n)
	}
	if !strings.Contains(out.String(), "irrigator resume") {
		t.Fatalf("no resume hint:\n%s", out.String())
	}
}

func TestGuideRetryNeedsTypedConfirmation(t *testing.T) {
	e := newEnv(t)
	r, _ := analyze(e.run)
	var ran [][]string
	run := func(args []string) (int, error) { ran = append(ran, args); return 0, nil }
	// a (no work): retry but refuse to confirm; b: retry and confirm; then resume.
	in := strings.NewReader("r\nno\nr\nstopped\ny\n")
	var out bytes.Buffer
	if _, err := guide(r, in, &out, run); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"retry", e.run, "b", "--allow-uncertain", "--confirm-stopped"}, {"resume", e.run}}
	got, _ := json.Marshal(ran)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Fatalf("ran %s, want %s\n%s", got, exp, out.String())
	}
}

func TestGuideRefusesWhileControllerRuns(t *testing.T) {
	e := newEnv(t)
	r, _ := analyze(e.run)
	r.ControllerActive = true
	if _, err := guide(r, strings.NewReader(""), &bytes.Buffer{}, nil); err == nil {
		t.Fatal("guided fixes must refuse while a controller runs")
	}
}

func TestNothingStuck(t *testing.T) {
	e := newEnv(t)
	if err := checkpoint.Update(e.run, func(s *model.State) error {
		for _, id := range []string{"a", "b"} {
			s.Tasks[id].Status, s.Tasks[id].Attempt, s.Tasks[id].Error = "pending", 0, ""
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, _ := analyze(e.run)
	var out bytes.Buffer
	printReport(&out, r)
	if len(r.Cells) != 0 || !strings.Contains(out.String(), "Nothing is stuck") {
		t.Fatalf("%s", out.String())
	}
}
