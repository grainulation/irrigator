package runfixture

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func FormationGit(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func FixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping git integration test in short mode")
	}
	out, err := FormationGit(dir, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func FixtureWrite(t *testing.T, root, path, content string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func FixtureRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	FixtureGit(t, repo, "init", "-q", "-b", "main")
	FixtureGit(t, repo, "config", "user.name", "Fixture")
	FixtureGit(t, repo, "config", "user.email", "fixture@example.invalid")
	for _, p := range []string{"a", "b"} {
		FixtureWrite(t, repo, "packages/"+p+"/package.json", `{"dependencies":{"fixture-lib":"1.0.0"}}`)
		FixtureWrite(t, repo, "packages/"+p+"/source.txt", "api-v1")
	}
	FixtureWrite(t, repo, "pnpm-lock.yaml", "fixture-v1\n")
	FixtureWrite(t, repo, "README.md", "unchanged\n")
	FixtureGit(t, repo, "add", ".")
	FixtureGit(t, repo, "commit", "-qm", "base")
	return repo, FixtureGit(t, repo, "rev-parse", "HEAD")
}

func FixtureWorktree(t *testing.T, repo, commit string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tree")
	FixtureGit(t, repo, "worktree", "add", "--detach", dir, commit)
	t.Cleanup(func() { _, _ = FormationGit(repo, nil, "worktree", "remove", "--force", dir) })
	return dir
}
