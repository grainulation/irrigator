package executor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
)

type cleanupReport struct {
	Run    string         `json:"run"`
	RunID  string         `json:"run_id"`
	DryRun bool           `json:"dry_run"`
	Herdr  cleanupSection `json:"herdr"`
	Tmux   cleanupSection `json:"tmux"`
}

// cleanupSection reports one executor. Removed lists what was removed (what
// would be, under --dry-run); Kept lists what was left and why.
type cleanupSection struct {
	Status  string   `json:"status"` // cleaned (would_clean under --dry-run), nothing, skipped, error
	Detail  string   `json:"detail,omitempty"`
	Removed []string `json:"removed"`
	Kept    []string `json:"kept"`
}

func CleanupCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	var dryRun, allFinished bool
	fs.BoolVar(&dryRun, "dry-run", false, "list what would be removed and change nothing")
	fs.BoolVar(&allFinished, "all-finished", false, "treat RUN as a directory of runs and clean every run in it (live terminals and windows are still never touched)")
	var targets []string
	for {
		if err := commandflags.ParseCommandFlags(fs, args); err != nil {
			return 2, err
		}
		if fs.NArg() == 0 {
			break
		}
		targets = append(targets, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(targets) != 1 {
		return 2, errors.New("usage: irrigator cleanup RUN [--dry-run] [--all-finished]")
	}
	runs := []string{targets[0]}
	if allFinished {
		entries, err := os.ReadDir(targets[0])
		if err != nil {
			return 1, err
		}
		runs = nil
		for _, e := range entries {
			dir := filepath.Join(targets[0], e.Name())
			if _, err := os.Stat(filepath.Join(dir, "state.json")); err == nil {
				runs = append(runs, dir)
			}
		}
		sort.Strings(runs)
	}
	code := 0
	var reports []cleanupReport
	for _, run := range runs {
		r, err := cleanupRun(run, dryRun)
		if err != nil {
			if !allFinished {
				return 1, err
			}
			fmt.Fprintf(os.Stderr, "cleanup: %s: %v\n", run, err)
			code = 1
			continue
		}
		if r.Herdr.Status == "error" || r.Tmux.Status == "error" {
			code = 1
		}
		reports = append(reports, r)
	}
	if allFinished {
		return code, jsonio.PrintJSON(reports)
	}
	if len(reports) == 0 {
		return 1, nil
	}
	return code, jsonio.PrintJSON(reports[0])
}

func cleanupRun(run string, dryRun bool) (cleanupReport, error) {
	s, err := checkpoint.ReadState(run)
	if err != nil {
		return cleanupReport{}, err
	}
	if s.RunID == "" {
		return cleanupReport{}, fmt.Errorf("%s has no run id", run)
	}
	r := cleanupReport{Run: run, RunID: s.RunID, DryRun: dryRun,
		Herdr: cleanupHerdr(s.RunID, dryRun), Tmux: cleanupTmux(s.RunID, dryRun)}
	if dryRun {
		// Nothing was removed: say so in the status, not only in dry_run.
		for _, sec := range []*cleanupSection{&r.Herdr, &r.Tmux} {
			if sec.Status == "cleaned" {
				sec.Status = "would_clean"
			}
		}
	}
	return r, nil
}

func cleanupCall(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("%s %s: %v: %s", filepath.Base(bin), args[0], err, text)
	}
	return text, nil
}

// cleanupHerdr removes the run's finished herdr terminals. A terminal whose
// state is missing or unrecognised is treated as live and kept.
func cleanupHerdr(runID string, dryRun bool) cleanupSection {
	sec := cleanupSection{Removed: []string{}, Kept: []string{}}
	st := herdrStatus()
	if st.Status == herdrNotInstalled {
		sec.Status, sec.Detail = "skipped", "hc is not installed"
		return sec
	}
	if st.Path == "" {
		sec.Status, sec.Detail = "error", st.Detail
		return sec
	}
	out, err := cleanupCall(st.Path, "ls", "--all", "--json", "--label", "irrigator.run="+runID)
	if err != nil {
		sec.Status, sec.Detail = "error", err.Error()
		return sec
	}
	listed, err := decodeHerdrListing([]byte(out))
	if err != nil {
		sec.Status, sec.Detail = "error", err.Error()
		return sec
	}
	for _, t := range listed {
		switch {
		case !t.matchesRun(runID, ""):
		case !t.finished():
			sec.Kept = append(sec.Kept, t.Name+" (not finished)")
		case dryRun:
			sec.Removed = append(sec.Removed, t.Name)
		default:
			if _, err := cleanupCall(st.Path, "rm", t.Name); err != nil {
				sec.Status, sec.Detail = "error", err.Error()
				sec.Kept = append(sec.Kept, t.Name+" (hc rm failed)")
				continue
			}
			sec.Removed = append(sec.Removed, t.Name)
		}
	}
	if sec.Status == "" {
		sec.Status = "nothing"
		if len(sec.Removed) > 0 {
			sec.Status = "cleaned"
		}
	}
	return sec
}

// cleanupTmux kills the run's tmux session only when every pane in it is dead.
// Otherwise it lists the live windows and leaves the session alone.
func cleanupTmux(runID string, dryRun bool) cleanupSection {
	sec := cleanupSection{Removed: []string{}, Kept: []string{}}
	path, err := exec.LookPath("tmux")
	if err != nil {
		sec.Status, sec.Detail = "skipped", "tmux is not installed"
		return sec
	}
	session := TmuxSessionName(runID)
	if !tmuxSessionExists(path, session) {
		sec.Status, sec.Detail = "nothing", "no session "+session
		return sec
	}
	out, err := runTmux(path, "list-panes", "-s", "-t", "="+session, "-F", "#{window_name} #{pane_dead}")
	if err != nil {
		sec.Status, sec.Detail = "error", err.Error()
		return sec
	}
	var live []string
	for _, line := range strings.Split(out, "\n") {
		name, dead, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && strings.TrimSpace(dead) != "1" {
			live = append(live, name)
		}
	}
	if len(live) > 0 {
		sec.Status = "nothing"
		sec.Detail = "session " + session + " kept: live windows"
		for _, w := range live {
			sec.Kept = append(sec.Kept, session+":"+w+" (live)")
		}
		return sec
	}
	if !dryRun {
		if _, err := runTmux(path, "kill-session", "-t", "="+session); err != nil {
			sec.Status, sec.Detail = "error", err.Error()
			sec.Kept = append(sec.Kept, session+" (kill-session failed)")
			return sec
		}
	}
	sec.Status = "cleaned"
	sec.Removed = append(sec.Removed, session)
	return sec
}
