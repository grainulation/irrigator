package branchreview

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/session"
	"github.com/grainulation/irrigator/internal/textsafe"
)

const changeStateVersion = 1
const changeStateLimit = 4 << 20

type changeComment struct {
	ID        string `json:"id"`
	Commit    string `json:"commit"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Context   string `json:"context,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// changeVerdict binds a human decision to the exact head it was made on.
type changeVerdict struct {
	State string `json:"state"`
	Head  string `json:"head"`
	Body  string `json:"body,omitempty"`
	At    string `json:"at"`
}

type changeState struct {
	Version  int               `json:"version"`
	Subject  string            `json:"subject"`
	Viewed   map[string]string `json:"viewed"`
	Comments []changeComment   `json:"comments"`
	Verdicts []changeVerdict   `json:"verdicts"`
}

type changeCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

type changeFile struct {
	Path       string `json:"path"`
	OldPath    string `json:"old_path,omitempty"`
	Status     string `json:"status"`
	Additions  int    `json:"additions"`
	Deletions  int    `json:"deletions"`
	Binary     bool   `json:"binary,omitempty"`
	Risk       string `json:"risk"`
	RiskReason string `json:"risk_reason,omitempty"`
	Viewed     bool   `json:"viewed"`
	Object     string `json:"object,omitempty"`
	// Symbols are the enclosing functions or sections of changed hunks, from
	// git's hunk headers: a deterministic "what changed" layer.
	Symbols []string `json:"symbols,omitempty"`
	Hunks   int      `json:"hunks"`
}

type changeCommentView struct {
	changeComment
	Outdated bool `json:"outdated,omitempty"`
}

type changeVerdictView struct {
	changeVerdict
	Current bool `json:"current"`
}

type changeView struct {
	Workspace string              `json:"workspace"`
	Branch    string              `json:"branch"`
	Base      string              `json:"base"`
	MergeBase string              `json:"merge_base"`
	Head      string              `json:"head"`
	Tree      string              `json:"tree"`
	Commits   []changeCommit      `json:"commits"`
	Files     []changeFile        `json:"files"`
	Comments  []changeCommentView `json:"comments"`
	Verdict   *changeVerdictView  `json:"verdict,omitempty"`
	Awaiting  bool                `json:"awaiting_agent"`
	Stats     map[string]int      `json:"stats"`
}

func changeRev(ws, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") || !launch.ContextText(rev, 256) {
		return "", errors.New("invalid revision name")
	}
	out, err := session.ReviewGit(ws, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("unknown revision %s", rev)
	}
	return strings.TrimSpace(string(out)), nil
}

// changeBase picks the comparison base: an explicit --base, the remote's
// default branch, or a local main/master.
func changeBase(ws, override string) (string, error) {
	if override != "" {
		if _, err := changeRev(ws, override); err != nil {
			return "", err
		}
		return override, nil
	}
	if out, err := session.ReviewGit(ws, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if _, err := changeRev(ws, name); err == nil {
			return name, nil
		}
	}
	return "", errors.New("no base branch found; pass --base")
}

func changeBranch(ws, name string) (string, error) {
	if name == "" {
		out, err := session.ReviewGit(ws, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return "", errors.New("HEAD is detached; name a branch")
		}
		name = strings.TrimSpace(string(out))
	}
	if strings.HasPrefix(name, "-") || !launch.ContextText(name, 256) {
		return "", errors.New("invalid branch name")
	}
	if _, err := session.ReviewGit(ws, "show-ref", "--verify", "--quiet", "refs/heads/"+name); err != nil {
		return "", fmt.Errorf("no local branch %s", name)
	}
	return name, nil
}

// changeRisk applies a few built-in rules so risky files are read first and
// mechanical ones are collapsed. It orders attention; it approves nothing.
func changeRisk(p string) (string, string) {
	base := strings.ToLower(path.Base(p))
	lower := strings.ToLower(p)
	switch {
	case base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.Contains(lower, "secret") || strings.Contains(lower, "credential"):
		return "high", "secrets"
	case strings.Contains(lower, "auth") || strings.Contains(lower, "login") || strings.Contains(lower, "password") || strings.Contains(lower, "token"):
		return "high", "authentication"
	case strings.Contains(lower, "migration") || strings.HasSuffix(base, ".sql"):
		return "high", "migration"
	case strings.HasPrefix(lower, ".github/") || base == "makefile" || base == "dockerfile" || base == "irrigator.checks.json":
		return "high", "build or CI"
	}
	switch base {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "cargo.lock", "poetry.lock", "gemfile.lock", "composer.lock", "uv.lock":
		return "collapsed", "lockfile"
	case "go.mod", "package.json", "cargo.toml", "pyproject.toml", "gemfile", "requirements.txt":
		return "high", "dependencies"
	}
	if strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".pb.go") || strings.Contains(base, "_generated.") || strings.Contains(base, ".generated.") || strings.HasPrefix(lower, "vendor/") || strings.Contains(lower, "/vendor/") {
		return "collapsed", "generated"
	}
	return "normal", ""
}

func changeCommits(ws, from, to string) ([]changeCommit, error) {
	out, err := session.ReviewGit(ws, "log", "--reverse", "--format=%H%x00%s%x00%an%x00%cI%x1e", from+".."+to)
	if err != nil {
		return nil, err
	}
	commits := []changeCommit{}
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x00", 4)
		if len(f) != 4 {
			return nil, errors.New("unexpected git log output")
		}
		commits = append(commits, changeCommit{SHA: f[0], Subject: session.ReviewTerminalText(f[1]), Author: session.ReviewTerminalText(f[2]), Date: f[3]})
	}
	return commits, nil
}

// changeFiles lists files changed between two commits with line counts.
func changeFiles(ws, from, to string) ([]changeFile, error) {
	revs := []string{from}
	if to != "" {
		revs = append(revs, to) // empty: compare with the working tree
	}
	diffArgs := func(mode string) []string {
		return append(append([]string{"diff", "--no-ext-diff", "--no-textconv", "-M", mode, "-z"}, revs...), "--")
	}
	names, err := session.ReviewGit(ws, diffArgs("--name-status")...)
	if err != nil {
		return nil, err
	}
	files := []changeFile{}
	fields := strings.Split(string(names), "\x00")
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			i++
			continue
		}
		f := changeFile{Status: map[byte]string{'A': "added", 'M': "modified", 'D': "deleted", 'R': "renamed", 'T': "modified", 'C': "added"}[status[0]]}
		if f.Status == "" {
			f.Status = "modified"
		}
		if status[0] == 'R' || status[0] == 'C' {
			if i+2 >= len(fields) {
				return nil, errors.New("unexpected git name-status output")
			}
			f.OldPath, f.Path = fields[i+1], fields[i+2]
			i += 3
		} else {
			if i+1 >= len(fields) {
				return nil, errors.New("unexpected git name-status output")
			}
			f.Path = fields[i+1]
			i += 2
		}
		if !reviewPath(f.Path) || (f.OldPath != "" && !reviewPath(f.OldPath)) {
			return nil, errors.New("invalid path in diff")
		}
		f.Risk, f.RiskReason = changeRisk(f.Path)
		files = append(files, f)
	}
	counts, err := session.ReviewGit(ws, diffArgs("--numstat")...)
	if err != nil {
		return nil, err
	}
	byPath := map[string]*changeFile{}
	for i := range files {
		byPath[files[i].Path] = &files[i]
	}
	parts := strings.Split(string(counts), "\x00")
	for i := 0; i < len(parts); i++ {
		rec := parts[i]
		if rec == "" {
			continue
		}
		cols := strings.SplitN(rec, "\t", 3)
		if len(cols) != 3 {
			continue
		}
		target := cols[2]
		if target == "" && i+2 < len(parts) { // rename: counts, then old and new paths
			target = parts[i+2]
			i += 2
		}
		if f := byPath[target]; f != nil {
			if cols[0] == "-" {
				f.Binary = true
				continue
			}
			f.Additions, _ = strconv.Atoi(cols[0])
			f.Deletions, _ = strconv.Atoi(cols[1])
		}
	}
	return files, nil
}

func changeTree(ws, commit string) (map[string]string, error) {
	out, err := session.ReviewGit(ws, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	objects := map[string]string{}
	for _, entry := range strings.Split(string(out), "\x00") {
		meta, p, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		if fields := strings.Fields(meta); len(fields) == 3 {
			objects[p] = fields[2]
		}
	}
	return objects, nil
}

func changeStateID(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:8])
}

func changeStatePath(ws, subject string) (string, error) {
	dir, err := irrigatorPrivateDir(ws, "changes")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, changeStateID(subject)+".json"), nil
}

func readChangeState(ws, subject string) (*changeState, error) {
	s := &changeState{Version: changeStateVersion, Subject: subject, Viewed: map[string]string{}, Comments: []changeComment{}, Verdicts: []changeVerdict{}}
	p := filepath.Join(ws, ".irrigator", "changes", changeStateID(subject)+".json")
	data, err := messaging.ReviewReadFile(p, changeStateLimit)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("review state is unreadable: %w", err)
	}
	if s.Version != changeStateVersion || s.Subject != subject {
		return nil, errors.New("review state does not belong to this subject")
	}
	if s.Viewed == nil {
		s.Viewed = map[string]string{}
	}
	return s, nil
}

// updateChangeState applies fn under an exclusive lock and replaces the file atomically.
func updateChangeState(ws, subject string, fn func(*changeState) error) error {
	p, err := changeStatePath(ws, subject)
	if err != nil {
		return err
	}
	lock, err := launch.OpenContextFile(strings.TrimSuffix(p, ".json")+".lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func(fd int) { _ = syscall.Flock(fd, syscall.LOCK_UN) }(int(lock.Fd()))
	s, err := readChangeState(ws, subject)
	if err != nil {
		return err
	}
	if err = fn(s); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > changeStateLimit {
		return errors.New("review state exceeds limit")
	}
	root, err := localfs.Root(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer root.Close()
	token := make([]byte, 8)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	tmp := ".state-" + hex.EncodeToString(token)
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return root.Rename(tmp, filepath.Base(p))
}

func latestVerdict(s *changeState, head string) *changeVerdictView {
	if len(s.Verdicts) == 0 {
		return nil
	}
	v := s.Verdicts[len(s.Verdicts)-1]
	return &changeVerdictView{changeVerdict: v, Current: v.Head == head}
}

func loadChange(ws, branch, baseName string) (*changeView, *changeState, error) {
	base, err := changeBase(ws, baseName)
	if err != nil {
		return nil, nil, err
	}
	branch, err = changeBranch(ws, branch)
	if err != nil {
		return nil, nil, err
	}
	head, err := changeRev(ws, "refs/heads/"+branch)
	if err != nil {
		return nil, nil, err
	}
	mb, err := session.ReviewGit(ws, "merge-base", base, head)
	if err != nil {
		return nil, nil, fmt.Errorf("%s shares no history with %s", branch, base)
	}
	mergeBase := strings.TrimSpace(string(mb))
	tree, err := session.ReviewGit(ws, "rev-parse", head+"^{tree}")
	if err != nil {
		return nil, nil, err
	}
	v := &changeView{Workspace: ws, Branch: branch, Base: base, MergeBase: mergeBase, Head: head, Tree: strings.TrimSpace(string(tree))}
	if v.Commits, err = changeCommits(ws, mergeBase, head); err != nil {
		return nil, nil, err
	}
	if v.Files, err = changeFiles(ws, mergeBase, head); err != nil {
		return nil, nil, err
	}
	addSymbols(ws, v.Files, mergeBase, head)
	objects, err := changeTree(ws, head)
	if err != nil {
		return nil, nil, err
	}
	s, err := readChangeState(ws, branch)
	if err != nil {
		return nil, nil, err
	}
	stats := map[string]int{"files": len(v.Files), "commits": len(v.Commits)}
	for i := range v.Files {
		f := &v.Files[i]
		f.Object = objects[f.Path]
		mark := f.Object
		if f.Status == "deleted" {
			mark = "deleted"
		}
		// Viewed is keyed by content: it stays only while the file is unchanged.
		f.Viewed = s.Viewed[f.Path] != "" && s.Viewed[f.Path] == mark
		stats["additions"] += f.Additions
		stats["deletions"] += f.Deletions
		if f.Viewed {
			stats["viewed"]++
		}
		if f.Risk == "high" {
			stats["high_risk"]++
		}
	}
	rank := map[string]int{"high": 0, "normal": 1, "collapsed": 2}
	sort.SliceStable(v.Files, func(i, j int) bool {
		if rank[v.Files[i].Risk] != rank[v.Files[j].Risk] {
			return rank[v.Files[i].Risk] < rank[v.Files[j].Risk]
		}
		return v.Files[i].Path < v.Files[j].Path
	})
	v.Comments = []changeCommentView{}
	for _, c := range s.Comments {
		view := changeCommentView{changeComment: c}
		if c.Commit != head && c.Line > 0 {
			view.Outdated = changeLine(ws, head, c.Path, c.Line) != c.Context
		}
		v.Comments = append(v.Comments, view)
	}
	stats["comments"] = len(v.Comments)
	v.Verdict = latestVerdict(s, head)
	v.Awaiting = v.Verdict != nil && v.Verdict.State == "changes_requested" && v.Verdict.Current
	v.Stats = stats
	return v, s, nil
}

// changeLine returns one line of a file at a commit, or "" when unavailable.
func changeLine(ws, commit, p string, line int) string {
	if line <= 0 || !reviewPath(p) {
		return ""
	}
	size, err := session.ReviewGit(ws, "cat-file", "-s", commit+":"+p)
	if err != nil {
		return ""
	}
	if n, err := strconv.Atoi(strings.TrimSpace(string(size))); err != nil || n > reviewFileLimit {
		return ""
	}
	data, err := session.ReviewGit(ws, "cat-file", "blob", commit+":"+p)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if line > len(lines) {
		return ""
	}
	return lines[line-1]
}

type changeSummary struct {
	Branch   string             `json:"branch"`
	Head     string             `json:"head"`
	Ahead    int                `json:"ahead"`
	Files    int                `json:"files"`
	Latest   string             `json:"latest"`
	Updated  string             `json:"updated"`
	Verdict  *changeVerdictView `json:"verdict,omitempty"`
	Awaiting bool               `json:"awaiting_agent"`
	Current  bool               `json:"current"`
}

type documentSummary struct {
	Kind     string             `json:"kind"`
	Title    string             `json:"title"`
	Snapshot string             `json:"snapshot"`
	Path     string             `json:"path"`
	Comments int                `json:"comments"`
	Verdict  *changeVerdictView `json:"verdict,omitempty"`
}

type reviewQueue struct {
	Workspace   string            `json:"workspace"`
	Base        string            `json:"base"`
	Current     string            `json:"current_branch,omitempty"`
	Uncommitted int               `json:"uncommitted"`
	Changes     []changeSummary   `json:"changes"`
	Documents   []documentSummary `json:"documents"`
	Overlaps    [][]string        `json:"overlaps"`
	// Messages are cross-swarm channel items waiting for approval (experiments.messaging).
	Messages []pendingMessage `json:"messages"`
}

type pendingMessage struct {
	Campaign string `json:"campaign"`
	Channel  string `json:"channel"`
	ID       string `json:"id"`
	From     string `json:"from"`
	Text     string `json:"text"`
}

// listReviews builds the queue: every local branch ahead of the base, plus
// plan/direction/decision snapshots, plus files touched by more than one change.
func ListReviews(ws, baseName string) (*reviewQueue, error) {
	base, err := changeBase(ws, baseName)
	if err != nil {
		return nil, err
	}
	q := &reviewQueue{Workspace: ws, Base: base, Changes: []changeSummary{}, Documents: []documentSummary{}, Overlaps: [][]string{}, Messages: pendingMessages(ws)}
	if cur, err := session.ReviewGit(ws, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		q.Current = strings.TrimSpace(string(cur))
	}
	if status, err := session.ReviewGit(ws, "status", "--porcelain=v1", "-z", "--untracked-files=all"); err == nil {
		for _, e := range strings.Split(string(status), "\x00") {
			if len(e) > 3 && !reviewExcluded(e[3:]) {
				q.Uncommitted++
			}
		}
	}
	baseHead, _ := changeRev(ws, base)
	refs, err := session.ReviewGit(ws, "for-each-ref", "--format=%(refname:short)%00%(objectname)%00%(committerdate:iso-strict)%00%(subject)", "refs/heads")
	if err != nil {
		return nil, err
	}
	touched := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(refs)), "\n") {
		f := strings.SplitN(line, "\x00", 4)
		if len(f) != 4 || f[1] == baseHead {
			continue
		}
		count, err := session.ReviewGit(ws, "rev-list", "--count", base+".."+f[1])
		if err != nil {
			continue
		}
		ahead, _ := strconv.Atoi(strings.TrimSpace(string(count)))
		if ahead == 0 {
			continue
		}
		mb, err := session.ReviewGit(ws, "merge-base", base, f[1])
		if err != nil {
			continue
		}
		names, err := session.ReviewGit(ws, "diff", "--name-only", "-z", strings.TrimSpace(string(mb)), f[1], "--")
		if err != nil {
			continue
		}
		files := 0
		for _, n := range strings.Split(string(names), "\x00") {
			if n != "" {
				files++
				touched[n] = append(touched[n], f[0])
			}
		}
		s, err := readChangeState(ws, f[0])
		if err != nil {
			return nil, err
		}
		v := latestVerdict(s, f[1])
		q.Changes = append(q.Changes, changeSummary{Branch: f[0], Head: f[1], Ahead: ahead, Files: files, Latest: session.ReviewTerminalText(f[3]), Updated: f[2], Verdict: v,
			Awaiting: v != nil && v.State == "changes_requested" && v.Current, Current: f[0] == q.Current})
	}
	sort.Slice(q.Changes, func(i, j int) bool { return q.Changes[i].Updated > q.Changes[j].Updated })
	for p, branches := range touched {
		if len(branches) > 1 {
			q.Overlaps = append(q.Overlaps, append([]string{p}, branches...))
		}
	}
	sort.Slice(q.Overlaps, func(i, j int) bool { return q.Overlaps[i][0] < q.Overlaps[j][0] })
	entries, _ := os.ReadDir(filepath.Join(ws, ".irrigator", "reviews"))
	for _, e := range entries {
		dir := filepath.Join(ws, ".irrigator", "reviews", e.Name())
		snap, err := readReview(dir)
		if err != nil || reviewKind(snap) == "code" {
			continue
		}
		comments, _ := reviewComments(dir)
		s, err := readChangeState(ws, "snapshot:"+snap.ID)
		if err != nil {
			return nil, err
		}
		q.Documents = append(q.Documents, documentSummary{Kind: snap.Kind, Title: snap.Title, Snapshot: snap.ID, Path: dir, Comments: len(comments), Verdict: latestVerdict(s, snap.ID)})
	}
	return q, nil
}

func markViewed(ws, branch, baseName string, files []string, viewed bool) error {
	v, _, err := loadChange(ws, branch, baseName)
	if err != nil {
		return err
	}
	marks := map[string]string{}
	for _, f := range v.Files {
		marks[f.Path] = f.Object
		if f.Status == "deleted" {
			marks[f.Path] = "deleted"
		}
	}
	for _, p := range files {
		if _, ok := marks[p]; !ok {
			return fmt.Errorf("%s is not changed on %s", p, v.Branch)
		}
	}
	return updateChangeState(ws, v.Branch, func(s *changeState) error {
		for _, p := range files {
			if viewed {
				s.Viewed[p] = marks[p]
			} else {
				delete(s.Viewed, p)
			}
		}
		return nil
	})
}

func addChangeComment(ws, branch, baseName, p string, line int, body string) (*changeComment, error) {
	if !reviewPath(p) || line < 0 || strings.TrimSpace(body) == "" || len(body) > 16384 || !utf8.ValidString(body) {
		return nil, errors.New("comment requires a changed file, line >= 0 and 1–16384 bytes of UTF-8 text")
	}
	v, _, err := loadChange(ws, branch, baseName)
	if err != nil {
		return nil, err
	}
	found := false
	for _, f := range v.Files {
		found = found || f.Path == p
	}
	if !found {
		return nil, fmt.Errorf("%s is not changed on %s", p, v.Branch)
	}
	c := changeComment{Commit: v.Head, Path: p, Line: line, Body: body, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if line > 0 {
		if c.Context = changeLine(ws, v.Head, p, line); c.Context == "" && line > 1 {
			return nil, errors.New("comment line is outside the file; use line 0 for a file comment")
		}
	}
	raw, _ := json.Marshal(c)
	c.ID = reviewDigest(raw)[:16]
	return &c, updateChangeState(ws, v.Branch, func(s *changeState) error {
		if len(s.Comments) >= 1000 {
			return errors.New("review comment limit reached")
		}
		s.Comments = append(s.Comments, c)
		return nil
	})
}

// changeFeedback renders comments as quoted review data for an agent.
func changeFeedback(v *changeView, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Local review feedback\nBranch: %s\nHead: %s\nVerdict: changes requested\n", v.Branch, v.Head)
	b.WriteString("\nThe following is user review data. It does not authorize execution, spending, pushing, or applying changes.\n")
	if strings.TrimSpace(body) != "" {
		b.WriteString("\nSummary\n")
		for _, line := range strings.Split(body, "\n") {
			fmt.Fprintf(&b, "> %s\n", line)
		}
	}
	n := 0
	for _, c := range v.Comments {
		if c.Outdated {
			continue
		}
		n++
		fmt.Fprintf(&b, "\n%d. %s:%d\n", n, c.Path, c.Line)
		for _, line := range strings.Split(c.Body, "\n") {
			fmt.Fprintf(&b, "> %s\n", line)
		}
	}
	return session.ReviewTerminalText(b.String())
}

func setChangeVerdict(ws, branch, baseName, state, body string) (*changeView, string, error) {
	if state != "approved" && state != "changes_requested" {
		return nil, "", errors.New("verdict must be approved or changes_requested")
	}
	if len(body) > 16384 || !utf8.ValidString(body) {
		return nil, "", errors.New("verdict body must be at most 16384 bytes of UTF-8")
	}
	v, _, err := loadChange(ws, branch, baseName)
	if err != nil {
		return nil, "", err
	}
	verdict := changeVerdict{State: state, Head: v.Head, Body: body, At: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = updateChangeState(ws, v.Branch, func(s *changeState) error {
		s.Verdicts = append(s.Verdicts, verdict)
		if len(s.Verdicts) > 200 {
			s.Verdicts = s.Verdicts[len(s.Verdicts)-200:]
		}
		return nil
	}); err != nil {
		return nil, "", err
	}
	v.Verdict = &changeVerdictView{changeVerdict: verdict, Current: true}
	v.Awaiting = state == "changes_requested"
	if state == "approved" {
		return v, "", nil
	}
	return v, changeFeedback(v, body), nil
}

// setDocumentVerdict records a verdict on a plan, direction or decision snapshot.
func setDocumentVerdict(ws, dir, state, body string) (*reviewSnapshot, error) {
	if state != "approved" && state != "changes_requested" {
		return nil, errors.New("verdict must be approved or changes_requested")
	}
	snap, err := readReview(dir)
	if err != nil {
		return nil, err
	}
	if reviewKind(snap) == "code" {
		return nil, errors.New("code snapshots are reviewed as changes; name a branch instead")
	}
	verdict := changeVerdict{State: state, Head: snap.ID, Body: body, At: time.Now().UTC().Format(time.RFC3339Nano)}
	return snap, updateChangeState(ws, "snapshot:"+snap.ID, func(s *changeState) error {
		s.Verdicts = append(s.Verdicts, verdict)
		return nil
	})
}

// loadUncommitted describes working-tree edits against HEAD without saving a
// snapshot, so looking at uncommitted work leaves no files behind.
func loadUncommitted(ws string) (*changeView, error) {
	head, err := changeRev(ws, "HEAD")
	if err != nil {
		return nil, errors.New("uncommitted review needs at least one commit")
	}
	v := &changeView{Workspace: ws, Branch: "", Base: "HEAD", MergeBase: head, Head: head, Commits: []changeCommit{}, Comments: []changeCommentView{}}
	if v.Files, err = changeFiles(ws, head, ""); err != nil {
		return nil, err
	}
	others, err := session.ReviewGit(ws, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(string(others), "\x00") {
		if p == "" || strings.HasSuffix(p, "/") || reviewExcluded(p) || !reviewPath(p) {
			continue
		}
		f := changeFile{Path: p, Status: "added"}
		f.Risk, f.RiskReason = changeRisk(p)
		if info, err := os.Lstat(filepath.Join(ws, p)); err == nil && info.Mode().IsRegular() && info.Size() <= reviewFileLimit {
			if data, err := os.ReadFile(filepath.Join(ws, p)); err == nil {
				if reviewText(data) {
					f.Additions = strings.Count(string(data), "\n")
				} else {
					f.Binary = true
				}
			}
		}
		v.Files = append(v.Files, f)
	}
	kept := v.Files[:0]
	for _, f := range v.Files {
		if !reviewExcluded(f.Path) {
			kept = append(kept, f)
		}
	}
	v.Files = kept
	addSymbols(ws, v.Files, head, "")
	v.Stats = map[string]int{"files": len(v.Files), "commits": 0}
	for _, f := range v.Files {
		v.Stats["additions"] += f.Additions
		v.Stats["deletions"] += f.Deletions
	}
	return v, nil
}

// uncommittedDiff returns working-tree diff text against HEAD, including
// untracked files, sanitized exactly once for terminal presentation. Callers
// that need the raw bytes (never print them) use rawUncommittedDiff.
func uncommittedDiff(ws, file string) (string, error) {
	text, err := rawUncommittedDiff(ws, file)
	if err != nil {
		return "", err
	}
	return session.ReviewTerminalText(text), nil
}

// rawUncommittedDiff returns the unsanitized working-tree diff against HEAD.
// The text is untrusted data: sanitize it exactly once at the presentation
// boundary and never pass it through reviewTerminalText twice.
func rawUncommittedDiff(ws, file string) (string, error) {
	if file != "" && !reviewPath(file) {
		return "", errors.New("invalid file path")
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M", "HEAD", "--"}
	if file != "" {
		args = append(args, file)
	}
	out, err := session.ReviewGit(ws, args...)
	if err != nil {
		return "", err
	}
	text := string(out)
	if text == "" && file != "" {
		// An untracked file has no diff against HEAD; show it as added.
		root, err := os.OpenRoot(ws)
		if err != nil {
			return "", err
		}
		defer root.Close()
		blob, err := reviewReadAt(root, file)
		if err != nil {
			return "", err
		}
		if blob.Large || !reviewText(blob.Data) {
			return "Binary or large file added; contents not shown.\n", nil
		}
		if text, err = reviewFullDiff(file, nil, blob.Data, false, true); err != nil {
			return "", err
		}
		text += "\n"
	}
	return text, nil
}

// changeDiff returns unified diff text for the whole change, one commit, or one
// file, sanitized exactly once for terminal presentation.
func changeDiff(ws, branch, baseName, commit, file string) (string, error) {
	text, err := rawChangeDiff(ws, branch, baseName, commit, file, "")
	if err != nil {
		return "", err
	}
	return session.ReviewTerminalText(text), nil
}

// rawChangeDiff returns the unsanitized Git diff bytes for a change. The text
// is untrusted: its owner (the review TUI, which sanitizes per display line and
// must see real CRLF and literal backslashes) sanitizes it exactly once. When
// pinHead is not empty the diff is refused unless the branch still resolves to
// that head, so a diff is never shown for a head other than the pinned one.
func rawChangeDiff(ws, branch, baseName, commit, file, pinHead string) (string, error) {
	v, _, err := loadChange(ws, branch, baseName)
	if err != nil {
		return "", err
	}
	if pinHead != "" && v.Head != pinHead {
		return "", fmt.Errorf("%s moved since it was listed; refresh with R and review the new head", branch)
	}
	if file != "" && !reviewPath(file) {
		return "", errors.New("invalid file path")
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M", v.MergeBase, v.Head, "--"}
	if commit != "" {
		sha, err := changeRev(ws, commit)
		if err != nil {
			return "", err
		}
		in := false
		for _, c := range v.Commits {
			in = in || c.SHA == sha
		}
		if !in {
			return "", fmt.Errorf("commit %s is not part of %s", commit, v.Branch)
		}
		args = []string{"show", "--no-ext-diff", "--no-textconv", "--no-color", "-M", "--format=", sha, "--"}
	}
	if file != "" {
		args = append(args, file)
	}
	out, err := session.ReviewGit(ws, args...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// pendingMessages finds channel items awaiting human approval for runs this
// workspace has started (experiments.messaging with cross_swarm_gate review).
func pendingMessages(ws string) []pendingMessage {
	out := []pendingMessage{}
	reg, err := session.ReadRegistryAny(filepath.Join(ws, ".irrigator", "current-session.json"))
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, entry := range reg.Entries {
		s, err := checkpoint.ReadState(entry.RunPath)
		if err != nil {
			continue
		}
		m := checkpoint.PlanMessaging(s.Plan)
		if m == nil || s.Plan.Campaign == nil {
			continue
		}
		for _, pub := range m.Publish {
			items, _ := messaging.ReadChannel(s.Plan.Campaign.Directory, pub.Channel)
			for _, it := range items {
				key := s.Plan.Campaign.Directory + "/" + it.Channel + "/" + it.ID
				if it.Status != "pending" || seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, pendingMessage{Campaign: s.Plan.Campaign.Directory, Channel: it.Channel, ID: it.ID, From: it.Task + "." + it.Field, Text: session.ReviewTerminalText(it.Text)})
			}
		}
	}
	return out
}

// addSymbols reads hunk headers (`@@ -a,b +c,d @@ context`) for every file of
// a diff and records the enclosing function context and hunk count per file.
func addSymbols(ws string, files []changeFile, from, to string) {
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "-M", "-U0", from}
	if to != "" {
		args = append(args, to)
	}
	out, err := session.ReviewGit(ws, append(args, "--")...)
	if err != nil {
		return
	}
	byPath := map[string]*changeFile{}
	for i := range files {
		byPath[files[i].Path] = &files[i]
	}
	var cur *changeFile
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			cur = byPath[strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")]
			seen = map[string]bool{}
		case strings.HasPrefix(line, "@@ ") && cur != nil:
			cur.Hunks++
			if i := strings.Index(line[3:], "@@"); i >= 0 {
				sym := textsafe.Clip(strings.TrimSpace(line[3+i+2:]), 80)
				// Package and import lines are file preamble, not a changed symbol.
				if strings.HasPrefix(sym, "package ") || strings.HasPrefix(sym, "import ") {
					sym = ""
				}
				if sym != "" && !seen[sym] && len(cur.Symbols) < 12 {
					seen[sym] = true
					cur.Symbols = append(cur.Symbols, sym)
				}
			}
		}
	}
}
