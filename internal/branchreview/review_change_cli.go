package branchreview

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/messaging"
	"github.com/grainulation/irrigator/internal/session"
	"github.com/grainulation/irrigator/internal/terminalview"
)

type reviewFileList []string

func (l *reviewFileList) String() string     { return strings.Join(*l, ",") }
func (l *reviewFileList) Set(v string) error { *l = append(*l, v); return nil }

// changeActions are the review subcommands that operate on branches.
var changeActions = map[string]bool{"list": true, "change": true, "diff": true, "viewed": true, "unviewed": true, "approve": true, "request-changes": true, "prune": true, "tui": true}

// reviewChangeDispatch routes branch review commands; ok is false for the
// snapshot commands handled by reviewCLI.
func reviewChangeDispatch(args []string) (int, error, bool) {
	// Bare `irrigator review` and `irrigator review --flag ...` open the queue.
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && args[0] != commandflags.MetadataArgument) {
		code, err := changeCLI("list", args)
		return code, err, true
	}
	if changeActions[args[0]] {
		code, err := changeCLI(args[0], args[1:])
		return code, err, true
	}
	if args[0] == "comment" {
		for _, a := range args[1:] {
			if a == "--change" || strings.HasPrefix(a, "--change=") {
				code, err := changeCLI("comment", args[1:])
				return code, err, true
			}
		}
	}
	return 0, nil, false
}

func changeCLI(action string, args []string) (int, error) {
	fs := flag.NewFlagSet("review "+action, flag.ContinueOnError)
	workspace := fs.String("workspace", ".", "Git repository to review")
	asJSON := fs.Bool("json", false, "print structured output")
	var base, commit, file, body, change, snapshot, out string
	var files reviewFileList
	var line, olderThan int
	var apply, uncommitted bool
	if action != "prune" {
		fs.StringVar(&base, "base", "", "branch or commit to compare against (default: the remote default branch, then main or master)")
	}
	switch action {
	case "diff":
		fs.StringVar(&commit, "commit", "", "show only this commit of the change")
		fs.StringVar(&file, "file", "", "show only this file")
		fs.BoolVar(&uncommitted, "uncommitted", false, "show working-tree edits against HEAD instead of a branch")
	case "change":
		fs.BoolVar(&uncommitted, "uncommitted", false, "describe working-tree edits against HEAD instead of a branch (saves nothing)")
	case "viewed", "unviewed":
		fs.Var(&files, "file", "changed file to mark (repeatable)")
	case "comment":
		fs.StringVar(&change, "change", "", "branch under review")
		fs.StringVar(&file, "file", "", "changed file")
		fs.IntVar(&line, "line", 0, "line in the file at the reviewed head; 0 for a file comment")
		fs.StringVar(&body, "body", "", "comment text")
	case "approve", "request-changes":
		fs.StringVar(&body, "body", "", "summary for the agent or plan author")
		fs.StringVar(&snapshot, "snapshot", "", "plan, direction or decision snapshot to decide instead of a branch")
		if action == "request-changes" {
			fs.StringVar(&out, "out", "", "also write the feedback to this new file")
		}
	case "prune":
		fs.BoolVar(&apply, "apply", false, "delete the listed items (default: preview only)")
		fs.IntVar(&olderThan, "older-than", 30, "days after which saved review snapshots are removed")
	}
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		return 2, err
	}
	ws, err := session.ChangeWorkspace(*workspace)
	if err != nil {
		return 1, err
	}
	branch := ""
	if fs.NArg() > 1 || (fs.NArg() == 1 && (action == "list" || action == "prune" || action == "comment" || snapshot != "")) {
		return 2, fmt.Errorf("unexpected argument %q", fs.Arg(fs.NArg()-1))
	}
	if fs.NArg() == 1 {
		branch = fs.Arg(0)
	}
	switch action {
	case "list":
		q, err := ListReviews(ws, base)
		if err != nil {
			return 1, err
		}
		report, err := PruneReviews(ws, 30, false)
		if err != nil {
			return 1, err
		}
		if *asJSON {
			return 0, jsonio.PrintJSON(map[string]any{"queue": q, "storage": report})
		}
		printQueue(q, report)
		return 0, nil
	case "change":
		if uncommitted {
			v, err := loadUncommitted(ws)
			if err != nil {
				return 1, err
			}
			if *asJSON {
				return 0, jsonio.PrintJSON(v)
			}
			printChange(v)
			return 0, nil
		}
		v, _, err := loadChange(ws, branch, base)
		if err != nil {
			return 1, err
		}
		if *asJSON {
			return 0, jsonio.PrintJSON(v)
		}
		printChange(v)
		return 0, nil
	case "diff":
		var text string
		if uncommitted {
			text, err = uncommittedDiff(ws, file)
		} else {
			text, err = changeDiff(ws, branch, base, commit, file)
		}
		if err != nil {
			return 1, err
		}
		fmt.Print(text)
		return 0, nil
	case "viewed", "unviewed":
		if len(files) == 0 {
			return 2, errors.New("name at least one --file")
		}
		if err := markViewed(ws, branch, base, files, action == "viewed"); err != nil {
			return 1, err
		}
		fmt.Printf("Marked %d file(s) %s.\n", len(files), action)
		return 0, nil
	case "comment":
		c, err := addChangeComment(ws, change, base, file, line, body)
		if err != nil {
			return 1, err
		}
		if *asJSON {
			return 0, jsonio.PrintJSON(c)
		}
		fmt.Printf("Comment saved on %s:%d.\n", session.ReviewTerminalText(c.Path), c.Line)
		return 0, nil
	case "approve", "request-changes":
		state := map[string]string{"approve": "approved", "request-changes": "changes_requested"}[action]
		if state == "changes_requested" && strings.TrimSpace(body) == "" && snapshot == "" {
			// Comments alone are enough, but an empty request is never useful.
			if v, _, err := loadChange(ws, branch, base); err == nil && len(v.Comments) == 0 {
				return 2, errors.New("request changes needs --body or at least one comment")
			}
		}
		if snapshot != "" {
			snap, err := setDocumentVerdict(ws, snapshot, state, body)
			if err != nil {
				return 1, err
			}
			fmt.Printf("%s %s: %s\n", strings.ReplaceAll(state, "_", " "), reviewKind(snap), session.ReviewTerminalText(snap.Title))
			return 0, nil
		}
		v, feedback, err := setChangeVerdict(ws, branch, base, state, body)
		if err != nil {
			return 1, err
		}
		if out != "" {
			f, err := launch.OpenContextFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return 1, err
			}
			defer f.Close()
			if _, err = f.WriteString(feedback); err != nil {
				return 1, err
			}
		}
		if *asJSON {
			return 0, jsonio.PrintJSON(map[string]any{"change": v, "feedback": feedback})
		}
		if state == "approved" {
			fmt.Printf("Approved %s at %s. A new commit clears this approval.\n", v.Branch, v.Head[:12])
			return 0, nil
		}
		fmt.Print(feedback)
		return 0, nil
	case "tui":
		if *asJSON || fs.NArg() != 0 {
			return 2, errors.New("usage: irrigator review tui [--workspace DIR] [--base REF]")
		}
		return reviewTUI(ws, base)
	case "prune":
		report, err := PruneReviews(ws, olderThan, apply)
		if err != nil {
			return 1, err
		}
		if *asJSON {
			return 0, jsonio.PrintJSON(report)
		}
		printPrune(report)
		return 0, nil
	}
	return 2, errors.New("unknown review action")
}

type pruneItem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Bytes  int64  `json:"bytes"`
}

type pruneReport struct {
	UsageBytes int64       `json:"usage_bytes"`
	Items      []pruneItem `json:"items"`
	Bytes      int64       `json:"bytes"`
	Applied    bool        `json:"applied"`
}

func reviewDirSize(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// pruneReviews finds local data that no longer serves a purpose: state for
// deleted branches or removed snapshots, and snapshots and agent session logs
// older than the cutoff.
// It deletes only with apply, and only real directories and files it created.
func PruneReviews(ws string, olderThanDays int, apply bool) (*pruneReport, error) {
	if olderThanDays < 1 {
		return nil, errors.New("--older-than must be at least 1 day")
	}
	r := &pruneReport{Items: []pruneItem{}, Applied: apply}
	private := filepath.Join(ws, ".irrigator")
	for _, name := range []string{"reviews", "changes", "sessions"} {
		r.UsageBytes += reviewDirSize(filepath.Join(private, name))
	}
	cutoff := time.Now().Add(-time.Duration(olderThanDays) * 24 * time.Hour)
	reviews := filepath.Join(private, "reviews")
	entries, _ := os.ReadDir(reviews)
	for _, e := range entries {
		info, err := os.Lstat(filepath.Join(reviews, e.Name()))
		if err != nil || !info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		p := filepath.Join(reviews, e.Name())
		r.Items = append(r.Items, pruneItem{Path: p, Reason: fmt.Sprintf("snapshot older than %d days", olderThanDays), Bytes: reviewDirSize(p)})
	}
	sessions := filepath.Join(private, "sessions")
	entries, _ = os.ReadDir(sessions)
	for _, e := range entries {
		info, err := os.Lstat(filepath.Join(sessions, e.Name()))
		if err != nil || !info.Mode().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") || info.ModTime().After(cutoff) {
			continue
		}
		r.Items = append(r.Items, pruneItem{Path: filepath.Join(sessions, e.Name()), Reason: fmt.Sprintf("agent session idle for %d days", olderThanDays), Bytes: info.Size()})
	}
	changes := filepath.Join(private, "changes")
	entries, _ = os.ReadDir(changes)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(changes, e.Name())
		data, err := messaging.ReviewReadFile(p, changeStateLimit)
		if err != nil {
			continue
		}
		var s changeState
		if json.Unmarshal(data, &s) != nil || s.Subject == "" {
			continue
		}
		reason := ""
		if id, ok := strings.CutPrefix(s.Subject, "snapshot:"); ok {
			if !reviewSnapshotExists(reviews, id) {
				reason = "its snapshot was removed"
			}
		} else if _, err := session.ReviewGit(ws, "show-ref", "--verify", "--quiet", "refs/heads/"+s.Subject); err != nil {
			reason = "branch " + s.Subject + " no longer exists"
		}
		if reason != "" {
			r.Items = append(r.Items, pruneItem{Path: p, Reason: reason, Bytes: int64(len(data))})
		}
	}
	for _, it := range r.Items {
		r.Bytes += it.Bytes
	}
	if !apply {
		return r, nil
	}
	for _, it := range r.Items {
		info, err := os.Lstat(it.Path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err = os.RemoveAll(it.Path); err != nil {
			return r, err
		}
		if strings.HasSuffix(it.Path, ".json") {
			os.Remove(strings.TrimSuffix(it.Path, ".json") + ".lock")
		}
	}
	return r, nil
}

func reviewSnapshotExists(reviews, id string) bool {
	entries, _ := os.ReadDir(reviews)
	for _, e := range entries {
		if s, err := readReview(filepath.Join(reviews, e.Name())); err == nil && s.ID == id {
			return true
		}
	}
	return false
}

func reviewBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func verdictText(v *changeVerdictView) string {
	if v == nil {
		return "not reviewed"
	}
	label := map[string]string{"approved": "approved", "changes_requested": "changes requested"}[v.State]
	if !v.Current {
		return label + " (outdated: new commits since)"
	}
	return label
}

// reviewWorktrees maps branch names to the checked-out worktree that holds
// them, for terminal links. Any failure returns an empty map: links are a
// convenience and must never change what the list says.
func reviewWorktrees(ws string) map[string]string {
	out, err := session.ReviewGit(ws, "worktree", "list", "--porcelain")
	paths := map[string]string{}
	if err != nil {
		return paths
	}
	dir := ""
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			dir = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch refs/heads/") && dir != "":
			paths[strings.TrimPrefix(line, "branch refs/heads/")] = dir
		}
	}
	return paths
}

// padLinked pads text to width columns, then links the text (not the padding).
func padLinked(link bool, path, text string, width int) string {
	pad := ""
	if n := width - len([]rune(text)); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	return terminalview.PathLink(link, path, text) + pad
}

func printQueue(q *reviewQueue, storage *pruneReport) {
	links := terminalview.LinksEnabled(os.Stdout)
	var worktrees map[string]string
	if links {
		worktrees = reviewWorktrees(q.Workspace)
	}
	fmt.Printf("Review queue · base %s\n", session.ReviewTerminalText(q.Base))
	if q.Uncommitted > 0 {
		fmt.Printf("\n  Uncommitted: %d file(s) in the working tree\n", q.Uncommitted)
	}
	if len(q.Changes) == 0 && len(q.Documents) == 0 {
		fmt.Println("\n  Nothing to review.")
	}
	if len(q.Changes) > 0 {
		fmt.Println("\nChanges")
		for _, c := range q.Changes {
			marker := " "
			if c.Current {
				marker = "*"
			}
			status := verdictText(c.Verdict)
			if c.Awaiting {
				status += " · awaiting agent"
			}
			branch := session.ReviewTerminalText(c.Branch)
			if dir := worktrees[c.Branch]; links && dir != "" {
				branch = padLinked(true, dir, branch, 32)
			} else {
				branch = fmt.Sprintf("%-32s", branch)
			}
			fmt.Printf(" %s %s %d commit(s) · %d file(s) · %s\n", marker, branch, c.Ahead, c.Files, status)
		}
	}
	if len(q.Documents) > 0 {
		fmt.Println("\nPlans and decisions")
		for _, d := range q.Documents {
			title := session.ReviewTerminalText(d.Title)
			if links && d.Path != "" {
				title = terminalview.PathLink(true, d.Path, title)
			}
			fmt.Printf("   %-9s %s · %s\n", d.Kind, title, verdictText(d.Verdict))
		}
	}
	if len(q.Overlaps) > 0 {
		fmt.Println("\nFiles changed on more than one branch")
		for _, o := range q.Overlaps {
			fmt.Printf("   %s: %s\n", session.ReviewTerminalText(o[0]), session.ReviewTerminalText(strings.Join(o[1:], ", ")))
		}
	}
	fmt.Printf("\nLocal Irrigator data: %s", reviewBytes(storage.UsageBytes))
	if len(storage.Items) > 0 {
		fmt.Printf(" · %d item(s) can be removed: irrigator review prune", len(storage.Items))
	}
	fmt.Println()
}

func printChange(v *changeView) {
	if v.Branch == "" {
		fmt.Println("Uncommitted work against HEAD (commit it to review it as a change)")
	} else {
		fmt.Printf("%s → %s · %s\n", session.ReviewTerminalText(v.Branch), session.ReviewTerminalText(v.Base), verdictText(v.Verdict))
	}
	fmt.Printf("%d commit(s) · %d file(s) · +%d −%d · %d/%d viewed · %d comment(s)\n", v.Stats["commits"], v.Stats["files"], v.Stats["additions"], v.Stats["deletions"], v.Stats["viewed"], v.Stats["files"], v.Stats["comments"])
	if v.Awaiting {
		fmt.Println("Changes requested; waiting for a new commit.")
	}
	fmt.Println("\nCommits")
	for _, c := range v.Commits {
		fmt.Printf("  %s %s\n", c.SHA[:10], c.Subject)
	}
	fmt.Println("\nFiles")
	for _, f := range v.Files {
		mark := " "
		if f.Viewed {
			mark = "✓"
		}
		note := ""
		if f.RiskReason != "" {
			note = " · " + f.RiskReason
		}
		counts := fmt.Sprintf("+%d −%d", f.Additions, f.Deletions)
		if f.Binary {
			counts = "binary"
		}
		fmt.Printf("  %s %-9s %s (%s)%s\n", mark, f.Status, session.ReviewTerminalText(f.Path), counts, note)
	}
	fmt.Println("\nNext: irrigator review diff [--file PATH] · review viewed --file PATH · review approve | request-changes --body TEXT")
}

func printPrune(r *pruneReport) {
	fmt.Printf("Local Irrigator data: %s\n", reviewBytes(r.UsageBytes))
	if len(r.Items) == 0 {
		fmt.Println("Nothing to remove.")
		return
	}
	verb := "Would remove"
	if r.Applied {
		verb = "Removed"
	}
	for _, it := range r.Items {
		fmt.Printf("  %s (%s): %s\n", session.ReviewTerminalText(filepath.Base(it.Path)), reviewBytes(it.Bytes), it.Reason)
	}
	fmt.Printf("%s %d item(s), %s.", verb, len(r.Items), reviewBytes(r.Bytes))
	if !r.Applied {
		fmt.Print(" Run irrigator review prune --apply to delete them.")
	}
	fmt.Println()
}
