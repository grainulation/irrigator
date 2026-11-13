package triage

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
)

// runner runs an irrigator command in-process (for example ["retry", RUN, ID]).
type runner func(args []string) (int, error)

// CLI is `irrigator triage RUN [--json] [--fix]`.
func CLI(args []string, run runner) (int, error) {
	fs := flag.NewFlagSet("triage", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the triage as JSON")
	fix := fs.Bool("fix", false, "walk through each stuck cell and run the step you choose (needs a terminal)")
	var positional []string
	rest := args
	for {
		if err := commandflags.ParseCommandFlags(fs, rest); err != nil {
			return 2, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return 2, errors.New("usage: irrigator triage RUN [--json] [--fix]")
	}
	report, err := analyze(positional[0])
	if err != nil {
		return 1, err
	}
	switch {
	case *fix && *asJSON:
		return 2, errors.New("--fix and --json cannot be combined")
	case *fix:
		if !term.IsTerminal(os.Stdin.Fd()) {
			return 2, errors.New("--fix asks before every step and needs a terminal; run irrigator triage RUN to see the commands instead")
		}
		return guide(report, os.Stdin, os.Stdout, run)
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(report)
	}
	printReport(os.Stdout, report)
	return 0, nil
}

// Print writes the human triage.
func printReport(w io.Writer, r *report) {
	controller := "not running"
	if r.ControllerActive {
		controller = "RUNNING"
	}
	fmt.Fprintf(w, "Triage of %s (run %s, control %s, controller %s)\n", r.Run, r.RunID, r.Control, controller)
	if len(r.Cells) == 0 {
		fmt.Fprintln(w, "\nNothing is stuck: no cell is uncertain or failed.")
		return
	}
	for _, c := range r.Cells {
		fmt.Fprintln(w)
		printCell(w, c)
		fmt.Fprintf(w, "  → %s\n", c.Advice.Situation)
		fmt.Fprintf(w, "    %s\n", c.Advice.Next)
		for _, cmd := range c.Advice.Commands {
			fmt.Fprintf(w, "    $ %s\n", cmd)
		}
	}
	fmt.Fprintln(w, "\nNothing was changed. Run with --fix to go through these one by one.")
}

func printCell(w io.Writer, c *cell) {
	fmt.Fprintf(w, "%s  %s  attempt %d\n", c.ID, c.Status, c.Attempt)
	process := map[string]string{
		"stopped":     "stopped (confirmed)",
		"unconfirmed": "exit NOT confirmed: check that no agent process for this cell is still running",
		"none":        "no process record",
	}[c.Process]
	fmt.Fprintf(w, "  agent process: %s\n", process)
	if c.WorkerLive {
		fmt.Fprintln(w, "  worker: still running")
	}
	cost := fmt.Sprintf("$%.4f", c.CostUSD)
	if c.CostUnknown {
		cost = "unknown (not $0)"
	}
	fmt.Fprintf(w, "  cost: %s\n", cost)
	if c.Error != "" {
		fmt.Fprintf(w, "  error: %s\n", firstLine(c.Error, 200))
	}
	if e := c.Code; e != nil {
		switch {
		case !e.BranchOK:
			fmt.Fprintf(w, "  branch: %s (missing)\n", e.Branch)
		default:
			stat := e.Diffstat
			if stat == "" {
				stat = "no changes"
			}
			fmt.Fprintf(w, "  branch: %s, %d commit(s) beyond %s (%s)\n", e.Branch, e.Commits, e.Base, stat)
		}
		where := "exists"
		if !e.WorktreeOK {
			where = "missing"
		}
		fmt.Fprintf(w, "  worktree: %s (%s)\n", e.Worktree, where)
		if len(e.Uncommitted) > 0 {
			fmt.Fprintf(w, "  uncommitted: %s\n", strings.Join(e.Uncommitted, ", "))
		}
	}
	if len(c.LastLines) > 0 {
		fmt.Fprintln(w, "  last progress:")
		for _, l := range c.LastLines {
			fmt.Fprintf(w, "    | %s\n", l)
		}
	}
}

// guide walks through each stuck cell, offers the steps that fit it, and runs the
// chosen one through the normal commands. Anything that attests the agent has
// stopped needs the person to type "stopped" first.
func guide(r *report, in io.Reader, out io.Writer, run runner) (int, error) {
	if r.ControllerActive {
		return 1, errControllerLive
	}
	if len(r.Cells) == 0 {
		fmt.Fprintln(out, "Nothing is stuck: no cell is uncertain or failed.")
		return 0, nil
	}
	reader := bufio.NewReader(in)
	ask := func(prompt string) string {
		fmt.Fprint(out, prompt)
		line, _ := reader.ReadString('\n')
		return strings.TrimSpace(line)
	}
	queued := false
	for i := 0; i < len(r.Cells); i++ {
		c := r.Cells[i]
		fmt.Fprintf(out, "\n[%d/%d] ", i+1, len(r.Cells))
		printCell(out, c)
		fmt.Fprintf(out, "  → %s\n    %s\n", c.Advice.Situation, c.Advice.Next)
		if c.WorkerLive {
			fmt.Fprintln(out, "  Its worker is still running; skipping.")
			continue
		}
		opts := options(c)
		for _, o := range opts {
			fmt.Fprintf(out, "  [%s] %s\n", o.key, o.label)
		}
		choice := strings.ToLower(ask("  Choose: "))
		var picked *option
		for j := range opts {
			if opts[j].key == choice {
				picked = &opts[j]
			}
		}
		if picked == nil || picked.key == "s" {
			fmt.Fprintln(out, "  Skipped.")
			continue
		}
		confirm := []string{}
		if picked.attests && c.Process != "stopped" {
			if ask("  This attests that no agent process for this cell is still running. Type 'stopped' to confirm: ") != "stopped" {
				fmt.Fprintln(out, "  Not confirmed; skipped.")
				continue
			}
			confirm = []string{"--confirm-stopped"}
		}
		args, err := picked.build(r.Run, c, ask)
		if err != nil {
			fmt.Fprintf(out, "  %v; skipped.\n", err)
			continue
		}
		if args == nil { // the step changed evidence only: look at this cell again
			fresh, err := analyze(r.Run)
			if err != nil {
				return 1, err
			}
			for _, fc := range fresh.Cells {
				if fc.ID == c.ID {
					r.Cells[i] = fc
					i--
				}
			}
			continue
		}
		args = append(args, confirm...)
		fmt.Fprintf(out, "  $ irrigator %s\n", strings.Join(args, " "))
		if code, err := run(args); err != nil || code != 0 {
			fmt.Fprintf(out, "  Failed (exit %d): %v\n", code, err)
			continue
		}
		if picked.key != "x" {
			queued = true
		}
	}
	if queued && strings.EqualFold(ask("\nResume the run now so retried and accepted cells' dependents continue? [y/N] "), "y") {
		fmt.Fprintf(out, "$ irrigator resume %s\n", r.Run)
		return run([]string{"resume", r.Run})
	}
	if queued {
		fmt.Fprintf(out, "\nWhen ready: irrigator resume %s\n", r.Run)
	}
	return 0, nil
}

type option struct {
	key, label string
	attests    bool
	// build returns the irrigator command to run, or nil args after a step that
	// only changed evidence (the cell is then shown again).
	build func(run string, c *cell, ask func(string) string) ([]string, error)
}

func options(c *cell) []option {
	var opts []option
	retry := func(run string, c *cell, _ func(string) string) ([]string, error) {
		args := []string{"retry", run, c.ID}
		if c.Status == "uncertain" {
			args = append(args, "--allow-uncertain")
		}
		return args, nil
	}
	if e := c.Code; e != nil {
		if len(e.Uncommitted) > 0 && e.WorktreeOK {
			opts = append(opts, option{key: "c", label: "commit the uncommitted changes to the cell's branch (then choose again)",
				build: func(_ string, c *cell, _ func(string) string) ([]string, error) {
					if _, err := git(e.Worktree, "add", "-A"); err != nil {
						return nil, err
					}
					if _, err := git(e.Worktree, "commit", "-q", "-m", "irrigator triage: leftover agent changes in "+c.ID); err != nil {
						return nil, errors.New("git commit failed")
					}
					return nil, nil
				}})
		}
		if e.BranchOK && e.Commits > 0 && len(e.Uncommitted) == 0 {
			opts = append(opts, option{key: "a", attests: true, label: fmt.Sprintf("accept the branch's %d commit(s) as the cell's result", e.Commits),
				build: func(run string, c *cell, _ func(string) string) ([]string, error) {
					path, err := writeCodeResult(run, c)
					if err != nil {
						return nil, err
					}
					return []string{"resolve", run, c.ID, "--result", path}, nil
				}})
		}
	} else if c.Status == "uncertain" {
		opts = append(opts, option{key: "f", attests: true, label: "record a result from a JSON file you provide (resolve)",
			build: func(run string, c *cell, ask func(string) string) ([]string, error) {
				path := ask("  Result JSON file: ")
				if path == "" {
					return nil, errors.New("no file given")
				}
				return []string{"resolve", run, c.ID, "--result", path}, nil
			}})
	}
	label := "retry the cell (it runs again when the run resumes)"
	if c.Status == "uncertain" {
		label = "retry the cell (may repeat work the agent already did; a code cell continues its branch)"
	}
	opts = append(opts, option{key: "r", attests: c.Status == "uncertain" || !c.ReplaySafe, label: label, build: retry})
	opts = append(opts, option{key: "x", label: "cancel the cell (give up on it)",
		build: func(run string, c *cell, _ func(string) string) ([]string, error) {
			return []string{"cancel", run, c.ID}, nil
		}})
	return append(opts, option{key: "s", label: "skip for now"})
}

// writeCodeResult records the branch as the cell's outputs for resolve.
func writeCodeResult(run string, c *cell) (string, error) {
	s, err := checkpoint.ReadState(run)
	if err != nil {
		return "", err
	}
	e := c.Code
	summary := fmt.Sprintf("Accepted with irrigator triage after an %s attempt: %d commit(s) on %s (%s).", c.Status, e.Commits, e.Branch, e.Diffstat)
	known := map[string]string{"branch": e.Branch, "commit": e.Head, "diffstat": e.Diffstat, "summary": summary}
	result := map[string]string{}
	for _, k := range s.Tasks[c.ID].Spec.Requires {
		if v, ok := known[k]; ok {
			result[k] = v
		} else {
			result[k] = summary
		}
	}
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(run, "cells", c.ID, "triage-result-"+strconv.Itoa(c.Attempt)+".json")
	return path, os.WriteFile(path, append(raw, '\n'), 0o600)
}

func firstLine(s string, n int) string {
	s = strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
	if len(s) > n {
		s = s[:n]
	}
	return s
}
