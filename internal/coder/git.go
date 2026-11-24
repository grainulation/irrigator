package coder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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

const (
	maxStatusBytes   = 65536
	maxStatusEntries = 64
	maxPathBytes     = 512
	statusTimeout    = 10 * time.Second
)

type statusEntry struct {
	Status       string `json:"status"`
	Path         string `json:"path"`
	OriginalPath string `json:"original_path,omitempty"`
}

type worktreeStatus struct {
	Basis     string        `json:"basis"`
	Complete  bool          `json:"complete"`
	Dirty     *bool         `json:"dirty"`
	Truncated bool          `json:"truncated"`
	TimedOut  bool          `json:"timed_out"`
	Entries   []statusEntry `json:"entries"`
}

// captureWorktreeStatus runs `git -C worktree status --porcelain=v1 -z --untracked-files=all`
// with strict byte, entry, path, and timeout bounds.
func captureWorktreeStatus(worktree string, timeout time.Duration) worktreeStatus {
	if timeout <= 0 {
		timeout = statusTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return worktreeStatus{Basis: "git_status", Entries: []statusEntry{}}
	}
	if err := cmd.Start(); err != nil {
		return worktreeStatus{Basis: "git_status", Entries: []statusEntry{}}
	}

	raw, readErr := io.ReadAll(io.LimitReader(stdout, maxStatusBytes+1))
	tooLarge := len(raw) > maxStatusBytes
	if tooLarge && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = stdout.Close()
	waitErr := cmd.Wait()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)

	if readErr != nil && len(raw) == 0 {
		return worktreeStatus{Basis: "git_status", TimedOut: timedOut, Entries: []statusEntry{}}
	}
	bounded := raw
	if tooLarge {
		bounded = raw[:maxStatusBytes]
	}
	entries, clipped, moreEntries := parsePorcelainZ(bounded, maxStatusEntries, maxPathBytes)
	truncated := tooLarge || clipped || moreEntries
	okExit := waitErr == nil

	var dirty *bool
	if okExit || len(raw) > 0 {
		d := len(raw) > 0
		dirty = &d
	}
	return worktreeStatus{
		Basis:     "git_status",
		Complete:  okExit && !timedOut && !truncated,
		Dirty:     dirty,
		Truncated: truncated,
		TimedOut:  timedOut,
		Entries:   entries,
	}
}

func parsePorcelainZ(raw []byte, maxEntries, maxPath int) (entries []statusEntry, clipped, more bool) {
	entries = []statusEntry{}
	records := bytes.Split(raw, []byte{0})
	i := 0
	for i < len(records) && len(records[i]) > 0 && len(entries) < maxEntries {
		rec := records[i]
		i++
		if len(rec) < 4 {
			continue
		}
		xy := string(rec[:2])
		p := rec[3:]
		if len(p) > maxPath {
			clipped = true
			p = p[:maxPath]
		}
		item := statusEntry{Status: xy, Path: strings.ToValidUTF8(string(p), "?")}
		if xy[0] == 'R' || xy[0] == 'C' || xy[1] == 'R' || xy[1] == 'C' {
			var orig []byte
			if i < len(records) {
				orig = records[i]
				i++
			}
			if len(orig) > maxPath {
				clipped = true
				orig = orig[:maxPath]
			}
			item.OriginalPath = strings.ToValidUTF8(string(orig), "?")
		}
		entries = append(entries, item)
	}
	for ; i < len(records); i++ {
		if len(records[i]) > 0 {
			more = true
			break
		}
	}
	return entries, clipped, more
}

func diffStatusEntries(before, after []statusEntry) []statusEntry {
	prev := make(map[string]string, len(before))
	for _, e := range before {
		prev[e.Path+"\x00"+e.OriginalPath] = e.Status
	}
	var changed []statusEntry
	for _, e := range after {
		if oldStatus, ok := prev[e.Path+"\x00"+e.OriginalPath]; !ok || oldStatus != e.Status {
			changed = append(changed, e)
		}
	}
	return changed
}

func parseSeedList(rawValues []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, item := range rawValues {
		for _, part := range strings.Split(item, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			clean, err := cleanSeedRelPath(trimmed)
			if err != nil {
				return nil, err
			}
			if !seen[clean] {
				seen[clean] = true
				out = append(out, clean)
			}
		}
	}
	return out, nil
}

func cleanSeedRelPath(raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, 0) || strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("invalid seed-ignored path %q", raw)
	}
	if strings.HasPrefix(raw, "/") || (len(raw) >= 2 && raw[1] == ':') || filepath.IsAbs(raw) {
		return "", fmt.Errorf("seed-ignored path %q must be repository-relative", raw)
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("seed-ignored path %q escapes repository", raw)
	}
	slash := filepath.ToSlash(clean)
	if slash == ".git" || strings.HasPrefix(slash, ".git/") {
		return "", fmt.Errorf("seed-ignored path %q cannot target .git", raw)
	}
	return slash, nil
}

// seedIgnoredPaths seeds gitignored files or directories from repo into worktree
// using copy-on-write clones where supported (macOS APFS `cp -c`, Linux `cp --reflink=auto`)
// and falling back to regular copies. Tracked or unignored paths are refused.
func seedIgnoredPaths(repo, worktree string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	evalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		evalRepo, err = filepath.Abs(repo)
		if err != nil {
			return nil, err
		}
	}
	var seeded []string
	for _, rel := range paths {
		clean, err := cleanSeedRelPath(rel)
		if err != nil {
			return seeded, err
		}
		nativeRel := filepath.FromSlash(clean)
		src := filepath.Join(repo, nativeRel)
		info, err := os.Lstat(src)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return seeded, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return seeded, fmt.Errorf("seed-ignored path %q must not be a symlink", clean)
		}
		evalSrc, err := filepath.EvalSymlinks(src)
		if err != nil || !strings.HasPrefix(evalSrc, evalRepo+string(filepath.Separator)) {
			return seeded, fmt.Errorf("seed-ignored path %q resolves outside repository", clean)
		}
		if tracked, _ := git(repo, "ls-files", "--", clean); tracked != "" {
			return seeded, fmt.Errorf("seed-ignored path %q contains tracked files", clean)
		}
		if err := exec.Command("git", "-C", repo, "check-ignore", "-q", "--", clean).Run(); err != nil {
			return seeded, fmt.Errorf("seed-ignored path %q is not gitignored", clean)
		}
		dst := filepath.Join(worktree, nativeRel)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return seeded, err
		}
		if err := cloneOrCopyPath(src, dst); err != nil {
			return seeded, fmt.Errorf("seed %q: %w", clean, err)
		}
		seeded = append(seeded, clean)
	}
	return seeded, nil
}

func cloneOrCopyPath(src, dst string) error {
	switch runtime.GOOS {
	case "darwin":
		if err := exec.Command("cp", "-c", "-R", src, dst).Run(); err == nil {
			return nil
		}
		_ = os.RemoveAll(dst)
	case "linux":
		if err := exec.Command("cp", "--reflink=auto", "-a", src, dst).Run(); err == nil {
			return nil
		}
		_ = os.RemoveAll(dst)
	}
	return copyTree(src, dst)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(linkTarget) {
				return fmt.Errorf("refusing absolute symlink %q -> %q", rel, linkTarget)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(rel), linkTarget))
			if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
				return fmt.Errorf("refusing escaping symlink %q -> %q", rel, linkTarget)
			}
			return os.Symlink(linkTarget, target)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file %q", rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}
