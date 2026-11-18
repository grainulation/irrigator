package branchreview

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/session"
)

// Selective application is a separate explicit mutation, never a review verdict
// or a merge. Its journal identifies both heads, selected paths, exact pre/post
// worktree and index state, and verification of the resulting candidate.
type appliedPath struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Link   bool   `json:"link,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Index  []byte `json:"index,omitempty"`
	Flags  []byte `json:"index_flags,omitempty"`
}

type applyVerification struct {
	Argv            []string `json:"argv"`
	CandidateSHA256 string   `json:"candidate_sha256"`
	StartedAt       string   `json:"started_at"`
	CompletedAt     string   `json:"completed_at,omitempty"`
	Status          string   `json:"status"`
	ExitCode        *int     `json:"exit_code,omitempty"`
	Error           string   `json:"error,omitempty"`
	StdoutSHA256    string   `json:"stdout_sha256,omitempty"`
	StderrSHA256    string   `json:"stderr_sha256,omitempty"`
}

type appliedChange struct {
	Version      int                `json:"version"`
	Digest       string             `json:"journal_sha256"`
	ID           string             `json:"id"`
	Workspace    string             `json:"workspace"`
	Branch       string             `json:"branch"`
	SourceHead   string             `json:"source_head"`
	TargetHead   string             `json:"target_head"`
	MergeBase    string             `json:"merge_base"`
	Paths        []string           `json:"paths"`
	PatchSHA256  string             `json:"patch_sha256"`
	Status       string             `json:"status"`
	CreatedAt    string             `json:"created_at"`
	UndoneAt     string             `json:"undone_at,omitempty"`
	Before       []appliedPath      `json:"before"`
	After        []appliedPath      `json:"after,omitempty"`
	Verification *applyVerification `json:"verification,omitempty"`
	Error        string             `json:"error,omitempty"`
}

const applyStateLimit = 64 << 20
const applyContentLimit = 12 << 20

func applyGit(ws string, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	argv := append([]string{"--no-pager", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null", "-c", "log.showSignature=false", "--literal-pathspecs"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir, cmd.Env, cmd.Stdin = ws, session.ReviewEnv(), bytes.NewReader(input)
	out := &session.ReviewLimitedBuffer{Limit: applyStateLimit}
	stderr := &session.ReviewLimitedBuffer{Limit: 8192}
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("review apply git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

func withApplyLock(ws string, fn func(string) error) error {
	dir, err := irrigatorPrivateDir(ws, "applied")
	if err != nil {
		return err
	}
	lock, err := launch.OpenContextFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another selective apply or undo is in progress")
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	return fn(dir)
}

func validApplyID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && strings.ToLower(id) == id
}

func saveApplied(dir string, state *appliedChange) error {
	if !validApplyID(state.ID) {
		return errors.New("invalid apply ID")
	}
	copy := *state
	copy.Digest = ""
	data, err := json.Marshal(copy)
	if err != nil || len(data) > applyStateLimit {
		return errors.New("selective apply snapshot exceeds 64 MiB")
	}
	state.Digest = reviewDigest(data)
	return jsonio.AtomicJSON(filepath.Join(dir, state.ID+".json"), state)
}

func readApplied(ws, id string) (*appliedChange, error) {
	if !validApplyID(id) {
		return nil, errors.New("invalid apply ID")
	}
	data, err := messaging.ReviewReadFile(filepath.Join(ws, ".irrigator", "applied", id+".json"), applyStateLimit)
	if err != nil {
		return nil, err
	}
	var state appliedChange
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("apply journal must contain exactly one JSON value")
	}
	copy := state
	copy.Digest = ""
	canonical, err := json.Marshal(copy)
	if err != nil || state.Digest == "" || state.Digest != reviewDigest(canonical) {
		return nil, errors.New("apply journal content digest mismatch")
	}
	if state.Version != 1 || state.ID != id || state.Workspace != ws || len(state.Paths) == 0 || len(state.Paths) > 256 || len(state.Before) != len(state.Paths) {
		return nil, errors.New("apply journal does not match this workspace or schema")
	}
	seen := map[string]bool{}
	for i, p := range state.Paths {
		if !reviewPath(p) || reviewExcluded(p) || seen[p] || state.Before[i].Path != p {
			return nil, errors.New("unsafe apply snapshot path")
		}
		seen[p] = true
		if err := validateAppliedPath(state.Before[i], len(state.TargetHead)); err != nil {
			return nil, err
		}
	}
	if len(state.After) != 0 {
		if len(state.After) != len(state.Paths) {
			return nil, errors.New("incomplete post-apply snapshot")
		}
		for i, p := range state.After {
			if p.Path != state.Paths[i] {
				return nil, errors.New("invalid post-apply path")
			}
			if err := validateAppliedPath(p, len(state.TargetHead)); err != nil {
				return nil, err
			}
		}
	}
	return &state, nil
}

func validateAppliedPath(state appliedPath, hashSize int) error {
	if (hashSize != 40 && hashSize != 64) || state.Mode > 0777 || len(state.Data) > reviewFileLimit || (!state.Exists && (state.Link || state.Mode != 0 || len(state.Data) != 0)) {
		return errors.New("invalid file content or mode in apply journal")
	}
	if len(state.Index) == 0 {
		if len(state.Flags) != 0 {
			return errors.New("index flags without an index entry")
		}
		return nil
	}
	if string(state.Flags) != "H "+state.Path+"\x00" {
		return errors.New("selected index snapshot has unsupported flags")
	}
	rows := strings.Split(string(state.Index), "\x00")
	if len(rows) != 2 || rows[1] != "" {
		return errors.New("invalid selected index snapshot")
	}
	meta, name, ok := strings.Cut(rows[0], "\t")
	fields := strings.Fields(meta)
	if !ok || name != state.Path || len(fields) != 3 || fields[2] != "0" || (fields[0] != "100644" && fields[0] != "100755" && fields[0] != "120000") {
		return errors.New("index snapshot does not belong to the selected file")
	}
	if raw, err := hex.DecodeString(fields[1]); err != nil || len(raw)*2 != hashSize {
		return errors.New("invalid index object in apply journal")
	}
	return nil
}

// Real directory ancestors are required. The final path may itself be a link:
// its target text is snapshotted, never dereferenced.
func applyAncestors(root *os.Root, p string, create bool) error {
	parts := strings.Split(p, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		info, err := root.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return nil
			}
			if err := root.Mkdir(parent, 0755); err != nil {
				return err
			}
			info, err = root.Lstat(parent)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("selected path has a non-directory or symlink ancestor: %s", p)
		}
	}
	return nil
}

func captureAppliedPaths(ws string, paths []string) ([]appliedPath, error) {
	root, err := os.OpenRoot(ws)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	states := make([]appliedPath, 0, len(paths))
	total := 0
	for _, p := range paths {
		if !reviewPath(p) || reviewExcluded(p) {
			return nil, fmt.Errorf("unsafe selected path: %s", p)
		}
		if err := applyAncestors(root, p, false); err != nil {
			return nil, err
		}
		state := appliedPath{Path: p}
		info, err := root.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			if info.IsDir() {
				return nil, fmt.Errorf("selected path is a directory, not file content: %s", p)
			}
			blob, err := reviewReadAt(root, p)
			if err != nil {
				return nil, err
			}
			if blob.Large {
				return nil, fmt.Errorf("selected file exceeds 2 MiB: %s", p)
			}
			state.Exists, state.Link, state.Mode, state.Data = true, blob.Link, uint32(info.Mode().Perm()), blob.Data
			total += len(blob.Data)
		}
		state.Index, err = applyGit(ws, nil, "ls-files", "--stage", "-z", "--", p)
		if err != nil {
			return nil, err
		}
		state.Flags, err = applyGit(ws, nil, "ls-files", "-v", "-z", "--", p)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	if total > applyContentLimit {
		return nil, errors.New("selected files exceed 12 MiB")
	}
	return states, nil
}

func sameAppliedPaths(a, b []appliedPath) bool {
	one, _ := json.Marshal(a)
	two, _ := json.Marshal(b)
	return bytes.Equal(one, two)
}

func selectedPaths(v *changeView, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, errors.New("apply requires at least one explicit --file")
	}
	changed := map[string]changeFile{}
	for _, f := range v.Files {
		changed[f.Path] = f
	}
	selected := map[string]bool{}
	for _, p := range requested {
		f, ok := changed[p]
		if !ok || !reviewPath(p) || reviewExcluded(p) {
			return nil, fmt.Errorf("not a selectable changed file: %s", p)
		}
		selected[p] = true
		if f.OldPath != "" { // A rename always selects both endpoints.
			if reviewExcluded(f.OldPath) {
				return nil, errors.New("rename touches a protected path")
			}
			selected[f.OldPath] = true
		}
	}
	if len(selected) > 256 {
		return nil, errors.New("select at most 256 paths")
	}
	paths := make([]string, 0, len(selected))
	for p := range selected {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

func applyCandidate(ws string) (string, error) {
	snapshot, err := captureReview(ws, "")
	if err != nil {
		return "", err
	}
	index, err := applyGit(ws, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Head      string       `json:"head"`
		Inventory []reviewBlob `json:"inventory"`
		Index     []byte       `json:"index"`
	}{snapshot.BaseCommit, snapshot.Inventory, index})
	if err != nil {
		return "", err
	}
	return reviewDigest(data), nil
}

func applySelectedChange(ws, branch, base, expectedHead string, requested, verify []string, timeout time.Duration) (*appliedChange, error) {
	if len(verify) == 0 || verify[0] == "" || len(verify) > 128 || timeout <= 0 || timeout > time.Hour {
		return nil, errors.New("apply requires --verify JSON argv and a timeout between 1ns and 1h")
	}
	for _, arg := range verify {
		if strings.ContainsRune(arg, 0) || len(arg) > 8192 {
			return nil, errors.New("invalid verification argument")
		}
	}
	if expectedHead == "" {
		return nil, errors.New("apply requires the reviewed --head commit from review change")
	}
	var state *appliedChange
	err := withApplyLock(ws, func(dir string) error {
		v, _, err := loadChange(ws, branch, base)
		if err != nil {
			return err
		}
		if v.Head != expectedHead {
			return errors.New("reviewed branch head changed; inspect the new diff before applying")
		}
		paths, err := selectedPaths(v, requested)
		if err != nil {
			return err
		}
		target, err := changeRev(ws, "HEAD")
		if err != nil {
			return err
		}
		before, err := captureAppliedPaths(ws, paths)
		if err != nil {
			return err
		}
		// Refuse dirty selected paths rather than overwrite or stash user work.
		// Unrelated staged, unstaged and untracked content stays in place.
		for _, p := range before {
			entry, err := applyGit(ws, nil, "ls-tree", "-z", target, "--", p.Path)
			if err != nil {
				return err
			}
			if len(entry) == 0 {
				if p.Exists || len(p.Index) != 0 {
					return fmt.Errorf("selected new path already has local content: %s", p.Path)
				}
				continue
			}
			meta := strings.Fields(strings.SplitN(string(entry), "\t", 2)[0])
			if len(meta) != 3 || meta[1] != "blob" || !p.Exists || !reviewGitBlobMatches(p.Data, meta[2]) {
				return fmt.Errorf("selected path differs from HEAD or is not file content: %s", p.Path)
			}
			mode := "100644"
			if p.Link {
				mode = "120000"
			} else if os.FileMode(p.Mode)&0100 != 0 {
				mode = "100755"
			}
			want := mode + " " + meta[2] + " 0\t" + p.Path + "\x00"
			if meta[0] != mode || string(p.Index) != want {
				return fmt.Errorf("selected path has local index or mode changes: %s", p.Path)
			}
			if string(p.Flags) != "H "+p.Path+"\x00" {
				return fmt.Errorf("selected path has assume-unchanged or skip-worktree flags: %s", p.Path)
			}
		}
		// Bound target snapshots before any mutation, including added binaries.
		targetBytes := 0
		for _, p := range paths {
			entry, err := applyGit(ws, nil, "ls-tree", "-z", v.Head, "--", p)
			if err != nil {
				return err
			}
			if len(entry) == 0 {
				continue
			}
			meta := strings.Fields(strings.SplitN(string(entry), "\t", 2)[0])
			if len(meta) != 3 || meta[1] != "blob" {
				return errors.New("submodules and directory replacements are not selectable file content")
			}
			raw, err := applyGit(ws, nil, "cat-file", "blob", meta[2])
			if err != nil || len(raw) > reviewFileLimit {
				return fmt.Errorf("target file unavailable or exceeds 2 MiB: %s", p)
			}
			targetBytes += len(raw)
			if targetBytes > applyContentLimit {
				return errors.New("selected target contents exceed 12 MiB")
			}
		}
		baseline, err := captureReview(ws, "")
		if err != nil {
			return err
		}
		selectedSet := map[string]bool{}
		for _, p := range paths {
			selectedSet[p] = true
		}
		for _, blob := range baseline.Inventory {
			if !selectedSet[blob.Path] {
				targetBytes += len(blob.Data)
			}
		}
		if targetBytes > reviewInventoryLimit {
			return errors.New("assembled candidate exceeds the 24 MiB review inventory limit")
		}
		args := append([]string{"diff", "--binary", "--full-index", "--no-renames", "--no-ext-diff", "--no-textconv", v.MergeBase, v.Head, "--"}, paths...)
		patch, err := applyGit(ws, nil, args...)
		if err != nil {
			return err
		}
		if len(patch) == 0 {
			return errors.New("selected diff is empty")
		}
		if _, err := applyGit(ws, patch, "apply", "--check", "--index", "--binary", "--whitespace=nowarn", "-"); err != nil {
			return fmt.Errorf("selected diff conflicts with target: %w", err)
		}
		nowSource, sourceErr := changeRev(ws, "refs/heads/"+v.Branch)
		nowTarget, targetErr := changeRev(ws, "HEAD")
		again, captureErr := captureAppliedPaths(ws, paths)
		if sourceErr != nil || targetErr != nil || captureErr != nil || nowSource != v.Head || nowTarget != target || !sameAppliedPaths(before, again) {
			return errors.New("source, target or selected files changed during preparation; retry after review")
		}
		token := make([]byte, 16)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		state = &appliedChange{Version: 1, ID: hex.EncodeToString(token), Workspace: ws, Branch: v.Branch, SourceHead: v.Head, TargetHead: target, MergeBase: v.MergeBase, Paths: paths, PatchSHA256: reviewDigest(patch), Before: before, Status: "applying", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if err := saveApplied(dir, state); err != nil {
			return err
		}
		if _, err := applyGit(ws, patch, "apply", "--index", "--binary", "--whitespace=nowarn", "-"); err != nil {
			state.Status, state.Error = "apply_uncertain", err.Error()
			_ = saveApplied(dir, state)
			return err
		}
		state.After, err = captureAppliedPaths(ws, paths)
		if err != nil {
			state.Status, state.Error = "apply_uncertain", err.Error()
			_ = saveApplied(dir, state)
			return err
		}
		state.Status = "applied_unverified"
		if err := saveApplied(dir, state); err != nil {
			return err
		}
		candidate, err := applyCandidate(ws)
		if err != nil {
			return err
		}
		state.Verification = &applyVerification{Argv: append([]string(nil), verify...), CandidateSHA256: candidate, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Status: "running"}
		if err := saveApplied(dir, state); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.Command(verify[0], verify[1:]...)
		cmd.Dir, cmd.Env = ws, session.ReviewEnv()
		prefix := filepath.Join(dir, state.ID+"-verify")
		_, verifyErr := launch.RunProcess(ctx, cmd, prefix, nil)
		if stdout, readErr := messaging.ReviewReadFile(prefix+".stdout.json", 4<<20); readErr == nil {
			state.Verification.StdoutSHA256 = reviewDigest(stdout)
		}
		if stderr, readErr := messaging.ReviewReadFile(prefix+".stderr.log", 1<<20); readErr == nil {
			state.Verification.StderrSHA256 = reviewDigest(stderr)
		}
		state.Verification.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		state.Verification.Status, state.Status = "failed", "verification_failed"
		if verifyErr != nil {
			state.Verification.Error = verifyErr.Error()
			var exit interface{ ExitCode() int }
			if errors.As(verifyErr, &exit) && exit.ExitCode() >= 0 {
				code := exit.ExitCode()
				state.Verification.ExitCode = &code
			}
			if errors.Is(verifyErr, context.DeadlineExceeded) {
				state.Verification.Status = "timed_out"
			}
		} else {
			zero := 0
			state.Verification.ExitCode = &zero
			state.Verification.Status, state.Status = "passed", "verified"
		}
		afterCandidate, candidateErr := applyCandidate(ws)
		afterSource, afterSourceErr := changeRev(ws, "refs/heads/"+v.Branch)
		if candidateErr != nil || afterSourceErr != nil || afterCandidate != candidate || afterSource != v.Head {
			state.Verification.Status, state.Status = "candidate_changed", "verification_failed"
			state.Verification.Error = "candidate or source branch changed while verification ran; previous green results do not apply"
		}
		if err := saveApplied(dir, state); err != nil {
			return err
		}
		if state.Status != "verified" {
			return fmt.Errorf("selected changes applied but verification %s; inspect journal %s and use explicit undo if desired", state.Verification.Status, state.ID)
		}
		return nil
	})
	return state, err
}

func restoreAppliedPaths(ws string, before []appliedPath, head string) error {
	root, err := localfs.Root(ws)
	if err != nil {
		return err
	}
	defer root.Close()
	var index bytes.Buffer
	for _, state := range before {
		if err := applyAncestors(root, state.Path, state.Exists); err != nil {
			return err
		}
		if err := root.Remove(state.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if state.Exists {
			if state.Link {
				if err := root.Symlink(string(state.Data), state.Path); err != nil {
					return err
				}
			} else {
				file, err := root.OpenFile(state.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(state.Mode))
				if err != nil {
					return err
				}
				_, writeErr := file.Write(state.Data)
				if writeErr == nil {
					writeErr = file.Chmod(os.FileMode(state.Mode))
				}
				if writeErr == nil {
					writeErr = file.Sync()
				}
				closeErr := file.Close()
				if writeErr != nil {
					return writeErr
				}
				if closeErr != nil {
					return closeErr
				}
			}
		}
		fmt.Fprintf(&index, "0 %s\t%s\x00", strings.Repeat("0", len(head)), state.Path)
		_, _ = index.Write(state.Index) // bytes.Buffer.Write cannot fail.
	}
	_, err = applyGit(ws, index.Bytes(), "update-index", "-z", "--index-info")
	return err
}

func undoSelectedChange(ws, id string) (*appliedChange, error) {
	var state *appliedChange
	err := withApplyLock(ws, func(dir string) error {
		var err error
		state, err = readApplied(ws, id)
		if err != nil {
			return err
		}
		if state.Status != "verified" && state.Status != "verification_failed" && state.Status != "applied_unverified" {
			return fmt.Errorf("cannot undo journal in state %s; interrupted mutations require inspection, never automatic replay", state.Status)
		}
		if state.Verification != nil {
			marker, err := messaging.ReviewReadFile(filepath.Join(dir, id+"-verify.process.json"), 65536)
			var process struct {
				Status string `json:"status"`
			}
			if err != nil || json.Unmarshal(marker, &process) != nil || process.Status != "stopped" {
				return errors.New("verification process cleanup is unresolved; inspect its process marker before any undo")
			}
		}
		head, err := changeRev(ws, "HEAD")
		if err != nil || head != state.TargetHead {
			return errors.New("target HEAD changed since apply; undo refused")
		}
		current, err := captureAppliedPaths(ws, state.Paths)
		if err != nil {
			return err
		}
		if !sameAppliedPaths(current, state.After) {
			return errors.New("selected file contents, modes or index changed since apply; undo refused")
		}
		state.Status = "undoing"
		if err := saveApplied(dir, state); err != nil {
			return err
		}
		if err := restoreAppliedPaths(ws, state.Before, head); err != nil {
			state.Status, state.Error = "undo_uncertain", err.Error()
			_ = saveApplied(dir, state)
			return err
		}
		state.Status, state.UndoneAt = "undone", time.Now().UTC().Format(time.RFC3339Nano)
		return saveApplied(dir, state)
	})
	return state, err
}
