//go:build integration

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

// GC retention contract. Automatic Git worktree collection is withdrawn: no
// authoritative worker lifetime protocol proves that a worktree, its worker or
// the worker's descendants are dead and owned by a run. GC therefore removes
// only run-scoped obsolete attempt diagnostics and never touches worktrees,
// branches, lock receipts, unrelated paths or other runs.

type gcRepo struct {
	base, repo, common, sibling, custom string
}

func gcGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable unavailable")
	}
	full := append([]string{"-C", dir, "-c", "user.name=Irrigator Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(out.String())
}

func gcWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gcRepoFixture creates a real repository with a dirty main worktree, a dirty
// sibling worktree under the ACP default root and a dirty custom-root worktree.
func gcRepoFixture(t *testing.T) gcRepo {
	t.Helper()
	base := t.TempDir()
	r := gcRepo{base: base, repo: filepath.Join(base, "repo"), sibling: filepath.Join(base, "irrigator-worktrees", "run00001", "a-a1"), custom: filepath.Join(t.TempDir(), "custom-root", "a-a2")}
	if err := os.MkdirAll(r.repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gcGit(t, r.repo, "init", "-q")
	gcGit(t, r.repo, "symbolic-ref", "HEAD", "refs/heads/main")
	gcWrite(t, filepath.Join(r.repo, "tracked.txt"), "committed\n")
	gcGit(t, r.repo, "add", "tracked.txt")
	gcGit(t, r.repo, "commit", "-q", "-m", "initial")
	gcGit(t, r.repo, "worktree", "add", "-q", "-b", "irrigator/run00001/a", r.sibling, "main")
	gcGit(t, r.repo, "worktree", "add", "-q", "-b", "irrigator/run00001/a-retry", r.custom, "main")
	for _, wt := range []string{r.repo, r.sibling, r.custom} {
		gcWrite(t, filepath.Join(wt, "tracked.txt"), "uncommitted edit in "+filepath.Base(wt)+"\n")
		gcWrite(t, filepath.Join(wt, "untracked", "work.txt"), "untracked work\n")
	}
	common := gcGit(t, r.repo, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(common) {
		common = filepath.Join(r.repo, common)
	}
	r.common = filepath.Clean(common)
	return r
}

// gcRunFixture creates a stopped run whose node "a" has two obsolete attempt
// logs (attempts 1 and 2) and current attempt 3 diagnostics.
func gcRunFixture(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Initialize(model.Plan{Name: "gc-retention", Tasks: []model.Task{runfixture.Task("a", 0)}}, root, "mock"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Status = "verified"
		s.Tasks["a"].Attempt = 3
		s.Tasks["a"].Result = map[string]string{"answer": "accepted"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"1.stdout.json", "2.check-0.stderr.log", "3.stdout.json", "3.input.json"} {
		gcWrite(t, filepath.Join(root, "cells", "a", name), "diagnostic "+name)
	}
}

var gcObsolete = []string{"1.stdout.json", "2.check-0.stderr.log"}
var gcCurrent = []string{"3.stdout.json", "3.input.json"}

// gcTree records every entry under root except the listed absolute paths.
func gcTree(t *testing.T, root string, skip ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, s := range skip {
			if path == s {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		rel, _ := filepath.Rel(root, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, e := os.Readlink(path)
			if e != nil {
				return e
			}
			out[rel] = "symlink:" + target
		case d.IsDir():
			out[rel] = "dir"
		case d.Type().IsRegular():
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			out[rel] = "file:" + string(data)
		default:
			out[rel] = "other:" + d.Type().String()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

func gcSameTree(t *testing.T, label string, before, after map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		for k, v := range before {
			if after[k] != v {
				t.Errorf("%s: %s changed or removed (before %q, after %q)", label, k, v, after[k])
			}
		}
		for k := range after {
			if _, ok := before[k]; !ok {
				t.Errorf("%s: unexpected new entry %s", label, k)
			}
		}
	}
}

// gcGitState captures registered worktrees and refs.
func gcGitState(t *testing.T, r gcRepo) string {
	t.Helper()
	return gcGit(t, r.repo, "worktree", "list", "--porcelain") + "\n--\n" + gcGit(t, r.repo, "for-each-ref", "--format=%(refname) %(objectname)")
}

func gcExitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		t.Fatalf("short-lived helper: %v", err)
	}
	return cmd.Process.Pid
}

// gcForgeReceipt writes a lock receipt in the format the withdrawn collector
// trusted. GC must never read it, act on it, or remove it.
func gcForgeReceipt(t *testing.T, r gcRepo, name, worktree, branch string, pid int) string {
	t.Helper()
	host, _ := os.Hostname()
	raw, err := json.Marshal(map[string]any{"run_id": "run00001-forged", "cell": "a", "attempt": 1, "worktree": worktree, "branch": branch, "pid": pid, "hostname": host, "start_time": "0.000000"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.common, "irrigator", "locks", name+".lock")
	gcWrite(t, path, string(raw))
	return path
}

func TestGCRefusesMissingRunWithoutFilesystemSideEffects(t *testing.T) {
	r := gcRepoFixture(t)
	gcForgeReceipt(t, r, "stale", r.sibling, "irrigator/run00001/a", gcExitedPID(t))
	gitBefore := gcGitState(t, r)
	repoBefore := gcTree(t, r.repo)
	siblingBefore := gcTree(t, r.sibling)
	customBefore := gcTree(t, r.custom)

	emptyDir := filepath.Join(r.base, "empty-run")
	if err := os.Mkdir(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(r.base, "state-dir-run")
	if err := os.MkdirAll(filepath.Join(stateDir, "state.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	plainFile := filepath.Join(r.base, "not-a-run")
	gcWrite(t, plainFile, "plain")

	typo := filepath.Join(r.repo, "runs-typo", "run")
	for _, tc := range []struct {
		name, target string
		mustNotExist []string
	}{
		{"typo", typo, []string{filepath.Join(r.repo, "runs-typo")}},
		{"repository-root", r.repo, []string{filepath.Join(r.repo, "controller.lock")}},
		{"sibling-worktree", r.sibling, []string{filepath.Join(r.sibling, "controller.lock")}},
		{"empty-directory", emptyDir, []string{filepath.Join(emptyDir, "controller.lock")}},
		{"state-not-regular", stateDir, []string{filepath.Join(stateDir, "controller.lock")}},
		{"regular-file", plainFile, nil},
	} {
		for _, mode := range []string{"--apply", "--dry-run"} {
			t.Run(tc.name+mode, func(t *testing.T) {
				code, err := runCommand([]string{"gc", tc.target, mode})
				if code == 0 || err == nil {
					t.Fatalf("gc accepted a non-run target: %d %v", code, err)
				}
				for _, p := range tc.mustNotExist {
					if _, e := os.Lstat(p); !errors.Is(e, os.ErrNotExist) {
						t.Fatalf("gc created %s on a non-run target: %v", p, e)
					}
				}
			})
		}
	}
	if entries, err := os.ReadDir(emptyDir); err != nil || len(entries) != 0 {
		t.Fatalf("empty target changed: %v %v", entries, err)
	}
	if data, err := os.ReadFile(plainFile); err != nil || string(data) != "plain" {
		t.Fatalf("plain file changed: %q %v", data, err)
	}
	if got := gcGitState(t, r); got != gitBefore {
		t.Fatalf("git worktrees/refs changed:\n%s\n---\n%s", gitBefore, got)
	}
	gcSameTree(t, "main worktree", repoBefore, gcTree(t, r.repo))
	gcSameTree(t, "sibling worktree", siblingBefore, gcTree(t, r.sibling))
	gcSameTree(t, "custom worktree", customBefore, gcTree(t, r.custom))
}

func TestGCRetainsWorktreesAndIgnoresForgedLockMetadata(t *testing.T) {
	r := gcRepoFixture(t)
	// Runs inside the repository: the withdrawn collector resolved the Git
	// common directory from the run path and swept the whole repository.
	run := filepath.Join(r.repo, ".runs", "run-a")
	other := filepath.Join(r.repo, ".runs", "run-b")
	gcRunFixture(t, run)
	gcRunFixture(t, other)
	victim := filepath.Join(t.TempDir(), "unrelated")
	gcWrite(t, filepath.Join(victim, "precious.txt"), "do not delete\n")

	dead := gcExitedPID(t)
	gcForgeReceipt(t, r, "run00001-a-a1", r.sibling, "irrigator/run00001/a", dead)
	gcForgeReceipt(t, r, "run00001-a-a2", r.custom, "irrigator/run00001/a-retry", dead)
	gcForgeReceipt(t, r, "victim", victim, "irrigator/run00001/victim", dead)
	gcForgeReceipt(t, r, "main", r.repo, "irrigator/run00001/main", dead)
	gcForgeReceipt(t, r, "other-run", other, "irrigator/run00001/b", dead)
	gcForgeReceipt(t, r, "missing-identity", r.sibling, "irrigator/run00001/a", 0)
	gcWrite(t, filepath.Join(r.common, "irrigator", "locks", "corrupt.lock"), "{not json")

	snapshot := func() map[string]map[string]string {
		return map[string]map[string]string{
			"main":     gcTree(t, r.repo, filepath.Join(r.repo, ".git"), run),
			"sibling":  gcTree(t, r.sibling),
			"custom":   gcTree(t, r.custom),
			"victim":   gcTree(t, victim),
			"other":    gcTree(t, other),
			"receipts": gcTree(t, filepath.Join(r.common, "irrigator")),
			"gitmeta":  gcTree(t, filepath.Join(r.common, "worktrees")),
		}
	}
	check := func(label string, before map[string]map[string]string, gitBefore string) {
		t.Helper()
		after := snapshot()
		for k := range before {
			gcSameTree(t, label+" "+k, before[k], after[k])
		}
		if got := gcGitState(t, r); got != gitBefore {
			t.Fatalf("%s: git worktrees/refs changed:\n%s\n---\n%s", label, gitBefore, got)
		}
	}
	before := snapshot()
	gitBefore := gcGitState(t, r)
	runCell := filepath.Join(run, "cells", "a")

	report, err := runops.GarbageCollect(run, 1, false)
	if err != nil || report.Applied || len(report.Files) != len(gcObsolete) {
		t.Fatalf("preview: %+v %v", report, err)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "worktree") {
		t.Fatalf("GC report claims worktree collection: %s", raw)
	}
	check("preview", before, gitBefore)

	// A live worker lease and a live controller each block GC entirely.
	lease, err := checkpoint.FileLock(filepath.Join(runCell, "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	code, err := runCommand([]string{"gc", run, "--apply"})
	checkpoint.Unlock(lease)
	if code == 0 || err == nil {
		t.Fatal("GC accepted an active worker lease")
	}
	controller, err := checkpoint.FileLock(filepath.Join(run, "controller.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	code, err = runCommand([]string{"gc", run, "--apply"})
	checkpoint.Unlock(controller)
	if code == 0 || err == nil {
		t.Fatal("GC accepted an active controller")
	}
	for _, name := range append(append([]string{}, gcObsolete...), gcCurrent...) {
		if _, e := os.Stat(filepath.Join(runCell, name)); e != nil {
			t.Fatalf("refused GC removed %s: %v", name, e)
		}
	}
	check("refused", before, gitBefore)

	if code, err = runCommand([]string{"gc", run, "--apply"}); code != 0 || err != nil {
		t.Fatalf("gc --apply: %d %v", code, err)
	}
	for _, name := range gcObsolete {
		if _, e := os.Lstat(filepath.Join(runCell, name)); !errors.Is(e, os.ErrNotExist) {
			t.Fatalf("obsolete attempt log %s retained: %v", name, e)
		}
	}
	for _, name := range gcCurrent {
		if _, e := os.Stat(filepath.Join(runCell, name)); e != nil {
			t.Fatalf("current attempt log %s removed: %v", name, e)
		}
	}
	check("applied", before, gitBefore)
	for _, wt := range []string{r.repo, r.sibling, r.custom} {
		data, e := os.ReadFile(filepath.Join(wt, "tracked.txt"))
		if e != nil || !strings.HasPrefix(string(data), "uncommitted edit") {
			t.Fatalf("dirty tracked edit lost in %s: %q %v", wt, data, e)
		}
	}
	if s := runfixture.Read(t, other); s.Tasks["a"].Attempt != 3 {
		t.Fatalf("other run changed: %+v", s.Tasks["a"])
	}
}
