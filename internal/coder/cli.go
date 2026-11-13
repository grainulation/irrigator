package coder

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
)

var cellID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// planOptions describes a plan of coding cells.
type planOptions struct {
	agent, repo, base, model, worktrees, name, continueFrom string
	tasks, verify, depends, addDirs                         []string
	executable                                              string // absolute path of the irrigator binary that runs the cells
}

// buildPlan returns a swarm/v1 plan whose cells each run Options.Agent in its
// own worktree through `irrigator _code-worker`.
func buildPlan(o planOptions) (map[string]any, error) {
	if !slices.Contains(agents, o.agent) {
		return nil, fmt.Errorf("--agent must be one of %s, got %q", strings.Join(agents, ", "), o.agent)
	}
	order, tasks := []string{}, map[string]map[string]any{}
	for _, spec := range o.tasks {
		id, prompt, ok := strings.Cut(spec, "=")
		if !ok || !cellID.MatchString(id) || strings.TrimSpace(prompt) == "" {
			return nil, fmt.Errorf("--task needs ID=PROMPT with a simple ID (letters, digits, _ and -), got %q", spec)
		}
		if tasks[id] != nil {
			return nil, fmt.Errorf("duplicate task %s", id)
		}
		order = append(order, id)
		tasks[id] = map[string]any{"id": id, "provider": "coder", "prompt": strings.TrimSpace(prompt), "requires": outputFields}
	}
	if len(tasks) == 0 {
		return nil, errors.New("give at least one --task ID=PROMPT")
	}
	for _, spec := range o.depends {
		id, list, ok := strings.Cut(spec, "=")
		var deps []string
		for _, d := range strings.Split(list, ",") {
			if d != "" {
				deps = append(deps, d)
			}
		}
		valid := ok && tasks[id] != nil && len(deps) > 0
		for _, d := range deps {
			valid = valid && tasks[d] != nil && d != id
		}
		if !valid {
			return nil, fmt.Errorf("--depends needs ID=DEP[,DEP] naming existing, different tasks, got %q", spec)
		}
		fields := map[string][]string{}
		for _, d := range deps {
			fields[d] = []string{"summary"}
		}
		tasks[id]["depends_on"], tasks[id]["input_fields"] = deps, fields
	}
	context := map[string]string{}
	for _, spec := range o.verify {
		id, command, ok := strings.Cut(spec, "=")
		if !ok || tasks[id] == nil || strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("--verify needs ID=COMMAND naming an existing task, got %q", spec)
		}
		key := "verify_" + strings.ReplaceAll(id, "-", "_")
		if _, dup := context[key]; dup {
			return nil, fmt.Errorf("duplicate --verify for %s", id)
		}
		context[key] = strings.TrimSpace(command)
		tasks[id]["context"] = []string{key}
	}
	repo, err := filepath.Abs(o.repo)
	if err != nil {
		return nil, err
	}
	command := []string{o.executable, "_code-worker", "--agent", o.agent, "--repo", repo, "--base", o.base}
	if o.model != "" {
		command = append(command, "--model", o.model)
	}
	if o.worktrees != "" {
		w, err := filepath.Abs(o.worktrees)
		if err != nil {
			return nil, err
		}
		command = append(command, "--worktrees", w)
	}
	for _, d := range o.addDirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		command = append(command, "--add-dir", abs)
	}
	if o.continueFrom != "" {
		command = append(command, "--continue-from", o.continueFrom)
	}
	list := make([]map[string]any, 0, len(order))
	for _, id := range order {
		list = append(list, tasks[id])
	}
	name := o.name
	if name == "" {
		name = "Worktree coding cells"
	}
	// USER lets Claude Code find its macOS Keychain login; see docs/coding-cells.md.
	plan := map[string]any{"api_version": "swarm/v1", "name": name, "max_tasks": len(list),
		"providers": map[string]any{"coder": map[string]any{"command": command, "env": []string{"USER"}}},
		"tasks":     list}
	if len(context) > 0 {
		plan["context"] = context
	}
	return plan, nil
}

// applyFunc runs `irrigator apply PLAN args...`; the CLI package provides it.
type applyFunc func(args []string) (int, error)

// CodeCLI is `irrigator code`: build a plan of coding cells and apply it, or
// `irrigator code cleanup` to remove a run's worktrees.
func CodeCLI(args []string, apply applyFunc) (int, error) {
	if len(args) > 0 && args[0] == "cleanup" {
		return cleanupCLI(args[1:], os.Stdout)
	}
	var o planOptions
	var tasks, verify, depends, addDirs multi
	var run, timeout, out string
	var workers int
	var budget float64
	var planOnly bool
	fs := flag.NewFlagSet("code", flag.ContinueOnError)
	fs.StringVar(&o.agent, "agent", "claude", "coding agent: claude, codex or agy")
	fs.StringVar(&o.repo, "repo", ".", "git repository the agents change")
	fs.StringVar(&o.base, "base", "main", "branch or commit each cell branch starts from")
	fs.StringVar(&o.model, "model", "", "agent model (default: sonnet for claude, the agent's own default otherwise)")
	fs.Var(&tasks, "task", "one coding cell as ID=PROMPT; repeat for more")
	fs.Var(&verify, "verify", "ID=COMMAND run in ID's worktree after the agent finishes; non-zero fails the cell")
	fs.Var(&depends, "depends", "ID=DEP[,DEP]: run ID after DEP and show it DEP's summary")
	fs.Var(&addDirs, "add-dir", "extra directory agents may read, outside the worktree (repeatable)")
	fs.StringVar(&o.continueFrom, "continue-from", "", "start each cell from irrigator/<RUNID[:8]>/<cell> when that branch exists")
	fs.StringVar(&o.worktrees, "worktrees", "", "worktree root (default: irrigator-worktrees beside the repo)")
	fs.StringVar(&o.name, "name", "", "plan name")
	fs.StringVar(&run, "run", "", "new run directory (default: runs/code-<time>)")
	fs.IntVar(&workers, "workers", 2, "cells that run at once")
	fs.StringVar(&timeout, "timeout", "30m", "time limit per cell")
	fs.Float64Var(&budget, "call-budget", 1, "USD cap per cell (enforced by claude; codex and agy report cost as unknown)")
	fs.StringVar(&out, "out", "", "also write the plan to this file")
	fs.BoolVar(&planOnly, "plan-only", false, "print the plan and do not run it")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		return 2, err
	}
	if fs.NArg() > 0 {
		return 2, fmt.Errorf("unexpected argument %q; give tasks with --task ID=PROMPT", fs.Arg(0))
	}
	o.tasks, o.verify, o.depends, o.addDirs = tasks, verify, depends, addDirs
	exe, err := os.Executable()
	if err != nil {
		return 1, err
	}
	if o.executable, err = filepath.EvalSymlinks(exe); err != nil {
		return 1, err
	}
	if _, err := git(o.repo, "rev-parse", "--verify", o.base+"^{commit}"); err != nil {
		return 2, fmt.Errorf("--repo must be a git repository containing --base: %w", err)
	}
	plan, err := buildPlan(o)
	if err != nil {
		return 2, err
	}
	raw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return 1, err
	}
	raw = append(raw, '\n')
	if planOnly {
		_, err = os.Stdout.Write(raw)
		return 0, err
	}
	if run == "" {
		run = filepath.Join("runs", "code-"+time.Now().UTC().Format("20060102-150405"))
	}
	planPath := out
	if planPath == "" {
		planPath = filepath.Clean(run) + ".plan.json"
	}
	if err := os.MkdirAll(filepath.Dir(planPath), 0o755); err != nil {
		return 1, err
	}
	if err := os.WriteFile(planPath, raw, 0o644); err != nil {
		return 1, err
	}
	fmt.Fprintf(os.Stderr, "plan: %s\nrun: %s\nreview results: irrigator review list --workspace %s --base %s\n", planPath, run, o.repo, o.base)
	return apply([]string{planPath, "--run", run, "--backend", "coder", "--workers", fmt.Sprint(workers),
		"--timeout", timeout, "--call-budget", fmt.Sprint(budget)})
}

// cleanupCLI lists and removes one run's worktrees (and optionally branches).
func cleanupCLI(args []string, out io.Writer) (int, error) {
	var repo, run string
	var deleteBranches, force, dryRun bool
	fs := flag.NewFlagSet("code cleanup", flag.ContinueOnError)
	fs.StringVar(&repo, "repo", ".", "git repository the cells ran in")
	fs.StringVar(&run, "run", "", "run ID (the first eight characters are used)")
	fs.BoolVar(&deleteBranches, "delete-branches", false, "also delete the run's branches (git branch -D)")
	fs.BoolVar(&force, "force", false, "remove dirty worktrees too, discarding uncommitted work")
	fs.BoolVar(&dryRun, "dry-run", false, "list what would be removed and change nothing")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		return 2, err
	}
	run8 := run
	if len(run8) > 8 {
		run8 = run8[:8]
	}
	if run8 == "" {
		return 2, errors.New("usage: irrigator code cleanup --repo DIR --run RUNID [--dry-run] [--delete-branches] [--force]")
	}
	if _, err := git(repo, "worktree", "prune"); err != nil {
		return 1, err
	}
	found, err := worktreesOn(repo, "irrigator/"+run8+"/")
	if err != nil {
		return 1, err
	}
	refused := map[string]bool{}
	for _, wt := range found {
		path, branch := wt[0], wt[1]
		if samePath(path, repo) {
			fmt.Fprintf(out, "skip   %s (main repository) %s\n", path, branch)
			refused[branch] = true
			continue
		}
		status, err := git(path, "status", "--porcelain")
		if err != nil {
			return 1, err
		}
		dirty, note := status != "", ""
		if dirty {
			note = " (dirty)"
		}
		switch {
		case dirty && !force:
			fmt.Fprintf(out, "refuse %s %s: uncommitted work (use --force to discard it)\n", path, branch)
			refused[branch] = true
		case dryRun:
			fmt.Fprintf(out, "would remove %s %s%s\n", path, branch, note)
		default:
			removeArgs := []string{"worktree", "remove"}
			if dirty {
				removeArgs = append(removeArgs, "--force")
			}
			if _, err := git(repo, append(removeArgs, path)...); err != nil {
				return 1, err
			}
			fmt.Fprintf(out, "removed %s %s%s\n", path, branch, note)
		}
	}
	list, err := git(repo, "branch", "--list", "irrigator/"+run8+"/*", "--format=%(refname:short)")
	if err != nil {
		return 1, err
	}
	for _, branch := range strings.Split(list, "\n") {
		switch {
		case branch == "":
		case !deleteBranches:
			fmt.Fprintf(out, "kept   branch %s\n", branch)
		case refused[branch]:
			fmt.Fprintf(out, "kept   branch %s (its worktree was not removed)\n", branch)
		case dryRun:
			fmt.Fprintf(out, "would delete branch %s\n", branch)
		default:
			if _, err := git(repo, "branch", "-D", branch); err != nil {
				return 1, err
			}
			fmt.Fprintf(out, "deleted branch %s\n", branch)
		}
	}
	if len(refused) > 0 {
		return 1, nil
	}
	return 0, nil
}
