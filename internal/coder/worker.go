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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
	executionlaunch "github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

const protocol = "swarm.worker/v1"

// outputFields are the outputs every coding cell produces.
var outputFields = []string{"branch", "commit", "diffstat", "summary"}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type workerOptions struct {
	agent, repo, base, model, effort, worktrees, continueFrom string
	addDirs, seedIgnored                                      multi
	noTTY                                                     bool
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
	// Retain the hidden worker default for already admitted legacy commands.
	// New CodeCLI plans always record an explicitly resolved --agent.
	fs.StringVar(&o.agent, "agent", "claude", "coding agent recorded by the plan; legacy default claude")
	fs.StringVar(&o.repo, "repo", "", "path of the git repository")
	fs.StringVar(&o.base, "base", "main", "branch or commit new cell branches start from")
	fs.StringVar(&o.model, "model", "", "agent model (default: sonnet for claude, the agent's own default otherwise)")
	fs.StringVar(&o.effort, "effort", "", "explicit validated reasoning effort")
	fs.StringVar(&o.worktrees, "worktrees", "", "worktree root (default: irrigator-worktrees beside the repo)")
	fs.Var(&o.addDirs, "add-dir", "extra directory the agent may read, outside the worktree (repeatable)")
	fs.Var(&o.seedIgnored, "seed-ignored", "comma-separated gitignored relative paths to seed into the worktree")
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
	if o.effort != "" && !executionlaunch.ExecutionEffortSupported(o.agent, o.effort) {
		return outcome("not_started", map[string]any{"error": "unsupported coding worker effort; no fallback applied"})
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
	rawSeeds := append([]string(nil), o.seedIgnored...)
	if ctxSeeds := strings.TrimSpace(req.Context["seed_ignored"]); ctxSeeds != "" {
		rawSeeds = append(rawSeeds, ctxSeeds)
	}
	seedPaths, err := parseSeedList(rawSeeds)
	if err != nil {
		return outcome("not_started", map[string]any{"error": "worktree seeding failed: " + err.Error()})
	}
	if seeded, err := seedIgnoredPaths(o.repo, worktree, seedPaths); err != nil {
		return outcome("not_started", map[string]any{"error": "worktree seeding failed: " + err.Error()})
	} else if len(seeded) > 0 {
		p.say(cell, "seeded "+strings.Join(seeded, ", "))
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
	bin, args, _ := agentArgs(launch{agent: o.agent, model: o.model, effort: o.effort, prompt: promptFor(req, worktree, branch, o.base, previous),
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
	if !timedOut && !res.uncertain && res.notStarted {
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
	if !timedOut && !res.uncertain && res.done {
		if u := usageFor(res.usage, usedModel); u != nil {
			report["usage"] = u
		}
	}
	if usedModel != "" && model.ValidUsageModel(usedModel) {
		report["model"] = usedModel
	}
	var diag coderDiagnostics
	if report["usage"] == nil {
		if partial := res.obs.report(); partial != nil {
			diag.UsageObservation = partial
		}
	}
	settle := func(o string, kv ...any) map[string]any {
		out := outcome(o, report)
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		if diag.UsageObservation != nil || diag.Verification != nil {
			sidecarPath := worktree + ".diagnostics.json"
			var note string
			if err := writeDiagnosticsExclusive(sidecarPath, diag, req); err != nil {
				note = "coder diagnostics sidecar could not be created; existing evidence was not replaced"
			} else {
				note = "coder diagnostics: " + sidecarPath
			}
			if diag.UsageObservation != nil {
				detail := "counts unavailable"
				if diag.UsageObservation.ObservedTokens != nil {
					detail = fmt.Sprintf("%d observed tokens", diag.UsageObservation.ObservedTokens.TotalTokens)
				}
				billing := "USD unknown"
				if res.costUSD != nil {
					billing = "USD reported separately"
				}
				note += fmt.Sprintf("; partial token observation (%s, incomplete; %s)", detail, billing)
			}
			if diag.Verification != nil && diag.Verification.After.Dirty != nil && *diag.Verification.After.Dirty {
				note += "; verification finished with uncommitted work; inspect and explicitly commit or discard it before retry"
			}
			p.say(cell, note)
			if errStr, ok := out["error"].(string); ok && errStr != "" {
				out["error"] = errStr + "; " + note
			} else if sel, ok := out["output"].(map[string]string); ok {
				if s, hasSummary := sel["summary"]; hasSummary {
					sel["summary"] = s + "\n\n" + note
				}
			}
		}
		return out
	}
	costNote := "cost unknown"
	if res.costUSD != nil {
		costNote = fmt.Sprintf("$%.4f", *res.costUSD)
	}

	if res.uncertain {
		return settle("uncertain", "error", "agent reported contradictory or malformed execution evidence")
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
		verifyHead, headErr := git(worktree, "rev-parse", "--verify", "HEAD")
		verifyRef, refErr := git(worktree, "symbolic-ref", "-q", "HEAD")
		if headErr != nil || refErr != nil || verifyHead != output["commit"] || verifyRef != "refs/heads/"+branch {
			return settle("uncertain", "error", "verification input commit or branch identity changed")
		}
		p.say(cell, "verify: "+verify)
		before := captureWorktreeStatus(worktree, statusTimeout)
		sandboxVerify := strings.TrimSpace(req.Context["sandbox_verify"]) == "true"
		code, tail, vTimedOut := runVerify(verify, worktree, time.Duration(math.Max(60, seconds)*float64(time.Second)), sandboxVerify)
		after := captureWorktreeStatus(worktree, statusTimeout)
		dirtied := diffStatusEntries(before.Entries, after.Entries)
		verDiag := &verificationDiagnostics{
			Command:        verify,
			Before:         before,
			After:          after,
			TimedOut:       vTimedOut,
			DirtiedEntries: dirtied,
		}
		if !vTimedOut {
			verDiag.ExitCode = &code
		}
		verDirty := !after.Complete || after.Dirty == nil || *after.Dirty
		afterHead, headErr := git(worktree, "rev-parse", "--verify", "HEAD")
		afterRef, refErr := git(worktree, "symbolic-ref", "-q", "HEAD")
		verIDChanged := headErr != nil || refErr != nil || afterHead != verifyHead || afterRef != verifyRef
		if vTimedOut || code != 0 || !before.Complete || (before.Dirty != nil && *before.Dirty) || verDirty || len(dirtied) > 0 || verIDChanged || diag.UsageObservation != nil {
			diag.Verification = verDiag
		}
		if vTimedOut {
			return settle("uncertain", "error", fmt.Sprintf("verification `%s` timed out", verify))
		}
		if code == 77 {
			p.say(cell, "verify INCOMPLETE (exit 77)")
			return settle("uncertain", "error", fmt.Sprintf("verification incomplete or missing host prerequisite (exit 77): %s", tail))
		}
		if code != 0 {
			p.say(cell, fmt.Sprintf("verify FAILED (exit %d)", code))
			return settle("settled", "error", fmt.Sprintf("verification failed (exit %d): %s", code, tail))
		}
		if verDirty {
			p.say(cell, "verify dirtied worktree")
			return settle("uncertain", "error", fmt.Sprintf("verification `%s` left uncommitted or untracked changes in worktree", verify))
		}
		if verIDChanged {
			return settle("uncertain", "error", "verification changed commit or branch identity")
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

func runVerify(command, dir string, limit time.Duration, sandbox bool) (code int, tail string, timedOut bool) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	bin := "/bin/sh"
	args := []string{"-c", command}
	var envOverrides []string
	if sandbox {
		var attemptTmp string
		if runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("sandbox-exec"); err != nil {
				return 77, "sandbox-verify requested but sandbox-exec is not available", false
			}
			tmpDir, err := os.MkdirTemp("", "irrigator-verify-*")
			if err != nil {
				return 77, "sandbox-verify temp dir setup failed: " + err.Error(), false
			}
			defer os.RemoveAll(tmpDir)
			attemptTmp = tmpDir
		}
		var err error
		bin, args, envOverrides, err = buildSandboxVerifyCommand(runtime.GOOS, command, dir, attemptTmp, exec.LookPath)
		if err != nil {
			return 77, err.Error(), false
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if len(envOverrides) > 0 {
		cmd.Env = applyEnvOverrides(os.Environ(), envOverrides)
	}
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

func buildSandboxVerifyCommand(goos, command, dir, attemptTmp string, lookPath func(string) (string, error)) (bin string, args []string, envOverrides []string, err error) {
	switch goos {
	case "darwin":
		tool, err := lookPath("sandbox-exec")
		if err != nil {
			return "", nil, nil, errors.New("sandbox-verify requested but sandbox-exec is not available")
		}
		resolvedDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return "", nil, nil, fmt.Errorf("sandbox-verify resolve worktree: %w", err)
		}
		resolvedTmp, err := filepath.EvalSymlinks(attemptTmp)
		if err != nil {
			return "", nil, nil, fmt.Errorf("sandbox-verify resolve temp dir: %w", err)
		}
		for _, p := range []string{resolvedDir, resolvedTmp} {
			if strings.ContainsAny(p, "\x00\r\n") {
				return "", nil, nil, errors.New("sandbox-verify path contains invalid control characters")
			}
		}
		profile := fmt.Sprintf(
			"(version 1)\n(allow default)\n(deny network*)\n(deny file-write*)\n(allow file-write*\n  (literal \"/dev/null\")\n  (subpath %s)\n  (subpath %s))\n",
			strconv.Quote(resolvedDir),
			strconv.Quote(resolvedTmp),
		)
		return tool, []string{"-p", profile, "/bin/sh", "-c", command}, []string{"TMPDIR=" + resolvedTmp}, nil
	case "linux":
		tool, err := lookPath("bwrap")
		if err != nil {
			return "", nil, nil, errors.New("sandbox-verify requested but bwrap is not available")
		}
		resolvedDir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return "", nil, nil, fmt.Errorf("sandbox-verify resolve worktree: %w", err)
		}
		return tool, []string{
			"--unshare-net",
			"--ro-bind", "/", "/",
			"--tmpfs", "/tmp",
			"--dev", "/dev",
			"--proc", "/proc",
			"--bind", resolvedDir, resolvedDir,
			"--chdir", resolvedDir,
			"/bin/sh", "-c", command,
		}, []string{"TMPDIR=/tmp"}, nil
	default:
		return "", nil, nil, fmt.Errorf("sandbox-verify requested but OS-native sandbox is not supported on %s", goos)
	}
}

func applyEnvOverrides(base, overrides []string) []string {
	keys := make(map[string]bool, len(overrides))
	for _, ov := range overrides {
		if k, _, ok := strings.Cut(ov, "="); ok {
			keys[k] = true
		}
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		if k, _, ok := strings.Cut(entry, "="); ok && keys[k] {
			continue
		}
		out = append(out, entry)
	}
	return append(out, overrides...)
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
- Never bypass a failing check or permission refusal by weakening tests or verification gates.
- Some commands are not allowed. If one is refused, do not stop: continue with file edits and the
  commands you can run, and name the refused command in your summary.
- Finish with a short summary: what changed, exact test commands and exit codes, and anything left undone.`, worktree, branch, base))
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

const diagnosticsSchema = "irrigator.coder-diagnostics/v1"

type diagnosticsIdentity struct {
	RunID                string   `json:"run_id,omitempty"`
	RequestID            string   `json:"request_id,omitempty"`
	OperationID          string   `json:"operation_id,omitempty"`
	TaskID               string   `json:"task_id,omitempty"`
	Attempt              int      `json:"attempt,omitempty"`
	OmittedInvalidFields []string `json:"omitted_invalid_fields,omitempty"`
}

type verificationDiagnostics struct {
	Command        string         `json:"command"`
	Before         worktreeStatus `json:"before"`
	After          worktreeStatus `json:"after"`
	ExitCode       *int           `json:"exit_code,omitempty"`
	TimedOut       bool           `json:"timed_out,omitempty"`
	DirtiedEntries []statusEntry  `json:"dirtied_entries,omitempty"`
}

type coderDiagnostics struct {
	Schema           string                   `json:"schema"`
	Identity         diagnosticsIdentity      `json:"identity"`
	UsageObservation *usageObservationReport  `json:"usage_observation,omitempty"`
	Verification     *verificationDiagnostics `json:"verification,omitempty"`
}

func isPrintableASCII(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func buildDiagnosticsIdentity(req model.WorkRequest) diagnosticsIdentity {
	var id diagnosticsIdentity
	var omitted []string
	check := func(name, val string, dst *string) {
		if val == "" {
			return
		}
		if isPrintableASCII(val, 256) {
			*dst = val
		} else {
			omitted = append(omitted, name)
		}
	}
	check("run_id", req.RunID, &id.RunID)
	check("request_id", req.RequestID, &id.RequestID)
	check("operation_id", req.OperationID, &id.OperationID)
	check("task_id", req.Task.ID, &id.TaskID)
	if req.Attempt > 0 {
		id.Attempt = req.Attempt
	} else if req.Attempt < 0 {
		omitted = append(omitted, "attempt")
	}
	id.OmittedInvalidFields = omitted
	return id
}

func writeDiagnosticsExclusive(path string, diag coderDiagnostics, req model.WorkRequest) error {
	diag.Schema = diagnosticsSchema
	diag.Identity = buildDiagnosticsIdentity(req)
	raw, err := json.MarshalIndent(diag, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create diagnostics sidecar: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
