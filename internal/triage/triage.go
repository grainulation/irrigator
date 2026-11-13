// Package triage explains why cells of a run are stuck and what to do next.
// `irrigator triage RUN` is read-only: it gathers the evidence a person needs
// before deciding (is the agent really stopped, what did it spend, what is in
// its worktree) and recommends one existing command. `--fix` walks through
// the cells interactively and runs the chosen resolve, retry or cancel only
// after the person confirms; uncertain work is never replayed automatically.
package triage

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/session"
)

// report is the read-only triage of one run.
type report struct {
	Run              string  `json:"run"`
	RunID            string  `json:"run_id"`
	Control          string  `json:"control"`
	ControllerActive bool    `json:"controller_active"`
	Cells            []*cell `json:"cells"`
}

// cell is one stuck cell with its evidence and recommendation.
type cell struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Attempt     int       `json:"attempt"`
	Error       string    `json:"error,omitempty"`
	CostUSD     float64   `json:"reported_cost_usd"`
	CostUnknown bool      `json:"cost_unknown"`
	ReplaySafe  bool      `json:"replay_safe"`
	Process     string    `json:"process"` // stopped, unconfirmed, none (no process record)
	WorkerLive  bool      `json:"worker_live"`
	LastLines   []string  `json:"last_lines,omitempty"`
	Code        *codeInfo `json:"code,omitempty"`
	Advice      advice    `json:"advice"`
}

// codeInfo describes the worktree of an `irrigator code` cell.
type codeInfo struct {
	Repo        string   `json:"repo"`
	Base        string   `json:"base"`
	Branch      string   `json:"branch"`
	Worktree    string   `json:"worktree"`
	WorktreeOK  bool     `json:"worktree_exists"`
	BranchOK    bool     `json:"branch_exists"`
	Commits     int      `json:"commits_beyond_base"`
	Head        string   `json:"head,omitempty"`
	Diffstat    string   `json:"diffstat,omitempty"`
	Uncommitted []string `json:"uncommitted,omitempty"`
}

// advice is the recommended next step.
type advice struct {
	Situation string   `json:"situation"`
	Next      string   `json:"next"`
	Commands  []string `json:"commands"`
}

// analyze reads one run and returns its stuck cells. It changes nothing.
func analyze(root string) (*report, error) {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return nil, err
	}
	live, _ := session.ProbeLock(filepath.Join(root, "controller.lock"))
	r := &report{Run: root, RunID: s.RunID, Control: s.Control, ControllerActive: live}
	ids := make([]string, 0, len(s.Tasks))
	for id, row := range s.Tasks {
		if row != nil && (row.Status == "uncertain" || row.Status == "failed") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		r.Cells = append(r.Cells, analyzeCell(s, root, id, live))
	}
	return r, nil
}

func analyzeCell(s *model.State, root, id string, controllerLive bool) *cell {
	row := s.Tasks[id]
	c := &cell{ID: id, Status: row.Status, Attempt: row.Attempt, Error: row.Error, CostUSD: row.Cost,
		CostUnknown: row.CostUnknown || row.CostBasis == model.CostBasisUnknown, ReplaySafe: graph.ReplaySafe(s, row.Spec)}
	cell := filepath.Join(root, "cells", id)
	prefix := filepath.Join(cell, strconv.Itoa(row.Attempt))
	switch {
	case row.Attempt == 0:
		c.Process = "none"
	case model.ProviderProcessStopped(prefix):
		c.Process = "stopped"
	default:
		if _, err := os.Stat(prefix + ".process.json"); err != nil {
			c.Process = "none"
		} else {
			c.Process = "unconfirmed"
		}
	}
	c.WorkerLive, _ = session.ProbeLock(filepath.Join(cell, "lease"))
	c.LastLines = tail(prefix+".stderr.log", 6)
	c.Code = codeEvidence(s, row, id)
	c.Advice = advise(c, root, controllerLive)
	return c
}

// codeEvidence recognizes cells run by `irrigator code` from their provider
// command and inspects the cell's branch and worktree.
func codeEvidence(s *model.State, row *model.TaskState, id string) *codeInfo {
	spec, ok := s.Plan.Providers[row.Spec.Provider]
	if !ok || len(spec.Command) < 2 || spec.Command[1] != "_code-worker" || row.Attempt == 0 {
		return nil
	}
	flag := func(name, def string) string {
		for i := 2; i+1 < len(spec.Command); i++ {
			if spec.Command[i] == "--"+name {
				return spec.Command[i+1]
			}
		}
		return def
	}
	repo := flag("repo", "")
	if repo == "" {
		return nil
	}
	run8 := s.RunID
	if len(run8) > 8 {
		run8 = run8[:8]
	}
	root := flag("worktrees", filepath.Join(filepath.Dir(repo), "irrigator-worktrees"))
	e := &codeInfo{Repo: repo, Base: flag("base", "main"), Branch: "irrigator/" + run8 + "/" + id,
		Worktree: filepath.Join(root, run8, id+"-a"+strconv.Itoa(row.Attempt))}
	if _, err := os.Stat(e.Worktree); err == nil {
		e.WorktreeOK = true
	}
	if out, err := git(repo, "rev-parse", "--verify", "--quiet", e.Branch); err == nil && out != "" {
		e.BranchOK, e.Head = true, out
		if n, err := git(repo, "rev-list", "--count", e.Base+".."+e.Branch); err == nil {
			e.Commits, _ = strconv.Atoi(n)
		}
		stat, _ := git(repo, "diff", "--shortstat", e.Base+"..."+e.Branch)
		e.Diffstat = strings.TrimSpace(stat)
	}
	if e.WorktreeOK {
		if out, err := git(e.Worktree, "status", "--porcelain"); err == nil && out != "" {
			e.Uncommitted = strings.Split(out, "\n")
		}
	}
	return e
}

func advise(c *cell, root string, controllerLive bool) advice {
	q := func(s string) string { return shellQuote(s) }
	run, id := q(root), q(c.ID)
	confirm := ""
	if c.Process != "stopped" {
		confirm = " --confirm-stopped"
	}
	switch {
	case controllerLive || c.WorkerLive:
		return advice{Situation: "still running", Next: "Wait for it, or stop the run first. Do not retry while it runs.",
			Commands: []string{"irrigator status " + run + " --watch", "irrigator down " + run}}
	case c.Status == "failed" && c.ReplaySafe:
		return advice{Situation: "failed, safe to replay", Next: "Retry it and resume.",
			Commands: []string{"irrigator retry " + run + " " + id, "irrigator resume " + run}}
	}
	var a advice
	if c.Process == "unconfirmed" {
		a.Next = "The agent's exit was never confirmed: check that no agent process for this cell is still running before anything below. "
	}
	if c.Status == "failed" {
		a.Situation = "failed (not safe to replay automatically)"
	} else {
		a.Situation = "uncertain: the agent may have done some or all of its work"
	}
	if e := c.Code; e != nil {
		wt := q(e.Worktree)
		switch {
		case len(e.Uncommitted) > 0:
			a.Next += "The worktree has uncommitted changes. Review them, then commit them to keep the work (or discard them), and run triage again."
			a.Commands = []string{"git -C " + wt + " status", "git -C " + wt + " diff",
				"git -C " + wt + " add -A && git -C " + wt + " commit -m 'irrigator triage: leftover agent changes'"}
			return a
		case e.Commits > 0:
			a.Next += "The branch has " + strconv.Itoa(e.Commits) + " commit(s). Review them; if the work is complete, accept it as the cell's result (triage --fix writes the result file). Otherwise retry to continue the branch."
			a.Commands = []string{"git -C " + q(e.Repo) + " log --oneline " + q(e.Base) + ".." + q(e.Branch),
				"irrigator triage " + run + " --fix",
				"irrigator retry " + run + " " + id + retryFlags(c) + confirm + " && irrigator resume " + run}
			return a
		default:
			a.Next += "The branch has no work. Retry the cell, or cancel it."
			a.Commands = []string{"irrigator retry " + run + " " + id + retryFlags(c) + confirm + " && irrigator resume " + run,
				"irrigator cancel " + run + " " + id}
			return a
		}
	}
	a.Next += "Check what the provider actually did (logs, the external system). If it finished, record its result with resolve; if not, retry it."
	a.Commands = []string{"irrigator logs " + run + " --task " + id, "irrigator inspect " + run + " " + id,
		"irrigator resolve " + run + " " + id + " --result RESULT.json" + confirm,
		"irrigator retry " + run + " " + id + retryFlags(c) + confirm + " && irrigator resume " + run}
	return a
}

func retryFlags(c *cell) string {
	if c.Status == "uncertain" {
		return " --allow-uncertain"
	}
	return ""
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimRight(string(out), "\n"), err
}

func tail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			lines = append(lines, line)
			if len(lines) > n {
				lines = lines[1:]
			}
		}
	}
	return lines
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@=", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var errControllerLive = errors.New("a controller is running this run; stop it first (irrigator down RUN) and run triage again")
