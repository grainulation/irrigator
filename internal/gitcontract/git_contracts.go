package gitcontract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/session"
)

const gitContractSchema = "irrigator.git-contract/v1"
const gitContractBytes = 64 << 20

var gitContractID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,95}$`)
var gitContractOID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// Explicit local Git operations. These do not mutate a run checkpoint, replay
// providers, prune worktrees, or advance a developer's checked-out branch.
type gitContractRepo struct {
	repo, common string
	journal      *os.Root
}

func gitContractOpen(repo string) (*gitContractRepo, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	top, err := gitContractRun(abs, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if string(bytes.TrimSpace(top)) != abs {
		return nil, errors.New("repo must be its worktree root")
	}
	common, err := gitContractRun(abs, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	dir := string(bytes.TrimSpace(common))
	root, err := localfs.Root(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, part := range []string{"irrigator", "irrigator/git-contracts"} {
		if err = root.Mkdir(part, 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		st, e := root.Lstat(part)
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("Git contract directory must be a real directory")
		}
	}
	journal, err := root.OpenRoot("irrigator/git-contracts")
	if err != nil {
		return nil, err
	}
	return &gitContractRepo{abs, dir, journal}, nil
}
func (g *gitContractRepo) Close() { _ = g.journal.Close() }
func gitContractRun(repo string, env []string, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := append([]string{"--no-pager", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = repo
	cmd.Env = append(session.ReviewEnv(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_AUTHOR_NAME=Irrigator", "GIT_AUTHOR_EMAIL=irrigator@localhost", "GIT_COMMITTER_NAME=Irrigator", "GIT_COMMITTER_EMAIL=irrigator@localhost")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = bytes.NewReader(input)
	out := &session.ReviewLimitedBuffer{Limit: gitContractBytes}
	stderr := &session.ReviewLimitedBuffer{Limit: 8192}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}
func (g *gitContractRepo) git(input []byte, args ...string) (string, error) {
	out, err := gitContractRun(g.repo, nil, input, args...)
	return strings.TrimSpace(string(out)), err
}
func (g *gitContractRepo) oid(value, kind string) (string, error) {
	if !gitContractOID.MatchString(value) {
		return "", errors.New("expected full Git OID")
	}
	got, err := g.git(nil, "cat-file", "-t", value)
	if err != nil || got != kind {
		return "", fmt.Errorf("expected Git %s object", kind)
	}
	return value, nil
}
func gitContractDigest(v any) string {
	raw, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
func (g *gitContractRepo) lock() (func(), error) {
	f, err := localfs.OpenContextFile(filepath.Join(g.common, "irrigator/git-contracts/lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { checkpoint.Unlock(f) }, nil
}
func (g *gitContractRepo) record(id, suffix string, v any) error {
	if !gitContractID.MatchString(id) {
		return errors.New("invalid Git contract id")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("Git contract record too large")
	}
	// O_EXCL publishes immutable records. A partial/ambiguous write is retained
	// and subsequent reads fail closed; it is never overwritten during recovery.
	f, err := g.journal.OpenFile(id+"."+suffix+".json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(raw, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	d, err := g.journal.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (g *gitContractRepo) Read(id, suffix string, v any) error {
	if !gitContractID.MatchString(id) {
		return errors.New("invalid Git contract id")
	}
	f, err := g.journal.OpenFile(id+"."+suffix+".json", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("invalid Git contract record size")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("trailing Git contract data")
	}
	return nil
}
func (g *gitContractRepo) blob(raw []byte) (string, error) {
	return g.git(raw, "hash-object", "-w", "--stdin")
}
func (g *gitContractRepo) tree(entries map[string]string) (string, error) {
	names := []string{}
	for name := range entries {
		if name == "" || strings.ContainsAny(name, "/\x00") {
			return "", errors.New("invalid tree entry")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, name := range names {
		buf.WriteString(entries[name] + "\t" + name)
		buf.WriteByte(0)
	}
	return g.git(buf.Bytes(), "mktree", "-z")
}
func (g *gitContractRepo) commit(tree, base, message string) (string, error) {
	return g.git([]byte(message+"\n"), "commit-tree", tree, "-p", base)
}
func (g *gitContractRepo) createRef(ref, oid string) error {
	_, err := g.git(nil, "update-ref", "--no-deref", ref, oid, strings.Repeat("0", len(oid)))
	return err
}

// The capture API requires stopped writers. It rejects submodules and symlinks
// rather than pretending that a parent Git tree preserves nested repositories.
type gitContractCapture struct{ Tree, IndexBlob, IndexObjectsTree, InventoryDigest string }

func gitContractPath(name string) bool {
	if !utf8.ValidString(name) || !filepath.IsLocal(name) || name == "." || strings.Contains(name, "\\") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".git" || part == ".irrigator" {
			return false
		}
	}
	return true
}
func (g *gitContractRepo) capture(worktree, base string, salvage []string) (gitContractCapture, error) {
	var result gitContractCapture
	base, err := g.oid(base, "commit")
	if err != nil {
		return result, err
	}
	worktree, err = filepath.Abs(worktree)
	if err != nil {
		return result, err
	}
	worktree, err = filepath.EvalSymlinks(worktree)
	if err != nil {
		return result, err
	}
	common, err := gitContractRun(worktree, nil, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || strings.TrimSpace(string(common)) != g.common {
		return result, errors.New("worktree belongs to another repository")
	}
	top, err := gitContractRun(worktree, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil || strings.TrimSpace(string(top)) != worktree {
		return result, errors.New("capture requires exact worktree root")
	}
	shared, err := gitContractRun(worktree, nil, nil, "rev-parse", "--shared-index-path")
	if err != nil {
		return result, err
	}
	if len(bytes.TrimSpace(shared)) > 0 {
		return result, errors.New("split indexes unsupported; disable before capture")
	}
	idx, err := gitContractRun(worktree, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return result, err
	}
	indexFile, err := localfs.OpenContextFile(strings.TrimSpace(string(idx)), os.O_RDONLY, 0)
	if err != nil {
		return result, err
	}
	original, err := io.ReadAll(io.LimitReader(indexFile, (8<<20)+1))
	indexFile.Close()
	if err != nil || len(original) > 8<<20 {
		return result, errors.New("index capture exceeds bound")
	}
	result.IndexBlob, err = g.blob(original)
	if err != nil {
		return result, err
	}
	inventory := map[string]bool{}
	stagedEntries := map[string]string{}
	stages, err := gitContractRun(worktree, nil, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return result, err
	}
	for _, entry := range bytes.Split(stages, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		header, p, ok := bytes.Cut(entry, []byte{'\t'})
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 {
			return result, errors.New("malformed staged inventory")
		}
		if fields[0] == "160000" {
			return result, errors.New("submodule capture unsupported")
		}
		if fields[0] == "120000" {
			return result, errors.New("symlink capture unsupported")
		}
		if !gitContractPath(string(p)) {
			return result, errors.New("unsafe indexed path")
		}
		inventory[string(p)] = true
		oid, err := g.oid(fields[1], "blob")
		if err != nil {
			return result, err
		}
		stagedEntries[oid] = "100644 blob " + oid
	}
	result.IndexObjectsTree, err = g.tree(stagedEntries)
	if err != nil {
		return result, err
	}
	baseFiles, err := gitContractRun(worktree, nil, nil, "ls-tree", "-r", "-z", base)
	if err != nil {
		return result, err
	}
	for _, entry := range bytes.Split(baseFiles, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		header, p, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return result, errors.New("invalid base inventory")
		}
		if bytes.HasPrefix(header, []byte("160000 ")) || bytes.HasPrefix(header, []byte("120000 ")) {
			return result, errors.New("submodules/symlinks unsupported")
		}
		if !gitContractPath(string(p)) {
			return result, errors.New("unsafe base path")
		}
		inventory[string(p)] = true
	}
	others, err := gitContractRun(worktree, nil, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return result, err
	}
	for _, p := range bytes.Split(others, []byte{0}) {
		if len(p) > 0 {
			if !gitContractPath(string(p)) {
				return result, errors.New("unsafe untracked path")
			}
			inventory[string(p)] = true
		}
	}
	for _, p := range salvage {
		if !gitContractPath(p) {
			return result, errors.New("unsafe salvage path")
		}
		inventory[p] = true
	}
	if len(inventory) > 100000 {
		return result, errors.New("capture inventory exceeds limit")
	}
	paths := []string{}
	for p := range inventory {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	root, err := localfs.Root(worktree)
	if err != nil {
		return result, err
	}
	defer root.Close()
	temp, err := os.MkdirTemp("", "irrigator-git-index-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(temp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(temp, "index")}
	if _, err = gitContractRun(worktree, env, nil, "read-tree", "--empty"); err != nil {
		return result, err
	}
	var info bytes.Buffer
	total := 0
	for _, p := range paths {
		st, err := root.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return result, err
		}
		if !st.Mode().IsRegular() {
			return result, fmt.Errorf("capture rejects non-regular file: %s", p)
		}
		f, err := localfs.OpenContextFile(filepath.Join(worktree, p), os.O_RDONLY, 0)
		if err != nil {
			return result, err
		}
		raw, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
		after, e2 := f.Stat()
		f.Close()
		if err != nil || e2 != nil || len(raw) > 8<<20 || !os.SameFile(st, after) || st.Size() != after.Size() || !st.ModTime().Equal(after.ModTime()) {
			return result, errors.New("file changed or exceeds capture limit")
		}
		total += len(raw)
		if total > gitContractBytes {
			return result, errors.New("capture content exceeds limit")
		}
		oid, err := g.blob(raw)
		if err != nil {
			return result, err
		}
		mode := "100644"
		if st.Mode()&0100 != 0 {
			mode = "100755"
		}
		info.WriteString(mode + " " + oid + "\t" + p)
		info.WriteByte(0)
	}
	if _, err = gitContractRun(worktree, env, info.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return result, err
	}
	tree, err := gitContractRun(worktree, env, nil, "write-tree")
	if err != nil {
		return result, err
	}
	result.Tree = strings.TrimSpace(string(tree))
	result.InventoryDigest = gitContractDigest(paths)
	// Preserve staging distinctions without changing the real index; recheck that
	// it was not replaced during this stopped-writer capture.
	again, err := os.ReadFile(strings.TrimSpace(string(idx)))
	if err != nil || !bytes.Equal(original, again) {
		return result, errors.New("worker index changed during capture")
	}
	return result, nil
}

type gitQuarantineRecord struct {
	Schema           string             `json:"schema"`
	ID               string             `json:"id"`
	Base             string             `json:"base_commit_oid"`
	RecoveryCommit   string             `json:"recovery_commit_oid"`
	RecoveryRef      string             `json:"recovery_ref"`
	WorktreeRetained bool               `json:"worktree_retained"`
	Capture          gitContractCapture `json:"capture"`
}

func (g *gitContractRepo) quarantine(id, worktree, base string, salvage []string) (gitQuarantineRecord, error) {
	r := gitQuarantineRecord{Schema: gitContractSchema, ID: id, Base: base, RecoveryRef: "refs/irrigator/quarantine/" + id, WorktreeRetained: true}
	if !gitContractID.MatchString(id) {
		return r, errors.New("invalid quarantine id")
	}
	release, err := g.lock()
	if err != nil {
		return r, err
	}
	defer release()
	r.Capture, err = g.capture(worktree, base, salvage)
	if err != nil {
		return r, err
	}
	manifest, err := g.blob([]byte(gitContractDigest(r.Capture)))
	if err != nil {
		return r, err
	}
	tree, err := g.tree(map[string]string{"working-tree": "040000 tree " + r.Capture.Tree, "original-index": "100644 blob " + r.Capture.IndexBlob, "index-objects": "040000 tree " + r.Capture.IndexObjectsTree, "capture-digest": "100644 blob " + manifest})
	if err != nil {
		return r, err
	}
	r.RecoveryCommit, err = g.commit(tree, base, "irrigator: retained quarantine "+id)
	if err != nil {
		return r, err
	}
	if err = g.createRef(r.RecoveryRef, r.RecoveryCommit); err != nil {
		return r, err
	}
	if _, err = g.oid(r.RecoveryCommit, "commit"); err != nil {
		return r, err
	}
	return r, g.record(id, "quarantine", r)
}

// Verification is an explicit local command, not a model assertion. Records bind
// candidate, base, target and command digest; accepted identity cannot be changed.
type gitPreparedRecord struct {
	Schema       string `json:"schema"`
	ID           string `json:"id"`
	Candidate    string `json:"candidate_commit_oid"`
	Tree         string `json:"candidate_tree_oid"`
	Base         string `json:"base_commit_oid"`
	Target       string `json:"target_ref"`
	CheckDigest  string `json:"check_sha256"`
	OutputDigest string `json:"check_output_sha256"`
	Digest       string `json:"sha256"`
}
type gitAcceptanceRecord struct {
	Schema         string `json:"schema"`
	ID             string `json:"id"`
	PreparedDigest string `json:"prepared_sha256"`
}
type gitSettlementIntent struct {
	Schema         string `json:"schema"`
	ID             string `json:"id"`
	PreparedDigest string `json:"prepared_sha256"`
	Target         string `json:"target_ref"`
	Expected       string `json:"expected_commit_oid"`
	Candidate      string `json:"candidate_commit_oid"`
}
type gitSettlementReceipt struct {
	Schema       string `json:"schema"`
	ID           string `json:"id"`
	IntentDigest string `json:"intent_sha256"`
	State        string `json:"state"`
}

func gitContractPreparedDigest(p gitPreparedRecord) string {
	p.Digest = ""
	return gitContractDigest(p)
}
func (g *gitContractRepo) target(ref string) (string, error) {
	if !strings.HasPrefix(ref, "refs/irrigator/integration/") || strings.TrimPrefix(ref, "refs/irrigator/integration/") == "" {
		return "", errors.New("settlement only targets refs/irrigator/integration/*")
	}
	if _, err := g.git(nil, "check-ref-format", ref); err != nil {
		return "", err
	}
	if _, err := g.git(nil, "symbolic-ref", "-q", ref); err == nil {
		return "", errors.New("symbolic settlement targets are forbidden")
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return "", err
		}
	}
	worktrees, err := g.git(nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	for _, item := range strings.Split(worktrees, "\x00") {
		if item == "branch "+ref {
			return "", errors.New("checked-out settlement targets are forbidden")
		}
	}
	raw, err := g.git(nil, "show-ref", "--verify", "--hash", ref)
	if err != nil {
		return "", err
	}
	return g.oid(raw, "commit")
}
func (g *gitContractRepo) prepare(id, candidate, base, target string, check []string, timeout time.Duration) (gitPreparedRecord, error) {
	p := gitPreparedRecord{Schema: gitContractSchema, ID: id, Candidate: candidate, Base: base, Target: target}
	if !gitContractID.MatchString(id) || len(check) == 0 || timeout <= 0 || timeout > 5*time.Minute {
		return p, errors.New("prepare needs id, check argv and timeout within 5 minutes")
	}
	release, err := g.lock()
	if err != nil {
		return p, err
	}
	defer release()
	if _, err = g.oid(candidate, "commit"); err != nil {
		return p, err
	}
	if _, err = g.oid(base, "commit"); err != nil {
		return p, err
	}
	current, err := g.target(target)
	if err != nil || current != base {
		return p, errors.New("STALE_TARGET_BASE")
	}
	if _, err = g.git(nil, "merge-base", "--is-ancestor", base, candidate); err != nil {
		return p, errors.New("candidate must descend from expected target")
	}
	p.Tree, err = g.git(nil, "rev-parse", candidate+"^{tree}")
	if err != nil {
		return p, err
	}
	if _, err := g.git(nil, "config", "--local", "--get-regexp", "^filter\\."); err == nil {
		return p, errors.New("verification checkout refuses repository-local filters")
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return p, err
		}
	}
	temp, err := os.MkdirTemp("", "irrigator-verify-")
	if err != nil {
		return p, err
	}
	worktree := filepath.Join(temp, "tree")
	if _, err = g.git(nil, "worktree", "add", "--detach", worktree, candidate); err != nil {
		return p, err
	}
	// Never force-remove an uncertain or dirty verification workspace.
	clean := false
	defer func() {
		if clean {
			_, _ = g.git(nil, "worktree", "remove", worktree)
			_ = os.Remove(temp)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.Command(check[0], check[1:]...)
	cmd.Dir = worktree
	cmd.Env = []string{}
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "TEMP"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	output, err := launch.RunProcess(ctx, cmd, filepath.Join(temp, "check"), nil)
	if err != nil {
		return p, fmt.Errorf("verification failed; worktree retained at %s: %w", worktree, err)
	}
	captured, err := g.capture(worktree, candidate, nil)
	if err != nil {
		return p, fmt.Errorf("verification capture failed; worktree retained at %s: %w", worktree, err)
	}
	if captured.Tree != p.Tree {
		return p, fmt.Errorf("verification changed candidate; worktree retained at %s", worktree)
	}
	p.CheckDigest = gitContractDigest(check)
	p.OutputDigest = fmt.Sprintf("%x", sha256.Sum256(output))
	p.Digest = gitContractPreparedDigest(p)
	if err = g.createRef("refs/irrigator/candidates/"+id, candidate); err != nil {
		return p, err
	}
	if err = g.record(id, "prepare", p); err != nil {
		return p, err
	}
	clean = true
	return p, nil
}
func (g *gitContractRepo) prepared(id string) (gitPreparedRecord, error) {
	var p gitPreparedRecord
	if err := g.Read(id, "prepare", &p); err != nil {
		return p, err
	}
	if p.Schema != gitContractSchema || p.ID != id || p.Digest == "" || p.Digest != gitContractPreparedDigest(p) {
		return p, errors.New("invalid prepared identity")
	}
	tree, err := g.git(nil, "rev-parse", p.Candidate+"^{tree}")
	if err != nil || tree != p.Tree {
		return p, errors.New("candidate identity changed")
	}
	ref, err := g.git(nil, "show-ref", "--verify", "--hash", "refs/irrigator/candidates/"+id)
	if err != nil || ref != p.Candidate {
		return p, errors.New("candidate ref changed")
	}
	return p, nil
}
func (g *gitContractRepo) accept(id string) (gitAcceptanceRecord, error) {
	r := gitAcceptanceRecord{Schema: gitContractSchema, ID: id}
	release, err := g.lock()
	if err != nil {
		return r, err
	}
	defer release()
	p, err := g.prepared(id)
	if err != nil {
		return r, err
	}
	r.PreparedDigest = p.Digest
	var existing gitAcceptanceRecord
	if err = g.Read(id, "accept", &existing); err == nil {
		if existing != r {
			return r, errors.New("acceptance identity mismatch")
		}
		return r, nil
	} else if !os.IsNotExist(err) {
		return r, err
	}
	return r, g.record(id, "accept", r)
}
func (g *gitContractRepo) settle(id string, reconcile bool) (gitSettlementReceipt, error) {
	r := gitSettlementReceipt{Schema: gitContractSchema, ID: id}
	release, err := g.lock()
	if err != nil {
		return r, err
	}
	defer release()
	p, err := g.prepared(id)
	if err != nil {
		return r, err
	}
	var a gitAcceptanceRecord
	if err = g.Read(id, "accept", &a); err != nil || a.Schema != gitContractSchema || a.ID != id || a.PreparedDigest != p.Digest {
		return r, errors.New("ACCEPTANCE_REQUIRED")
	}
	expected := gitSettlementIntent{Schema: gitContractSchema, ID: id, PreparedDigest: p.Digest, Target: p.Target, Expected: p.Base, Candidate: p.Candidate}
	var intent gitSettlementIntent
	err = g.Read(id, "intent", &intent)
	if os.IsNotExist(err) {
		if reconcile {
			return r, errors.New("NO_MATCHING_DURABLE_INTENT")
		}
		intent = expected
		if err = g.record(id, "intent", intent); err != nil {
			return r, err
		}
	} else if err != nil {
		return r, err
	}
	if intent != expected {
		return r, errors.New("settlement intent mismatch")
	}
	r.IntentDigest = gitContractDigest(intent)
	current, err := g.target(intent.Target)
	if err != nil {
		return r, err
	}
	switch current {
	case intent.Candidate:
		r.State = "settled"
	case intent.Expected:
		if reconcile {
			return r, errors.New("INTENT_PENDING: explicit settle required to advance ref")
		}
		if _, err = g.git(nil, "update-ref", "--no-deref", intent.Target, intent.Candidate, intent.Expected); err != nil {
			return r, err
		}
		r.State = "settled"
	default:
		return r, errors.New("STALE_TARGET_BASE")
	}
	var old gitSettlementReceipt
	if err = g.Read(id, "settled", &old); err == nil {
		if old != r {
			return r, errors.New("settlement receipt mismatch")
		}
		return r, nil
	} else if !os.IsNotExist(err) {
		return r, err
	}
	return r, g.record(id, "settled", r)
}

func GitContractsCLI(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errors.New("usage: irrigator contracts git quarantine|prepare|accept|settle|reconcile")
	}
	sub := args[0]
	fs := flag.NewFlagSet("git "+sub, flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository root")
	id := fs.String("id", "", "record identity")
	worktree := fs.String("worktree", "", "worktree to preserve")
	base := fs.String("base", "", "full base commit OID")
	candidate := fs.String("candidate", "", "full candidate commit OID")
	target := fs.String("target", "", "existing refs/irrigator/integration/* reference")
	stopped := fs.Bool("confirm-stopped", false, "attest all worktree writers stopped")
	timeout := fs.Duration("timeout", time.Minute, "verification command timeout (at most 5m)")
	var salvage gitContractPaths
	fs.Var(&salvage, "salvage", "explicit ignored file path to preserve (repeatable)")
	if err := commandflags.ParseCommandFlags(fs, args[1:]); err != nil {
		return 2, err
	}
	if !gitContractID.MatchString(*id) {
		return 2, errors.New("a simple --id is required")
	}
	if sub != "prepare" && fs.NArg() != 0 {
		return 2, errors.New("unexpected positional arguments")
	}
	if sub == "quarantine" && (!*stopped || *worktree == "" || *base == "") {
		return 2, errors.New("quarantine requires --worktree --base --confirm-stopped")
	}
	g, err := gitContractOpen(*repo)
	if err != nil {
		return 1, err
	}
	defer g.Close()
	var result any
	switch sub {
	case "quarantine":
		result, err = g.quarantine(*id, *worktree, *base, salvage)
	case "prepare":
		result, err = g.prepare(*id, *candidate, *base, *target, fs.Args(), *timeout)
	case "accept":
		result, err = g.accept(*id)
	case "settle":
		result, err = g.settle(*id, false)
	case "reconcile":
		result, err = g.settle(*id, true)
	default:
		return 2, errors.New("unknown Git contract command")
	}
	if err != nil {
		return 1, err
	}
	return 0, jsonio.PrintJSON(result)
}

type gitContractPaths []string

func (p *gitContractPaths) String() string         { return strings.Join(*p, ",") }
func (p *gitContractPaths) Set(value string) error { *p = append(*p, value); return nil }
