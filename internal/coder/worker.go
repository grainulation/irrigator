// Package coder runs coding agents (Claude Code, Codex, Antigravity) in git
// worktrees: the hidden `_code-worker` command speaks swarm.worker/v1 for one
// cell, and `irrigator code` builds and applies a plan of such cells.
package coder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/model"
)

const protocol = "swarm.worker/v1"

// outputFields are the outputs every coding cell produces.
var outputFields = []string{"branch", "commit", "diffstat", "summary"}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type workerOptions struct {
	agent, repo, base, model, worktrees, continueFrom string
	addDirs                                           multi
	noTTY                                             bool
}

// WorkerCLI is the hidden `_code-worker` command: one swarm.worker/v1 request on
// stdin, one response on stdout, progress on stderr (and /dev/tty).
func WorkerCLI(args []string) (int, error) {
	return runWorker(args, os.Stdin, os.Stdout, os.Stderr)
}

func runWorker(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	var o workerOptions
	fs := flag.NewFlagSet("_code-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.agent, "agent", "claude", "coding agent: claude, codex or agy")
	fs.StringVar(&o.repo, "repo", "", "path of the git repository")
	fs.StringVar(&o.base, "base", "main", "branch or commit new cell branches start from")
	fs.StringVar(&o.model, "model", "", "agent model (default: sonnet for claude, the agent's own default otherwise)")
	fs.StringVar(&o.worktrees, "worktrees", "", "worktree root (default: irrigator-worktrees beside the repo)")
	fs.Var(&o.addDirs, "add-dir", "extra directory the agent may read, outside the worktree (repeatable)")
	fs.StringVar(&o.continueFrom, "continue-from", "", "earlier run id; a new cell branch starts from its irrigator/<RUNID[:8]>/<cell> branch")
	fs.BoolVar(&o.noTTY, "no-tty", false, "do not write progress to /dev/tty")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		return 2, err
	}
	if o.repo == "" {
		return 2, errors.New("--repo is required")
	}
	if o.model == "" {
		o.model = defaultModel(o.agent)
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return 1, err
	}
	var req model.WorkRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return 1, fmt.Errorf("read request: %w", err)
	}
	resp := work(o, req, newProgress(stderr, !o.noTTY))
	resp["protocol"], resp["request_id"] = protocol, req.RequestID
	return 0, json.NewEncoder(stdout).Encode(resp)
}

type progress struct {
	out io.Writer // locked: the agent's stderr is copied here concurrently
	tty *os.File
}

// lockedWriter serializes writes from progress lines and the agent's stderr.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func newProgress(out io.Writer, tty bool) *progress {
	p := &progress{out: lockedWriter{mu: &sync.Mutex{}, w: out}}
	if tty {
		if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
			p.tty = f
		}
	}
	return p
}

func (p *progress) say(cell, text string) {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		msg := "[" + cell + "] " + line
		fmt.Fprintln(p.out, msg)
		if p.tty != nil {
			if len(msg) > 220 {
				msg = msg[:220]
			}
			if _, err := fmt.Fprintln(p.tty, msg); err != nil {
				p.tty = nil
			}
		}
	}
}

func outcome(o string, extra map[string]any) map[string]any {
	resp := map[string]any{"outcome": o}
	for k, v := range extra {
		resp[k] = v
	}
	return resp
}

// work runs one cell and returns the response body (without protocol/request_id).
func work(o workerOptions, req model.WorkRequest, p *progress) map[string]any {
	cell := req.Task.ID
	if cell == "" {
		cell = "cell"
	}
	if req.Kind != "task" {
		return outcome("not_started", map[string]any{"error": "the coding worker only runs tasks, not validators"})
	}
	if _, _, err := agentArgs(launch{agent: o.agent}); err != nil {
		return outcome("not_started", map[string]any{"error": err.Error()})
	}
	run8 := req.RunID
	if run8 == "" {
		run8 = "run"
	}
	if len(run8) > 8 {
		run8 = run8[:8]
	}
	branch := "irrigator/" + run8 + "/" + cell
	root := o.worktrees
	if root == "" {
		abs, _ := filepath.Abs(o.repo)
		root = filepath.Join(filepath.Dir(abs), "irrigator-worktrees")
	}
	attempt := req.Attempt
	if attempt == 0 {
		attempt = 1
	}
	worktree := filepath.Join(root, run8, fmt.Sprintf("%s-a%d", cell, attempt))

	baseSHA, previous, err := setupWorktree(o, branch, worktree, cell)
	if errors.Is(err, errDirty) {
		return outcome("not_started", map[string]any{"error": "retry refused: " + strings.TrimPrefix(err.Error(), errDirty.Error()+": ")})
	}
	if err != nil {
		return outcome("not_started", map[string]any{"error": "worktree setup failed: " + err.Error()})
	}
	commonDir, _ := git(worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")

	budget := math.Max(0.01, req.MaxCostUSD)
	if req.MaxCostUSD == 0 {
		budget = 1
	}
	timeoutMS := req.TimeoutMS
	if timeoutMS == 0 {
		timeoutMS = 1800000
	}
	seconds := math.Max(1, float64(timeoutMS)/1000-30)
	bin, args, _ := agentArgs(launch{agent: o.agent, model: o.model, prompt: promptFor(req, worktree, branch, o.base, previous),
		worktree: worktree, gitCommonDir: commonDir, addDirs: o.addDirs, budgetUSD: budget, seconds: int(seconds)})

	modelNote := o.model
	if modelNote == "" {
		modelNote = "agent default"
	}
	budgetNote := fmt.Sprintf("budget $%.2f", budget)
	if o.agent != "claude" {
		budgetNote = "no USD cap (cost reported as unknown); timeout is the limit"
	}
	p.say(cell, fmt.Sprintf("start · %s · %s · model %s · %s", branch, o.agent, modelNote, budgetNote))

	res, timedOut, err := runAgent(o.agent, bin, args, worktree, time.Duration(seconds*float64(time.Second)), p, cell)
	if err != nil {
		return outcome("not_started", map[string]any{"error": fmt.Sprintf("could not launch %s: %v", bin, err)})
	}
	if res.notStarted {
		p.say(cell, "rejected before starting")
		return outcome("not_started", map[string]any{"error": fmt.Sprintf("%s rejected the run before starting: %s", o.agent, firstLine(res.text))})
	}
	report := map[string]any{}
	usedModel := res.model
	if usedModel == "" {
		usedModel = o.model
	}
	if res.costUSD != nil {
		report["cost_usd"] = math.Round(*res.costUSD*1e6) / 1e6
	}
	if u := usageFor(res.usage, usedModel); u != nil {
		report["usage"] = u
	}
	if usedModel != "" {
		report["model"] = usedModel
	}
	settle := func(o string, kv ...any) map[string]any {
		out := outcome(o, report)
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	costNote := "cost unknown"
	if res.costUSD != nil {
		costNote = fmt.Sprintf("$%.4f", *res.costUSD)
	}

	if timedOut || !res.done {
		// The agent ran and may have changed files; never claim a clean outcome.
		p.say(cell, "timed out or ended without a result")
		return settle("uncertain", "error", "agent timed out or ended without a result")
	}
	output, commits, err := collect(worktree, cell, branch, baseSHA, res.text, p)
	if err != nil { // the agent ran, so its effects are unconfirmed
		return settle("uncertain", "error", "agent finished but git failed: "+err.Error())
	}
	if res.isError {
		p.say(cell, "failed · "+costNote)
		text := strings.TrimSpace(res.text)
		if text == "" {
			text = "unknown"
		}
		if limited := limitError(text); limited != "" {
			return settle("settled", "error", limited)
		}
		first := strings.SplitN(text, "\n", 2)[0]
		if len(first) > 300 {
			first = first[:300]
		}
		return settle("settled", "error", "agent error: "+first)
	}
	if commits == 0 {
		p.say(cell, "no changes · "+costNote)
		return settle("settled", "error", "agent finished without changing anything")
	}
	if verify := strings.TrimSpace(req.Context["verify_"+strings.ReplaceAll(cell, "-", "_")]); verify != "" {
		p.say(cell, "verify: "+verify)
		code, tail, timedOut := runVerify(verify, worktree, time.Duration(math.Max(60, seconds)*float64(time.Second)))
		if timedOut {
			return settle("uncertain", "error", fmt.Sprintf("verification `%s` timed out", verify))
		}
		if code != 0 {
			p.say(cell, fmt.Sprintf("verify FAILED (exit %d)", code))
			return settle("settled", "error", fmt.Sprintf("verification failed (exit %d): %s", code, tail))
		}
		p.say(cell, "verify passed")
		output["summary"] += fmt.Sprintf("\n\nVerification `%s` passed.", verify)
	}
	wanted := req.Task.Requires
	if len(wanted) == 0 {
		wanted = outputFields
	}
	selected := map[string]string{}
	for _, k := range wanted {
		if v, ok := output[k]; ok {
			selected[k] = v
		} else {
			selected[k] = output["summary"]
		}
	}
	p.say(cell, fmt.Sprintf("done · %d commit(s) · %s · %s", commits, output["diffstat"], costNote))
	return settle("settled", "output", selected)
}

func setupWorktree(o workerOptions, branch, worktree, cell string) (baseSHA, previous string, err error) {
	if baseSHA, err = git(o.repo, "rev-parse", "--verify", o.base+"^{commit}"); err != nil {
		return "", "", err
	}
	if branchExists(o.repo, branch) {
		if err = retireEarlierAttempts(o.repo, branch); err != nil {
			return "", "", err
		}
		if _, err = git(o.repo, "worktree", "add", worktree, branch); err != nil { // a retry continues the cell's branch
			return "", "", err
		}
		previous, err = previousWork(o.repo, baseSHA, o.base, branch, "")
		return baseSHA, previous, err
	}
	start, earlier := baseSHA, ""
	if o.continueFrom != "" {
		from := o.continueFrom
		if len(from) > 8 {
			from = from[:8]
		}
		earlier = "irrigator/" + from + "/" + cell
		if earlier != branch && branchExists(o.repo, earlier) {
			if start, err = git(o.repo, "rev-parse", earlier); err != nil { // build on an earlier run's work
				return "", "", err
			}
		} else {
			earlier = ""
		}
	}
	if _, err = git(o.repo, "worktree", "add", "-b", branch, worktree, start); err != nil {
		return "", "", err
	}
	if earlier == "" {
		return baseSHA, "", nil
	}
	previous, err = previousWork(o.repo, baseSHA, o.base, branch, earlier)
	return baseSHA, previous, err
}

func runAgent(agent, bin string, args []string, dir string, limit time.Duration, p *progress, cell string) (result, bool, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Stderr = p.out
	ownGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result{}, false, err
	}
	if err := cmd.Start(); err != nil {
		return result{}, false, err
	}
	var timedOut atomic.Bool
	timer := time.AfterFunc(limit, func() {
		timedOut.Store(true)
		killGroup(cmd)
	})
	var res result
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if line := parseLine(agent, sc.Bytes(), &res, dir); line != "" {
			p.say(cell, line)
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	_ = cmd.Wait()
	timer.Stop()
	return res, timedOut.Load(), nil
}

// collect commits leftover changes and describes the branch.
func collect(worktree, cell, branch, baseSHA, summary string, p *progress) (map[string]string, int, error) {
	status, err := git(worktree, "status", "--porcelain")
	if err != nil {
		return nil, 0, err
	}
	if status != "" {
		if _, err := git(worktree, "add", "-A"); err != nil {
			return nil, 0, err
		}
		if _, err := git(worktree, "commit", "-q", "-m", "irrigator cell "+cell+": uncommitted agent changes"); err != nil {
			return nil, 0, err
		}
		p.say(cell, "committed leftover changes")
	}
	count, err := git(worktree, "rev-list", "--count", baseSHA+"..HEAD")
	if err != nil {
		return nil, 0, err
	}
	commits, _ := strconv.Atoi(count)
	head, err := git(worktree, "rev-parse", "HEAD")
	if err != nil {
		return nil, 0, err
	}
	diffstat, err := git(worktree, "diff", "--shortstat", baseSHA+"...HEAD")
	if err != nil {
		return nil, 0, err
	}
	if diffstat == "" {
		diffstat = "no changes"
	}
	summary = strings.TrimSpace(summary)
	if len(summary) > 4000 {
		summary = summary[:4000]
	}
	if summary == "" {
		summary = "(no summary)"
	}
	return map[string]string{"branch": branch, "commit": head, "diffstat": diffstat, "summary": summary}, commits, nil
}

func runVerify(command, dir string, limit time.Duration) (code int, tail string, timedOut bool) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	ownGroup(cmd)
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	err := cmd.Run()
	if ctx.Err() != nil {
		return 0, "", true
	}
	t := strings.TrimSpace(out.String())
	if len(t) > 1500 {
		t = t[len(t)-1500:]
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), t, false
	}
	if err != nil {
		return 127, err.Error(), false
	}
	return 0, t, false
}

func promptFor(req model.WorkRequest, worktree, branch, base, previous string) string {
	parts := []string{req.Task.Prompt}
	ctx := map[string]string{}
	for k, v := range req.Context {
		ctx[k] = v
	}
	if previous != "" {
		ctx["previous_attempts"] = previous
	}
	for _, name := range sortedKeys(ctx) {
		parts = append(parts, "\n## Context: "+name+"\n"+ctx[name])
	}
	deps := make([]string, 0, len(req.Dependencies))
	for d := range req.Dependencies {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	for _, d := range deps {
		raw, _ := json.MarshalIndent(req.Dependencies[d], "", "  ")
		parts = append(parts, "\n## Output of dependency "+d+"\n"+string(raw))
	}
	parts = append(parts, fmt.Sprintf(`
## Working rules
- You are in a git worktree at %s on branch %s, created from %s.
  Only change files inside it. Do not push, reset, or switch branches.
- Follow the repository's AGENTS.md and CONTRIBUTING.md if present.
- Keep the change focused on this task. Add or update tests for behavior you change.
- Commit your work with a clear message.
- Never run git push, git reset, git checkout, git switch or rm; delete files with git rm.
- Some commands are not allowed. If one is refused, do not stop: continue with file edits and the
  commands you can run, and name the refused command in your summary.
- Finish with a short summary: what changed, which tests ran and their result, and anything left undone.`, worktree, branch, base))
	return strings.Join(parts, "\n")
}

var (
	limitRE = regexp.MustCompile(`(?i)\b(session|usage|rate) limit\b|\blimit reached\b`)
	resetRE = regexp.MustCompile(`(?i)(\bresets?\b.*|\btry again\b.*)`)
)

// limitError is a plain message when the agent says the account hit a usage
// limit, else "".
func limitError(text string) string {
	line := ""
	for _, l := range strings.Split(text, "\n") {
		if limitRE.MatchString(l) {
			line = strings.TrimSpace(l)
			break
		}
	}
	if line == "" {
		return ""
	}
	when := "reset time not stated"
	if m := resetRE.FindString(line); m != "" {
		when = m
	} else if m := resetRE.FindString(text); m != "" {
		when = m
	}
	when = strings.TrimSpace(when)
	if len(when) > 200 {
		when = when[:200]
	}
	if len(line) > 200 {
		line = line[:200]
	}
	return fmt.Sprintf("account usage limit reached, not a code problem: %s (reset: %s). Resume this cell after the limit resets.", line, when)
}

func firstLine(s string) string {
	s = strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
