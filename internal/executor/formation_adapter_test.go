package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Capture the final file state using an isolated index seeded from base plus the
// worker's own tracked inventory. No global env change or live-index mutation.
func fixtureCapture(repo, worktree, base string) (string, error) {
	trackedCmd := exec.Command("git", "-C", worktree, "ls-files", "-z")
	tracked, err := trackedCmd.Output()
	if err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp("", "formation-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(temp, "index")}
	if _, err = runfixture.FormationGit(repo, env, "read-tree", base); err != nil {
		return "", err
	}
	if len(tracked) > 0 {
		cmd := exec.Command("git", "-C", repo, "--work-tree="+worktree, "update-index", "--add", "--remove", "-z", "--stdin")
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin = bytes.NewReader(tracked)
		if output, e := cmd.CombinedOutput(); e != nil {
			return "", fmt.Errorf("tracked capture: %w: %s", e, output)
		}
	}
	if _, err = runfixture.FormationGit(repo, env, "--work-tree="+worktree, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	return runfixture.FormationGit(repo, env, "write-tree")
}
func fixtureBoundary(repo, from, to string, scopes []string) error {
	cmd := exec.Command("git", "-C", repo, "diff-tree", "-r", "--no-renames", "--name-only", "-z", from, to)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		p := string(raw)
		allowed := false
		for _, scope := range scopes {
			allowed = allowed || p == scope || strings.HasPrefix(p, scope+"/")
		}
		if !allowed {
			return fmt.Errorf("SCOPE_VIOLATION: %s", p)
		}
	}
	return nil
}
func fixtureCommit(repo, tree, base string) (string, error) {
	return runfixture.FormationGit(repo, nil, "commit-tree", tree, "-p", base, "-m", "formation fixture")
}
func fixtureAssemble(repo, base string, trees map[string]string) (string, error) {
	scopes := make([]string, 0, len(trees))
	for p := range trees {
		scopes = append(scopes, p)
	}
	sort.Strings(scopes)
	current := base
	for _, p := range scopes {
		tree := trees[p]
		if err := fixtureBoundary(repo, base, tree, []string{p}); err != nil {
			return "", err
		}
		worker, err := fixtureCommit(repo, tree, base)
		if err != nil {
			return "", err
		}
		merged, err := runfixture.FormationGit(repo, nil, "merge-tree", "--write-tree", "--merge-base="+base, current, worker)
		if err != nil {
			return "", err
		}
		current, err = fixtureCommit(repo, merged, current)
		if err != nil {
			return "", err
		}
	}
	tree, err := runfixture.FormationGit(repo, nil, "rev-parse", current+"^{tree}")
	if err != nil {
		return "", err
	}
	if err = fixtureBoundary(repo, base, tree, scopes); err != nil {
		return "", err
	}
	return current, nil
}

// A child test process implements the actual external() validation envelope.
// It runs deterministic fixture assertions, never pnpm, a registry or a model.
func TestFormationAdapterProtocolHelper(t *testing.T) {
	if os.Getenv("IRR_FORMATION_HELPER") != "1" {
		return
	}
	var req model.WorkRequest
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&req); err != nil {
		os.Exit(2)
	}
	if req.Protocol != model.ProtocolVersion || req.Kind != "validate" || req.RequestID == "" {
		os.Exit(2)
	}
	cwd, err := os.Getwd()
	expectedDir, pathErr := filepath.EvalSymlinks(req.Context["fixture_worktree"])
	passed := err == nil && pathErr == nil && cwd == expectedDir
	for _, p := range strings.Split(req.Context["fixture_packages"], ",") {
		manifest, e := os.ReadFile(filepath.Join(cwd, p, "package.json"))
		source, e2 := os.ReadFile(filepath.Join(cwd, p, "source.txt"))
		passed = passed && e == nil && e2 == nil && strings.Contains(string(manifest), "2.0.0") && string(source) == "api-v2"
	}
	response := model.WorkResponse{Protocol: req.Protocol, RequestID: req.RequestID, Passed: &passed}
	if req.Context["bad_id"] == "true" {
		response.RequestID = "wrong"
	}
	if json.NewEncoder(os.Stdout).Encode(response) != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func fixtureVerify(t *testing.T, worktree string, packages []string, badID bool) bool {
	t.Helper()
	t.Setenv("IRR_FORMATION_HELPER", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "validate", RequestID: "fixture-validation", OperationID: "fixture", Attempt: 1, TimeoutMS: 5000, Context: map[string]string{"fixture_worktree": worktree, "fixture_packages": strings.Join(packages, ",")}}
	if badID {
		req.Context["bad_id"] = "true"
	}
	response, err := External(ctx, worktree, filepath.Join(t.TempDir(), "check"), model.CommandSpec{Command: []string{os.Args[0], "-test.run=^TestFormationAdapterProtocolHelper$"}, Env: []string{"IRR_FORMATION_HELPER"}}, req)
	if badID {
		return err != nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return response.Passed != nil && *response.Passed
}
func fixtureWorker(t *testing.T, repo, base, pkg string, fail bool) (string, string, error) {
	t.Helper()
	w := runfixture.FixtureWorktree(t, repo, base)
	runfixture.FixtureWrite(t, w, pkg+"/package.json", `{"dependencies":{"fixture-lib":"2.0.0"}}`)
	source := "api-v2"
	if fail {
		source = "broken"
	}
	runfixture.FixtureWrite(t, w, pkg+"/source.txt", source)
	if !fixtureVerify(t, w, []string{pkg}, false) {
		return w, "", errors.New("fixture verification failed")
	}
	tree, err := fixtureCapture(repo, w, base)
	if err != nil {
		return w, "", err
	}
	if err = fixtureBoundary(repo, base, tree, []string{pkg}); err != nil {
		return w, "", err
	}
	return w, tree, nil
}
func TestFormationAdapterTwoPackageSuccess(t *testing.T) {
	repo, base := runfixture.FixtureRepo(t)
	template, in := runfixture.FormationFixture(t)
	in.BaseCommit = base
	in.Packages = []string{"packages/a", "packages/b"}
	plan, err := runfixture.CompilePackageUpgrade(template, in)
	if err != nil {
		t.Fatal(err)
	}
	state := &model.State{Tasks: map[string]*model.TaskState{}}
	trees := map[string]string{}
	for i, pkg := range in.Packages {
		_, tree, err := fixtureWorker(t, repo, base, pkg, false)
		if err != nil {
			t.Fatal(err)
		}
		trees[pkg] = tree
		value, _ := json.Marshal(tree)
		state.Tasks[plan.Tasks[i].ID] = &model.TaskState{Status: "verified", Result: map[string]string{"candidate_tree_oid": string(value)}}
	}
	selected, err := managed.InputsForTask(state, plan.Tasks[2])
	if err != nil {
		t.Fatal(err)
	}
	// Assembly consumes the selected artifacts, not an out-of-band tree list.
	for i, pkg := range in.Packages {
		var oid string
		if err = json.Unmarshal([]byte(selected[plan.Tasks[i].ID]["candidate_tree_oid"]), &oid); err != nil {
			t.Fatal(err)
		}
		trees[pkg] = oid
	}
	assembled, err := fixtureAssemble(repo, base, trees)
	if err != nil {
		t.Fatal(err)
	}
	w := runfixture.FixtureWorktree(t, repo, assembled)
	runfixture.FixtureWrite(t, w, "pnpm-lock.yaml", "fixture-v2\n")
	if !fixtureVerify(t, w, in.Packages, false) {
		t.Fatal("integration checks failed")
	}
	final, err := fixtureCapture(repo, w, assembled)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixtureBoundary(repo, assembled, final, []string{"package.json", "pnpm-lock.yaml"}); err != nil {
		t.Fatal(err)
	}
	candidate, err := fixtureCommit(repo, final, base)
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/irrigator/candidates/fixture"
	zero := strings.Repeat("0", len(base))
	runfixture.FixtureGit(t, repo, "update-ref", ref, candidate, zero)
	if _, err = runfixture.FormationGit(repo, nil, "update-ref", ref, candidate, zero); err == nil {
		t.Fatal("duplicate publication succeeded")
	}
	if runfixture.FixtureGit(t, repo, "rev-parse", "HEAD") != base || runfixture.FixtureGit(t, repo, "status", "--porcelain") != "" {
		t.Fatal("primary checkout changed")
	}
	if runfixture.FixtureGit(t, repo, "show", candidate+":README.md") != "unchanged" {
		t.Fatal("base content lost")
	}
	// Extra integration writes fail the second delta, even after successful checks.
	runfixture.FixtureWrite(t, w, "packages/a/leak.txt", "bad")
	bad, err := fixtureCapture(repo, w, assembled)
	if err != nil {
		t.Fatal(err)
	}
	if fixtureBoundary(repo, assembled, bad, []string{"pnpm-lock.yaml", "package.json"}) == nil {
		t.Fatal("integration scope violation accepted")
	}
}
func TestFormationAdapterScopeViolation(t *testing.T) {
	repo, base := runfixture.FixtureRepo(t)
	w := runfixture.FixtureWorktree(t, repo, base)
	runfixture.FixtureWrite(t, w, "packages/b/leak.txt", "leak")
	runfixture.FixtureGit(t, w, "add", ".")
	runfixture.FixtureGit(t, w, "commit", "-qm", "out of scope")
	runfixture.FixtureWrite(t, w, ".gitignore", "packages/b/leak.txt\n")
	runfixture.FixtureGit(t, w, "add", ".")
	runfixture.FixtureGit(t, w, "commit", "-qm", "ignore leak")
	tree, err := fixtureCapture(repo, w, base)
	if err != nil {
		t.Fatal(err)
	}
	// Test the leak independently of the additional root .gitignore violation.
	if err = fixtureBoundary(repo, base, tree, []string{".gitignore", "packages/a"}); err == nil || !strings.Contains(err.Error(), "packages/b/leak.txt") {
		t.Fatalf("missed ignored tracked leak: %v", err)
	}
	if runfixture.FixtureGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/irrigator/candidates") != "" {
		t.Fatal("rejected candidate published")
	}
}
func TestFormationAdapterWorkerFailureBlocksIntegration(t *testing.T) {
	repo, base := runfixture.FixtureRepo(t)
	template, in := runfixture.FormationFixture(t)
	in.BaseCommit = base
	plan, err := runfixture.CompilePackageUpgrade(template, in)
	if err != nil {
		t.Fatal(err)
	}
	w, artifact, workerErr := fixtureWorker(t, repo, base, "packages/b", true)
	if workerErr == nil || artifact != "" {
		t.Fatal("failed worker emitted artifact")
	}
	if _, err = os.Stat(w); err != nil {
		t.Fatal("failed worktree lost")
	}
	state := &model.State{Tasks: map[string]*model.TaskState{}}
	for _, task := range plan.Tasks[:2] {
		state.Tasks[task.ID] = &model.TaskState{Status: "failed"}
	}
	if _, err = managed.InputsForTask(state, plan.Tasks[2]); err != launch.ErrNoWork {
		t.Fatalf("integration not blocked: %v", err)
	}
	if !fixtureVerify(t, w, []string{"packages/b"}, true) {
		t.Fatal("mismatched protocol identity accepted")
	}
}

// Test-only application policy, serialized only by this fixture's single caller.
// It is not a production Review command or atomic protection against external writers.
func fixtureApply(repo, base, candidate string, accepted bool) error {
	if !accepted {
		return errors.New("ACCEPTANCE_REQUIRED")
	}
	status, err := runfixture.FormationGit(repo, nil, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("DIRTY_WORKTREE_DETECTED")
	}
	branch, err := runfixture.FormationGit(repo, nil, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if branch != "main" {
		return errors.New("WRONG_BRANCH")
	}
	head, err := runfixture.FormationGit(repo, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != base {
		return errors.New("STALE_TARGET_BASE")
	}
	_, err = runfixture.FormationGit(repo, nil, "merge", "--ff-only", candidate)
	return err
}
func TestFormationAdapterApplicationGuards(t *testing.T) {
	for _, mode := range []string{"unaccepted", "dirty", "stale", "success"} {
		t.Run(mode, func(t *testing.T) {
			repo, base := runfixture.FixtureRepo(t)
			w := runfixture.FixtureWorktree(t, repo, base)
			runfixture.FixtureWrite(t, w, "packages/a/source.txt", "api-v2")
			tree, err := fixtureCapture(repo, w, base)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := fixtureCommit(repo, tree, base)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "dirty":
				runfixture.FixtureWrite(t, repo, "untracked.txt", "keep")
			case "stale":
				runfixture.FixtureWrite(t, repo, "README.md", "advanced")
				runfixture.FixtureGit(t, repo, "add", ".")
				runfixture.FixtureGit(t, repo, "commit", "-qm", "advance")
			}
			before := runfixture.FixtureGit(t, repo, "rev-parse", "HEAD")
			index := runfixture.FixtureGit(t, repo, "write-tree")
			status := runfixture.FixtureGit(t, repo, "status", "--porcelain=v1", "--untracked-files=all")
			err = fixtureApply(repo, base, candidate, mode != "unaccepted")
			if mode == "success" {
				if err != nil || runfixture.FixtureGit(t, repo, "rev-parse", "HEAD") != candidate {
					t.Fatalf("apply failed: %v", err)
				}
				return
			}
			expected := map[string]string{"unaccepted": "ACCEPTANCE_REQUIRED", "dirty": "DIRTY_WORKTREE_DETECTED", "stale": "STALE_TARGET_BASE"}[mode]
			if err == nil || err.Error() != expected {
				t.Fatalf("wanted %s, got %v", expected, err)
			}
			if runfixture.FixtureGit(t, repo, "rev-parse", "HEAD") != before || runfixture.FixtureGit(t, repo, "write-tree") != index || runfixture.FixtureGit(t, repo, "status", "--porcelain=v1", "--untracked-files=all") != status {
				t.Fatal("rejection mutated checkout")
			}
		})
	}
}
