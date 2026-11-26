package branchreview

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/review"
	"github.com/grainulation/irrigator/internal/session"
)

const maxPruneDays = int((1<<63 - 1) / (24 * time.Hour))

// Pin storage before enumeration and retain the handle through deletion. Neither
// an ancestor symlink nor a replacement of its pathname can redirect removal.
func openPruneRoot(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("prune storage %s must be a real directory", name)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		return nil, errors.New("prune storage changed during open")
	}
	return root, nil
}

func pruneReadFile(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("prune evidence must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("prune evidence exceeds limit")
	}
	return data, err
}

func pruneDirSize(root *os.Root, name string) int64 {
	var n int64
	_ = fs.WalkDir(root.FS(), name, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

type pruneCandidate struct {
	root *os.Root
	name string
	info os.FileInfo
}

func (c pruneCandidate) remove() error {
	current, err := c.root.Lstat(c.name)
	if err != nil {
		return err
	}
	if !os.SameFile(c.info, current) || !c.info.ModTime().Equal(current.ModTime()) || c.info.Size() != current.Size() {
		return errors.New("prune candidate changed; preview again")
	}
	return c.root.RemoveAll(c.name)
}

// PruneReviews previews or removes eligible local review storage. Selection is
// recomputed for each invocation; an earlier preview is not a deletion token.
func PruneReviews(ws string, olderThanDays int, apply bool) (report *pruneReport, resultErr error) {
	if olderThanDays < 1 || olderThanDays > maxPruneDays {
		return nil, fmt.Errorf("--older-than must be between 1 and %d days", maxPruneDays)
	}
	r := &pruneReport{Items: []pruneItem{}}
	defer func() {
		if resultErr == nil {
			r.Applied = apply
		} else {
			r.Error = resultErr.Error()
			if !apply {
				// An incomplete inventory is not a usable deletion preview.
				r.Items = []pruneItem{}
				r.Bytes = 0
			}
		}
	}()
	workspace, err := localfs.Root(ws)
	if err != nil {
		return r, err
	}
	defer workspace.Close()
	private, err := openPruneRoot(workspace, ".irrigator")
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	defer private.Close()
	roots := map[string]*os.Root{}
	for _, name := range []string{"reviews", "changes", "sessions"} {
		root, err := openPruneRoot(private, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return r, err
		}
		defer root.Close()
		roots[name] = root
		r.UsageBytes += pruneDirSize(root, ".")
	}
	var candidates []pruneCandidate
	var branches map[string]bool
	add := func(kind, name, reason string, info os.FileInfo, size int64) {
		r.Items = append(r.Items, pruneItem{Path: filepath.Join(ws, ".irrigator", kind, name), Reason: reason, Bytes: size})
		r.Bytes += size
		candidates = append(candidates, pruneCandidate{roots[kind], name, info})
	}
	cutoff := time.Now().Add(-time.Duration(olderThanDays) * 24 * time.Hour)
	for _, kind := range []string{"reviews", "sessions", "changes"} {
		root := roots[kind]
		if root == nil {
			continue
		}
		entries, err := fs.ReadDir(root.FS(), ".")
		if err != nil {
			return r, err
		}
		for _, entry := range entries {
			name := entry.Name()
			info, err := root.Lstat(name)
			if err != nil {
				return r, err
			}
			switch kind {
			case "reviews":
				if info.IsDir() && !info.ModTime().After(cutoff) {
					add(kind, name, fmt.Sprintf("snapshot older than %d days", olderThanDays), info, pruneDirSize(root, name))
				}
			case "sessions":
				if info.Mode().IsRegular() && strings.HasSuffix(name, ".jsonl") && !info.ModTime().After(cutoff) {
					add(kind, name, fmt.Sprintf("agent session idle for %d days", olderThanDays), info, info.Size())
				}
			case "changes":
				if !info.Mode().IsRegular() || !strings.HasSuffix(name, ".json") {
					continue
				}
				data, err := pruneReadFile(root, name, changeStateLimit)
				if err != nil {
					return r, err
				}
				var state changeState
				if json.Unmarshal(data, &state) != nil || state.Version != changeStateVersion || state.Subject == "" || name != changeStateID(state.Subject)+".json" {
					continue // Unrecognized state is never evidence of an orphan.
				}
				reason := ""
				if id, ok := strings.CutPrefix(state.Subject, "snapshot:"); ok {
					digest, err := hex.DecodeString(id)
					if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != id {
						continue
					}
					exists, err := pruneSnapshotExists(roots["reviews"], id)
					if err != nil {
						return r, err
					}
					if !exists {
						reason = "its snapshot was removed"
					}
				} else {
					if _, err := session.ReviewGit(ws, "check-ref-format", "refs/heads/"+state.Subject); err != nil {
						var exit *exec.ExitError
						if errors.As(err, &exit) && exit.ExitCode() == 1 {
							continue
						}
						return r, fmt.Errorf("cannot recognize branch subject: %w", err)
					}
					if branches == nil {
						branches, err = pruneBranchRefs(ws)
						if err != nil {
							return r, err
						}
					}
					if !branches["refs/heads/"+state.Subject] {
						reason = "branch " + state.Subject + " no longer exists"
					}
				}
				if reason != "" {
					add(kind, name, reason, info, int64(len(data)))
				}
			}
		}
	}
	if apply {
		if err := applyPruneCandidates(r, candidates); err != nil {
			return r, err
		}
	}
	return r, nil
}

func applyPruneCandidates(r *pruneReport, candidates []pruneCandidate) error {
	for i, candidate := range candidates {
		if err := candidate.remove(); err != nil {
			r.FailedPath = r.Items[i].Path
			r.Error = fmt.Sprintf("prune %s after %d completed removal(s); failing path may be partially removed: %v", r.FailedPath, len(r.Removed), err)
			return errors.New(r.Error)
		}
		r.Removed = append(r.Removed, r.Items[i])
	}
	r.Applied = true
	return nil
}

func pruneBranchRefs(ws string) (map[string]bool, error) {
	// Unlike --verify --quiet, enumeration reports a corrupt loose ref as an
	// error rather than returning the same status as a missing ref.
	data, err := session.ReviewGit(ws, "show-ref", "--heads")
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("cannot establish branch absence: %w", err)
		}
	}
	refs := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/heads/") {
			return nil, errors.New("cannot establish branch absence: invalid ref listing")
		}
		refs[fields[1]] = true
	}
	return refs, nil
}

func pruneSnapshotExists(root *os.Root, id string) (bool, error) {
	if root == nil {
		return false, nil
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return false, errors.New("cannot establish snapshot absence: linked review directory")
		}
		if !entry.IsDir() {
			continue
		}
		dir, err := openPruneRoot(root, entry.Name())
		if err != nil {
			return false, err
		}
		data, err := pruneReadFile(dir, "snapshot.json", 64<<20)
		dir.Close()
		if err != nil {
			return false, err
		}
		var snapshot review.Snapshot
		if json.Unmarshal(data, &snapshot) != nil || snapshot.Version != 1 || snapshot.ID == "" || snapshot.ID != review.Identity(snapshot) {
			return false, errors.New("cannot establish snapshot absence: invalid saved snapshot")
		}
		if snapshot.ID == id {
			return true, nil
		}
	}
	return false, nil
}
