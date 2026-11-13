package coder

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// git runs git in dir and returns trimmed stdout.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// worktreesOn returns the worktrees whose branch matches: an exact branch, or
// every branch under a prefix ending in "/". Each entry is {path, branch}.
func worktreesOn(repo, branch string) ([][2]string, error) {
	out, err := git(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var found [][2]string
	path := ""
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			path = p
		} else if b, ok := strings.CutPrefix(line, "branch refs/heads/"); ok && path != "" {
			if b == branch || (strings.HasSuffix(branch, "/") && strings.HasPrefix(b, branch)) {
				found = append(found, [2]string{path, b})
			}
		}
	}
	return found, nil
}

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

var errDirty = errors.New("dirty worktree")

// retireEarlierAttempts removes clean worktrees holding the cell's branch and
// refuses ones with uncommitted work, so a retry can check the branch out.
func retireEarlierAttempts(repo, branch string) error {
	if _, err := git(repo, "worktree", "prune"); err != nil {
		return err
	}
	found, err := worktreesOn(repo, branch)
	if err != nil {
		return err
	}
	for _, wt := range found {
		if samePath(wt[0], repo) {
			return fmt.Errorf("%w: branch %s is checked out in the main repository at %s", errDirty, branch, wt[0])
		}
		status, err := git(wt[0], "status", "--porcelain")
		if err != nil {
			return err
		}
		if status != "" {
			return fmt.Errorf("%w: an earlier attempt's worktree at %s has uncommitted work; inspect it, commit or discard the work, then retry", errDirty, wt[0])
		}
		if _, err := git(repo, "worktree", "remove", wt[0]); err != nil {
			return err
		}
	}
	return nil
}

func branchExists(repo, branch string) bool {
	out, err := git(repo, "branch", "--list", branch)
	return err == nil && out != ""
}

// previousWork describes commits already on the branch beyond base, or "".
func previousWork(repo, baseSHA, baseName, branch, origin string) (string, error) {
	ahead, err := git(repo, "rev-list", "--count", baseSHA+".."+branch)
	if err != nil || ahead == "0" {
		return "", err
	}
	log, err := git(repo, "log", "--format=%h %s", baseSHA+".."+branch)
	if err != nil {
		return "", err
	}
	how := "already has " + ahead + " commit(s) beyond " + baseName + " from an earlier attempt"
	if origin != "" {
		how = "was started from " + origin + " and already has " + ahead + " commit(s) beyond " + baseName
	}
	return fmt.Sprintf("Branch %s %s. Review that work (git log, git diff %s...HEAD) and continue it rather than starting over.\n\nCommits so far:\n%s",
		branch, how, baseName, log), nil
}
