//go:build unix

package coder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

// Fake agents: each writes its argv (one per line) and cwd, does git work by
// FAKE_MODE, then prints that agent's real stream format.
const fakeHead = `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_DIR/argv"
pwd > "$FAKE_DIR/cwd"
`

const fakeWork = `
case "$FAKE_MODE" in
  timeout) sleep 60 ;;
  noresult) exit 1 ;;
esac
case "$FAKE_MODE" in success|leftover) echo hi > hello.txt ;; second) echo two > second.txt ;; esac
case "$FAKE_MODE" in
  success) git add hello.txt && git commit -q -m "add hello" ;;
  second) git add second.txt && git commit -q -m "add second" ;;
esac
TEXT="added hello.txt"
ERR=false
case "$FAKE_MODE" in
  error) TEXT="agent says: boom"; ERR=true ;;
  limit) TEXT="Claude AI usage limit reached. Your session limit resets 3pm (America/New_York)"; ERR=true ;;
esac
`

var fakeAgents = map[string]string{
	"claude": fakeHead + `echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"hello.txt"}}]}}'
if [ -n "$FAKE_CLAUDE_EVENTS" ]; then
  printf '%s\n' "$FAKE_CLAUDE_EVENTS"
fi` + fakeWork + `
USAGE_JSON="${FAKE_CLAUDE_USAGE:-{\"input_tokens\":120,\"output_tokens\":40}}"
echo "{\"type\":\"result\",\"is_error\":$ERR,\"result\":\"$TEXT\",\"total_cost_usd\":0.0123,\"usage\":$USAGE_JSON,\"model\":\"claude-fake\"}"` + "\n",
	"codex": fakeHead + `echo '{"type":"thread.started","thread_id":"t1"}'
echo '{"type":"turn.started"}'
echo '{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"git status"}}'
if [ -n "$FAKE_CODEX_EVENTS" ]; then
  printf '%s\n' "$FAKE_CODEX_EVENTS"
fi` + fakeWork + `
if [ "$ERR" = true ]; then
  echo "{\"type\":\"turn.failed\",\"error\":{\"message\":\"$TEXT\"}}"
else
  echo "{\"type\":\"item.completed\",\"item\":{\"id\":\"i2\",\"type\":\"agent_message\",\"text\":\"$TEXT\"}}"
  echo '{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":5,"output_tokens":40,"reasoning_output_tokens":3}}'
fi
`,
	"agy": fakeHead + `if [ -n "$FAKE_AGY_STREAM" ]; then
  printf '%s\n' "$FAKE_AGY_STREAM"
  exit 1
fi
if [ "$FAKE_MODE" = agybadmodel ]; then
  echo '{"event":"result","result":{"conversation_id":"","status":"ERROR","response":"","error":"invalid model selection (--model \"x\"): model x is not recognized","num_turns":0,"usage":{"input_tokens":0,"output_tokens":0},"finished_reason":"error"}}'
  exit 1
fi
echo '{"event":"init","conversation_id":"c1","init":{"cwd":"x"}}'
echo '{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"write_file","tool_info":{"parameters":{"Path":"hello.txt"}}}}'
echo '{"event":"step_update","step_update":{"step_type":"subagent","state":"DONE","tool_name":"invoke_subagent","subagent_info":{"subagents":[{"type_name":"research","role":"Directory Explorer","conversation_id":"c2"}]}}}'
if [ "$FAKE_MODE" = agytimeout ]; then
  echo hi > hello.txt && git add hello.txt && git commit -q -m "partial"
  echo '{"event":"result","result":{"conversation_id":"c1","status":"ERROR","response":"","error":"print timeout after 1s with the turn still in progress; the response is partial","num_turns":1,"usage":{"input_tokens":5,"output_tokens":1},"finished_reason":"print_timeout"}}'
  exit 3
fi` + fakeWork + `
STATUS=SUCCESS; [ "$ERR" = true ] && STATUS=ERROR
echo "{\"event\":\"result\",\"result\":{\"conversation_id\":\"c1\",\"num_turns\":1,\"status\":\"$STATUS\",\"response\":\"$TEXT\",\"usage\":{\"input_tokens\":120,\"output_tokens\":40,\"thinking_tokens\":3,\"total_tokens\":163}}}"
`,
}

type fixture struct {
	t                *testing.T
	root, repo, fake string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, repo: filepath.Join(root, "repo"), fake: filepath.Join(root, "fake")}
	bin := filepath.Join(root, "bin")
	for _, d := range []string{bin, f.fake} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range fakeAgents {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DIR", f.fake)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "T")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	f.git(root, "init", "-q", "-b", "main", f.repo)
	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "add", "-A")
	f.git(f.repo, "commit", "-q", "-m", "base")
	return f
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type call struct {
	agent, mode, base, kind, runID, continueFrom string
	attempt                                      int
	timeoutMS                                    int64
	requires, addDirs, seedIgnored               []string
	context                                      map[string]string
	cell                                         string
}

func (f *fixture) run(c call) map[string]any {
	f.t.Helper()
	if c.agent == "" {
		c.agent = "claude"
	}
	defaults := map[*string]string{&c.base: "main", &c.kind: "task", &c.runID: "0123456789abcdef", &c.cell: "cell1"}
	for p, v := range defaults {
		if *p == "" {
			*p = v
		}
	}
	if c.attempt == 0 {
		c.attempt = 1
	}
	if c.timeoutMS == 0 {
		c.timeoutMS = 1800000
	}
	f.t.Setenv("FAKE_MODE", c.mode)
	ctx := map[string]string{"notes": "be brief"}
	for k, v := range c.context {
		ctx[k] = v
	}
	req := model.WorkRequest{Protocol: protocol, Kind: c.kind, RequestID: "req-1", OperationID: "op-1", RunID: c.runID,
		Attempt: c.attempt, TimeoutMS: c.timeoutMS, MaxCostUSD: 0.5, Context: ctx,
		Task: model.Task{ID: c.cell, Prompt: "Add hello.txt", Requires: c.requires}}
	raw, _ := json.Marshal(req)
	args := []string{"--agent", c.agent, "--repo", f.repo, "--base", c.base, "--worktrees", filepath.Join(f.root, "wt"), "--no-tty"}
	for _, d := range c.addDirs {
		args = append(args, "--add-dir", d)
	}
	for _, s := range c.seedIgnored {
		args = append(args, "--seed-ignored", s)
	}
	if c.continueFrom != "" {
		args = append(args, "--continue-from", c.continueFrom)
	}
	var out, errOut bytes.Buffer
	if code, err := runWorker(args, bytes.NewReader(raw), &out, &errOut); code != 0 || err != nil {
		f.t.Fatalf("worker exit %d: %v\n%s", code, err, errOut.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		f.t.Fatalf("response %q: %v", out.String(), err)
	}
	if resp["protocol"] != protocol || resp["request_id"] != "req-1" {
		f.t.Fatalf("bad envelope %v", resp)
	}
	return resp
}

func (f *fixture) argv() []string {
	raw, err := os.ReadFile(filepath.Join(f.fake, "argv"))
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func (f *fixture) cwd() string {
	raw, _ := os.ReadFile(filepath.Join(f.fake, "cwd"))
	return strings.TrimSpace(string(raw))
}

func output(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	out, ok := resp["output"].(map[string]any)
	if !ok {
		t.Fatalf("no output: %v", resp)
	}
	return out
}

func hasArg(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && (value == "" || argv[i+1] == value) {
			return true
		}
	}
	return false
}

func TestSuccessLandsABranchForEveryAgent(t *testing.T) {
	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) {
			f := newFixture(t)
			resp := f.run(call{agent: agent, mode: "success"})
			if resp["outcome"] != "settled" || resp["error"] != nil {
				t.Fatalf("%v", resp)
			}
			out := output(t, resp)
			wt := filepath.Join(f.root, "wt", "01234567", "cell1-a1")
			if out["branch"] != "irrigator/01234567/cell1" || out["summary"] != "added hello.txt" || !strings.Contains(out["diffstat"].(string), "1 file changed") {
				t.Fatalf("%v", out)
			}
			if f.cwd() != wt || f.git(f.repo, "rev-parse", "irrigator/01234567/cell1") != out["commit"] {
				t.Fatalf("cwd %s, commit %v", f.cwd(), out["commit"])
			}
			usage, _ := resp["usage"].(map[string]any)
			if usage["input_tokens"] != float64(120) || usage["output_tokens"] != float64(40) {
				t.Fatalf("usage %v", resp["usage"])
			}
			_, hasCost := resp["cost_usd"]
			if hasCost != (agent == "claude") {
				t.Fatalf("cost_usd present=%v for %s: unknown cost must be omitted, never 0", hasCost, agent)
			}
			argv := f.argv()
			switch agent {
			case "claude":
				if !hasArg(argv, "--model", "sonnet") || !hasArg(argv, "--max-budget-usd", "0.50") || !hasArg(argv, "--setting-sources", "") ||
					!strings.Contains(strings.Join(argv, " "), "Bash(git push:*)") {
					t.Fatalf("claude argv %q", argv)
				}
			case "codex":
				common := f.git(f.repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
				if argv[0] != "exec" || !hasArg(argv, "--sandbox", "workspace-write") || !hasArg(argv, "--cd", wt) || !hasArg(argv, "--add-dir", common) || hasArg(argv, "--model", "") {
					t.Fatalf("codex argv %q", argv)
				}
			case "agy":
				var settings map[string]any
				for i := range argv {
					if argv[i] == "--settings" {
						_ = json.Unmarshal([]byte(argv[i+1]), &settings)
					}
				}
				perms, _ := settings["permissions"].(map[string]any)
				deny, _ := json.Marshal(perms["deny"])
				if settings["toolPermission"] != "always-proceed" || settings["allowNonWorkspaceAccess"] != false ||
					!strings.Contains(string(deny), "command(git push)") || !hasArg(argv, "--mode", "accept-edits") {
					t.Fatalf("agy argv %q", argv)
				}
			}
		})
	}
}

func TestOutcomesForEveryAgent(t *testing.T) {
	for _, agent := range agents {
		t.Run(agent, func(t *testing.T) {
			f := newFixture(t)
			if r := f.run(call{agent: agent, mode: "leftover", cell: "left"}); r["outcome"] != "settled" ||
				!strings.Contains(f.git(f.repo, "log", "-1", "--format=%s", "irrigator/01234567/left"), "uncommitted agent changes") {
				t.Fatalf("leftover: %v", r)
			}
			if r := f.run(call{agent: agent, mode: "error", cell: "err"}); r["outcome"] != "settled" || r["error"] != "agent error: agent says: boom" {
				t.Fatalf("error: %v", r)
			}
			r := f.run(call{agent: agent, mode: "limit", cell: "lim"})
			if msg, _ := r["error"].(string); r["outcome"] != "settled" || !strings.Contains(msg, "account usage limit reached, not a code problem") || !strings.Contains(msg, "resets 3pm") {
				t.Fatalf("limit: %v", r)
			}
			if r := f.run(call{agent: agent, mode: "noresult", cell: "nores"}); r["outcome"] != "uncertain" {
				t.Fatalf("noresult: %v", r)
			}
			if r := f.run(call{agent: agent, mode: "nochange", cell: "none"}); r["outcome"] != "settled" || r["error"] != "agent finished without changing anything" {
				t.Fatalf("no change: %v", r)
			}
		})
	}
}

func TestAgyOwnTimeoutIsUncertainAndBadModelIsNotStarted(t *testing.T) {
	f := newFixture(t)
	if r := f.run(call{agent: "agy", mode: "agytimeout", cell: "slow"}); r["outcome"] != "uncertain" {
		t.Fatalf("agy print timeout must be uncertain (partial work is possible): %v", r)
	}
	r := f.run(call{agent: "agy", mode: "agybadmodel", cell: "bad"})
	if msg, _ := r["error"].(string); r["outcome"] != "not_started" || !strings.Contains(msg, "invalid model selection") {
		t.Fatalf("agy rejecting the model must be not_started: %v", r)
	}
	rejected := `{"event":"result","result":{"conversation_id":"","status":"ERROR","error":"bad model","num_turns":0,"usage":{"input_tokens":0}}}`
	for name, lines := range map[string][]string{
		"session started before error": {`{"event":"init","conversation_id":"c1"}`, rejected},
		"event after rejection":        {rejected, `{"event":"init","conversation_id":"c1"}`},
		"print timeout zero turns":     {strings.Replace(rejected, `"num_turns":0`, `"num_turns":0,"finished_reason":"print_timeout"`, 1)},
		"missing num_turns":            {strings.Replace(rejected, `"num_turns":0,`, "", 1)},
		"nonzero tokens":               {strings.Replace(rejected, `"input_tokens":0`, `"input_tokens":1`, 1)},
	} {
		var res result
		for _, line := range lines {
			parseLine("agy", []byte(line), &res, f.repo)
		}
		if res.notStarted {
			t.Fatalf("%s: classified as notStarted", name)
		}
	}
	var out, errOut bytes.Buffer
	raw, _ := json.Marshal(model.WorkRequest{Protocol: protocol, Kind: "task", RequestID: "req-1", RunID: "0123456789abcdef", Attempt: 1, TimeoutMS: 1800000,
		Task: model.Task{ID: "sub", Prompt: "p"}})
	t.Setenv("FAKE_MODE", "success")
	if _, err := runWorker([]string{"--agent", "agy", "--repo", f.repo, "--worktrees", filepath.Join(f.root, "wt"), "--no-tty"}, bytes.NewReader(raw), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "▸ subagent started: Directory Explorer") {
		t.Fatalf("no subagent progress line:\n%s", errOut.String())
	}
}

func TestTimeoutIsUncertainAndStopsTheAgent(t *testing.T) {
	f := newFixture(t)
	start := time.Now()
	r := f.run(call{mode: "timeout", timeoutMS: 31000}) // 1s after the worker's 30s margin
	if r["outcome"] != "uncertain" || time.Since(start) > 20*time.Second {
		t.Fatalf("%v after %v", r, time.Since(start))
	}
}

func TestNotStartedCases(t *testing.T) {
	f := newFixture(t)
	if r := f.run(call{mode: "success", base: "no-such-branch"}); r["outcome"] != "not_started" || !strings.HasPrefix(r["error"].(string), "worktree setup failed") {
		t.Fatalf("setup: %v", r)
	}
	if r := f.run(call{mode: "success", kind: "validator", cell: "v"}); r["outcome"] != "not_started" {
		t.Fatalf("validator: %v", r)
	}
	if r := f.run(call{agent: "gemini", mode: "success", cell: "g"}); r["outcome"] != "not_started" || !strings.Contains(r["error"].(string), "unknown agent") {
		t.Fatalf("unknown agent: %v", r)
	}
	t.Setenv("PATH", "/usr/bin:/bin") // git only, no agents
	if r := f.run(call{mode: "success", cell: "missing"}); r["outcome"] != "not_started" || !strings.Contains(r["error"].(string), "could not launch claude") {
		t.Fatalf("missing binary: %v", r)
	}
}

func TestRequiresSelectsOutputFields(t *testing.T) {
	f := newFixture(t)
	out := output(t, f.run(call{mode: "success", requires: []string{"branch", "answer"}}))
	if len(out) != 2 || out["branch"] != "irrigator/01234567/cell1" || out["answer"] != "added hello.txt" {
		t.Fatalf("%v", out)
	}
}

func TestRetryContinuesBranchAndRefusesDirtyWorktree(t *testing.T) {
	f := newFixture(t)
	f.run(call{mode: "success"})
	first := filepath.Join(f.root, "wt", "01234567", "cell1-a1")
	r := f.run(call{mode: "second", attempt: 2})
	if r["outcome"] != "settled" || !strings.Contains(strings.Join(f.argv(), "\n"), "## Context: previous_attempts") {
		t.Fatalf("retry: %v", r)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("earlier clean worktree not retired: %v", err)
	}
	if n := f.git(f.repo, "rev-list", "--count", "main..irrigator/01234567/cell1"); n != "2" {
		t.Fatalf("branch commits %s", n)
	}
	second := filepath.Join(f.root, "wt", "01234567", "cell1-a2")
	if err := os.WriteFile(filepath.Join(second, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r = f.run(call{mode: "success", attempt: 3})
	if r["outcome"] != "not_started" || !strings.Contains(r["error"].(string), "retry refused") || !strings.Contains(r["error"].(string), second) {
		t.Fatalf("dirty retry: %v", r)
	}
}

func TestRetryWithoutCommitsHasNoPreviousAttempts(t *testing.T) {
	f := newFixture(t)
	f.run(call{mode: "nochange"})
	f.run(call{mode: "success", attempt: 2})
	if strings.Contains(strings.Join(f.argv(), "\n"), "previous_attempts") {
		t.Fatal("previous_attempts without earlier commits")
	}
}

func TestVerification(t *testing.T) {
	f := newFixture(t)
	r := f.run(call{mode: "success", context: map[string]string{"verify_cell1": "test -f hello.txt && pwd"}})
	if summary := output(t, r)["summary"].(string); !strings.HasSuffix(summary, "Verification `test -f hello.txt && pwd` passed.") {
		t.Fatalf("pass: %v", r)
	}
	r = f.run(call{mode: "success", cell: "bad", context: map[string]string{"verify_bad": "echo nope; exit 3"}})
	if errMsg, _ := r["error"].(string); r["outcome"] != "settled" || !strings.HasPrefix(errMsg, "verification failed (exit 3): nope") || !strings.Contains(errMsg, "bad-a1.diagnostics.json") {
		t.Fatalf("fail: %v", r)
	}
	r = f.run(call{mode: "success", cell: "my-cell", context: map[string]string{"verify_my_cell": "exit 4"}})
	if !strings.HasPrefix(r["error"].(string), "verification failed (exit 4)") {
		t.Fatalf("dash key: %v", r)
	}
	r = f.run(call{mode: "error", cell: "skip", context: map[string]string{"verify_skip": "exit 9"}})
	if r["error"] != "agent error: agent says: boom" {
		t.Fatalf("verify ran after agent error: %v", r)
	}
	r = f.run(call{mode: "success", cell: "prereq", context: map[string]string{"verify_prereq": "echo missing gpu; exit 77"}})
	if errMsg, _ := r["error"].(string); r["outcome"] != "uncertain" || !strings.HasPrefix(errMsg, "verification incomplete or missing host prerequisite (exit 77): missing gpu") || !strings.Contains(errMsg, "prereq-a1.diagnostics.json") {
		t.Fatalf("exit 77 must be uncertain: %v", r)
	}
	r = f.run(call{mode: "success", cell: "dirtyv", context: map[string]string{"verify_dirtyv": "echo leak > leak.tmp"}})
	if errMsg, _ := r["error"].(string); r["outcome"] != "uncertain" || !strings.Contains(errMsg, "left uncommitted or untracked changes in worktree") || !strings.Contains(errMsg, "dirtyv-a1.diagnostics.json") {
		t.Fatalf("dirty verify worktree must be uncertain: %v", r)
	}
}

func TestAddDirAndContinueFrom(t *testing.T) {
	f := newFixture(t)
	ref := filepath.Join(f.root, "ref")
	f.run(call{mode: "success", addDirs: []string{ref}, runID: "aaaaaaaa11"})
	if !hasArg(f.argv(), "--add-dir", ref) {
		t.Fatalf("add-dir: %q", f.argv())
	}
	r := f.run(call{mode: "second", runID: "bbbbbbbb22", continueFrom: "aaaaaaaa11"})
	if r["outcome"] != "settled" || f.git(f.repo, "rev-list", "--count", "main..irrigator/bbbbbbbb/cell1") != "2" ||
		!strings.Contains(strings.Join(f.argv(), "\n"), "was started from irrigator/aaaaaaaa/cell1") {
		t.Fatalf("continue-from: %v", r)
	}
	f.run(call{mode: "success", runID: "cccccccc33", continueFrom: "dddddddd44"})
	if f.git(f.repo, "rev-list", "--count", "main..irrigator/cccccccc/cell1") != "1" {
		t.Fatal("missing earlier branch must start from base")
	}
}

func TestCleanup(t *testing.T) {
	f := newFixture(t)
	f.run(call{mode: "success", cell: "a"})
	f.run(call{mode: "success", cell: "b"})
	f.run(call{mode: "success", cell: "x", runID: "ffffffff99"})
	wt := func(run, cell string) string { return filepath.Join(f.root, "wt", run, cell+"-a1") }
	clean := func(args ...string) (int, string) {
		var out bytes.Buffer
		code, err := cleanupCLI(append([]string{"--repo", f.repo, "--run", "0123456789abcdef"}, args...), &out)
		if err != nil {
			t.Fatal(err)
		}
		return code, out.String()
	}
	if code, out := clean("--dry-run", "--delete-branches"); code != 0 || !strings.Contains(out, "would remove") || !strings.Contains(out, "would delete branch") {
		t.Fatalf("dry run %d %s", code, out)
	}
	if _, err := os.Stat(wt("01234567", "a")); err != nil {
		t.Fatal("dry run removed a worktree")
	}
	if err := os.WriteFile(filepath.Join(wt("01234567", "b"), "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := clean("--delete-branches")
	if code != 1 || !strings.Contains(out, "refuse") || !strings.Contains(out, "kept   branch irrigator/01234567/b (its worktree was not removed)") || !strings.Contains(out, "deleted branch irrigator/01234567/a") {
		t.Fatalf("dirty refusal %d %s", code, out)
	}
	if code, out := clean("--force"); code != 0 || !strings.Contains(out, "removed") || !strings.Contains(out, "kept   branch irrigator/01234567/b") {
		t.Fatalf("force %d %s", code, out)
	}
	if _, err := os.Stat(wt("ffffffff", "x")); err != nil || !branchExists(f.repo, "irrigator/ffffffff/x") {
		t.Fatal("cleanup touched another run")
	}
}

func TestBuildPlan(t *testing.T) {
	plan, err := buildPlan(planOptions{agent: "codex", repo: "/repo", base: "dev", executable: "/bin/irrigator", model: "gpt-x",
		tasks: []string{"docs=Fix typos", "tests=Add a test"}, depends: []string{"tests=docs"}, verify: []string{"tests=go test ./..."},
		addDirs: []string{"/ref"}, seedIgnored: []string{"node_modules,.venv"}, continueFrom: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan)
	for _, want := range []string{`"api_version":"swarm/v1"`, `"max_tasks":2`, `"command":["/bin/irrigator","_code-worker","--agent","codex","--repo","/repo","--base","dev","--model","gpt-x","--add-dir","/ref","--seed-ignored","node_modules,.venv","--continue-from","abc"]`,
		`"env":["USER"]`, `"depends_on":["docs"]`, `"input_fields":{"docs":["summary"]}`, `"context":{"verify_tests":"go test ./..."}`, `"requires":["branch","commit","diffstat","summary"]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("plan lacks %s:\n%s", want, raw)
		}
	}
	for _, bad := range []planOptions{
		{agent: "gemini", tasks: []string{"a=x"}},
		{agent: "claude"},
		{agent: "claude", tasks: []string{"bad id=x"}},
		{agent: "claude", tasks: []string{"a=x", "a=y"}},
		{agent: "claude", tasks: []string{"a=x"}, depends: []string{"a=a"}},
		{agent: "claude", tasks: []string{"a=x"}, verify: []string{"b=true"}},
		{agent: "claude", tasks: []string{"a=x"}, seedIgnored: []string{"../outside"}},
		{agent: "claude", tasks: []string{"a=x"}, seedIgnored: []string{".git/config"}},
	} {
		if _, err := buildPlan(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestCodeFlagsMatchWhatTheWorkerPasses(t *testing.T) {
	if got := MissingFlags("  --model-name X\n  --json\n  --add-dir <DIR>", []string{"--model", "--json", "--add-dir"}); len(got) != 1 || got[0] != "--model" {
		t.Fatalf("whole-flag match: %v", got)
	}
	for _, agent := range agents {
		_, flags, ok := CodeFlags(agent)
		_, args, _ := agentArgs(launch{agent: agent, model: "m", prompt: "p", worktree: "/w", gitCommonDir: "/g", addDirs: []string{"/d"}, budgetUSD: 1, seconds: 60})
		if !ok {
			t.Fatalf("%s has no flag list", agent)
		}
		used := map[string]bool{}
		for _, a := range args {
			if strings.HasPrefix(a, "--") {
				used[a] = true
			}
		}
		listed := map[string]bool{}
		for _, f := range flags {
			listed[f] = true
		}
		for f := range used {
			if !listed[f] && !(agent == "agy" && f == "--print") {
				t.Errorf("%s: worker passes %s but CodeFlags does not check it", agent, f)
			}
		}
	}
}

func TestAgyRejectionEvidenceThroughWorker(t *testing.T) {
	rejected := `{"event":"result","result":{"conversation_id":"","status":"ERROR","error":"bad model","num_turns":0,"usage":{"input_tokens":0}}}`
	cases := []struct{ name, stream, outcome string }{
		{"valid zero usage", rejected, "not_started"},
		{"absent usage", strings.Replace(rejected, `,"usage":{"input_tokens":0}`, "", 1), "not_started"},
		{"session before rejection", `{"event":"init","conversation_id":"c1"}` + "\n" + rejected, "uncertain"},
		{"session after rejection", rejected + "\n" + `{"event":"init","conversation_id":"c1"}`, "uncertain"},
		{"second result after rejection", rejected + "\n" + strings.Replace(rejected, `"conversation_id":""`, `"conversation_id":"c1"`, 1), "uncertain"},
		{"malformed prefix", "broken\n" + rejected, "uncertain"},
		{"duplicate usage", strings.Replace(rejected, `"input_tokens":0`, `"input_tokens":1,"input_tokens":0`, 1), "uncertain"},
	}
	for _, usage := range []string{`null`, `"not-an-object"`, `{}`, `{"input_tokens":"5"}`, `{"input_tokens":-1}`, `{"input_tokens":0.5}`, `{"input_tokens":0,"cache_read_tokens":1}`, `{"input_tokens":0,"cache_write_tokens":"0"}`} {
		cases = append(cases, struct{ name, stream, outcome string }{"invalid usage " + usage, strings.Replace(rejected, `{"input_tokens":0}`, usage, 1), "uncertain"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			t.Setenv("FAKE_AGY_STREAM", tc.stream)
			r := f.run(call{agent: "agy", mode: "success"})
			if r["outcome"] != tc.outcome {
				t.Fatalf("want %s, got %v", tc.outcome, r)
			}
			if _, ok := r["output"]; ok {
				t.Fatalf("rejection produced accepted output: %v", r)
			}
		})
	}
}

func TestVerificationPreservesCommitAndBranchIdentity(t *testing.T) {
	for name, command := range map[string]string{
		"committed change":          "printf changed > hello.txt && git add hello.txt && git commit -q -m verifier-change",
		"same SHA different branch": "git branch verifier-branch && git symbolic-ref HEAD refs/heads/verifier-branch",
		"detached same SHA":         "git update-ref --no-deref HEAD HEAD",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			r := f.run(call{mode: "success", context: map[string]string{"verify_cell1": command}})
			if errMsg, _ := r["error"].(string); r["outcome"] != "uncertain" || !strings.HasPrefix(errMsg, "verification changed commit or branch identity") || !strings.Contains(errMsg, "cell1-a1.diagnostics.json") {
				t.Fatalf("verification identity change accepted: %v", r)
			}
			if _, ok := r["output"]; ok {
				t.Fatalf("identity change produced accepted output: %v", r)
			}
		})
	}
}

func TestFormatUsageNonAdditiveAndValidation(t *testing.T) {
	u := usageFor(map[string]any{
		"input_tokens":                120,
		"output_tokens":               85,
		"thinking_tokens":             40, // already included in output_tokens
		"cache_read_input_tokens":     800,
		"cache_creation_input_tokens": 50,
	}, "claude-sonnet-4-6")
	if u == nil {
		t.Fatal("expected valid usage")
	}
	if u["input_tokens"] != int64(120) || u["cache_read_tokens"] != int64(800) || u["cache_write_tokens"] != int64(50) {
		t.Fatalf("input_tokens must not be inflated with cache_read_tokens: %+v", u)
	}
	// total_tokens = 120 + 85 + 800 + 50 = 1055 (thinking_tokens not added twice)
	if u["total_tokens"] != int64(1055) {
		t.Fatalf("total_tokens = %v, want 1055", u["total_tokens"])
	}

	for name, bad := range map[string]map[string]any{
		"negative":                 {"input_tokens": -1, "output_tokens": 5},
		"float":                    {"input_tokens": 1.5, "output_tokens": 5},
		"bool":                     {"input_tokens": true, "output_tokens": 5},
		"overflow_float":           {"input_tokens": float64(1 << 63)},
		"conflicting_cache_read":   {"cache_read_input_tokens": 100, "cache_read_tokens": 200},
		"conflicting_cache_write":  {"cache_creation_input_tokens": 10, "cache_write_tokens": 20},
		"zero_total_with_thinking": {"input_tokens": 0, "output_tokens": 0, "thinking_tokens": 10},
		"empty":                    {},
		"model_only":               {"model": "claude-sonnet-4-6"},
		"sum_overflow":             {"input_tokens": int64(1<<63 - 1), "output_tokens": 1},
	} {
		if got := usageFor(bad, ""); got != nil {
			t.Errorf("%s: expected nil usage, got %+v", name, got)
		}
	}
}

func TestPartialUsageObservationAndSidecarAcrossAgents(t *testing.T) {
	t.Run("claude interrupted deduplicates message IDs and keeps canonical usage nil", func(t *testing.T) {
		f := newFixture(t)
		events := strings.Join([]string{
			`{"type":"assistant","message":{"id":"msg_01","model":"claude-fake","usage":{"input_tokens":100,"output_tokens":5,"cache_read_input_tokens":500}}}`,
			`{"type":"assistant","message":{"id":"msg_01","model":"claude-fake","usage":{"input_tokens":100,"output_tokens":32,"thinking_tokens":12,"cache_read_input_tokens":500}}}`,
			`{"type":"assistant","message":{"id":"bad id with spaces","usage":{"input_tokens":999,"output_tokens":999}}}`,
			`{"type":"assistant","message":{"id":"msg_01","usage":{"input_tokens":-1}}}`, // invalid update must not erase msg_01
			`{"type":"assistant","message":{"id":"msg_02","model":"claude-fake","usage":{"input_tokens":20,"output_tokens":18,"cache_read_input_tokens":300,"cache_creation_input_tokens":10}}}`,
		}, "\n")
		t.Setenv("FAKE_CLAUDE_EVENTS", events)
		r := f.run(call{agent: "claude", mode: "noresult"})
		if r["outcome"] != "uncertain" {
			t.Fatalf("expected uncertain, got %v", r)
		}
		if _, hasUsage := r["usage"]; hasUsage {
			t.Fatalf("partial observation must not populate canonical usage: %v", r["usage"])
		}
		if _, hasCost := r["cost_usd"]; hasCost {
			t.Fatalf("cost_usd must remain absent when interrupted: %v", r)
		}
		sidecarPath := filepath.Join(f.root, "wt", "01234567", "cell1-a1.diagnostics.json")
		if errMsg, _ := r["error"].(string); !strings.Contains(errMsg, sidecarPath) || !strings.Contains(errMsg, "980 observed tokens") || !strings.Contains(errMsg, "USD unknown") {
			t.Fatalf("unexpected error note: %q", errMsg)
		}
		info, err := os.Stat(sidecarPath)
		if err != nil {
			t.Fatalf("sidecar missing: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("sidecar mode = %o, want 0600", info.Mode().Perm())
		}
		var diag map[string]any
		raw, _ := os.ReadFile(sidecarPath)
		if err := json.Unmarshal(raw, &diag); err != nil {
			t.Fatal(err)
		}
		if diag["schema"] != diagnosticsSchema {
			t.Fatalf("schema = %v", diag["schema"])
		}
		obs, _ := diag["usage_observation"].(map[string]any)
		if obs["basis"] != "assistant_stream" || obs["complete"] != false || obs["aggregation"] != "last_valid_snapshot_per_message_id" ||
			obs["messages_observed"] != float64(2) || obs["invalid_events_ignored"] != float64(2) {
			t.Fatalf("unexpected observation metadata: %v", obs)
		}
		toks, _ := obs["observed_tokens"].(map[string]any)
		if toks["input_tokens"] != float64(120) || toks["output_tokens"] != float64(50) || toks["thinking_tokens"] != float64(12) ||
			toks["cache_read_tokens"] != float64(800) || toks["cache_write_tokens"] != float64(10) || toks["total_tokens"] != float64(980) {
			t.Fatalf("unexpected observed_tokens: %v", toks)
		}
	})

	t.Run("terminal usage supersedes stream observations without sidecar", func(t *testing.T) {
		f := newFixture(t)
		t.Setenv("FAKE_CLAUDE_EVENTS", `{"type":"assistant","message":{"id":"msg_99","usage":{"input_tokens":9999,"output_tokens":99}}}`)
		t.Setenv("FAKE_CLAUDE_USAGE", `{"input_tokens":50,"output_tokens":20,"thinking_tokens":5,"cache_read_input_tokens":100}`)
		r := f.run(call{agent: "claude", mode: "success"})
		if r["outcome"] != "settled" {
			t.Fatalf("expected settled: %v", r)
		}
		usage, _ := r["usage"].(map[string]any)
		if usage["input_tokens"] != float64(50) || usage["total_tokens"] != float64(170) {
			t.Fatalf("unexpected canonical usage: %v", usage)
		}
		sidecarPath := filepath.Join(f.root, "wt", "01234567", "cell1-a1.diagnostics.json")
		if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
			t.Fatalf("sidecar should not exist on clean terminal completion: %v", err)
		}
	})

	t.Run("invalid terminal usage falls back to partial observation", func(t *testing.T) {
		f := newFixture(t)
		t.Setenv("FAKE_CLAUDE_EVENTS", `{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":15,"output_tokens":5}}}`)
		t.Setenv("FAKE_CLAUDE_USAGE", `{"input_tokens":-5,"output_tokens":10}`)
		r := f.run(call{agent: "claude", mode: "success"})
		if _, hasUsage := r["usage"]; hasUsage {
			t.Fatalf("invalid terminal usage must leave canonical usage nil: %v", r["usage"])
		}
		sidecarPath := filepath.Join(f.root, "wt", "01234567", "cell1-a1.diagnostics.json")
		if _, err := os.Stat(sidecarPath); err != nil {
			t.Fatalf("expected diagnostics sidecar when terminal usage is invalid: %v", err)
		}
	})

	t.Run("codex interrupted item usage and agy print_timeout observation", func(t *testing.T) {
		f := newFixture(t)
		t.Setenv("FAKE_CODEX_EVENTS", `{"type":"item.completed","item":{"id":"step_1","type":"agent_message","text":"working","usage":{"input_tokens":40,"cached_input_tokens":10,"output_tokens":12,"reasoning_output_tokens":4}}}`)
		rc := f.run(call{agent: "codex", mode: "noresult", cell: "cdx"})
		if _, hasUsage := rc["usage"]; hasUsage {
			t.Fatalf("codex noresult must not set usage: %v", rc)
		}
		if _, err := os.Stat(filepath.Join(f.root, "wt", "01234567", "cdx-a1.diagnostics.json")); err != nil {
			t.Fatalf("codex sidecar missing: %v", err)
		}

		ra := f.run(call{agent: "agy", mode: "agytimeout", cell: "agyto"})
		if ra["outcome"] != "uncertain" {
			t.Fatalf("expected uncertain, got %v", ra)
		}
		if _, hasUsage := ra["usage"]; hasUsage {
			t.Fatalf("agy timeout must not set canonical usage: %v", ra)
		}
		agySidecar := filepath.Join(f.root, "wt", "01234567", "agyto-a1.diagnostics.json")
		raw, err := os.ReadFile(agySidecar)
		if err != nil {
			t.Fatalf("agy sidecar missing: %v", err)
		}
		if !strings.Contains(string(raw), `"total_tokens": 6`) {
			t.Fatalf("expected agy partial observation in sidecar: %s", raw)
		}
	})
}

func TestUsageObservationsBoundsAndOverflow(t *testing.T) {
	obs := newUsageObservations()
	for i := 0; i < maxUsageMessages+5; i++ {
		obs.observe(fmt.Sprintf("m%d", i), map[string]any{"input_tokens": 1, "output_tokens": 1}, "")
	}
	rep := obs.report()
	if rep == nil || !rep.Truncated || rep.MessagesObserved != maxUsageMessages {
		t.Fatalf("expected truncated at %d, got %+v", maxUsageMessages, rep)
	}
	overflow := newUsageObservations()
	overflow.observe("m1", map[string]any{"input_tokens": int64(1 << 62), "output_tokens": 0}, "")
	overflow.observe("m2", map[string]any{"input_tokens": int64(1 << 62), "output_tokens": 0}, "")
	ovRep := overflow.report()
	if ovRep == nil || ovRep.CountsUnavailable != "overflow" || ovRep.ObservedTokens != nil {
		t.Fatalf("expected overflow report, got %+v", ovRep)
	}
}

func TestVerificationGitStatusBoundsAndSidecarProtection(t *testing.T) {
	t.Run("porcelain-z handles renames and bounds", func(t *testing.T) {
		raw := []byte(" M tracked.go\x00R  new name.go\x00old name.go\x00?? extra.tmp\x00")
		entries, clipped, more := parsePorcelainZ(raw, 2, 64)
		if clipped || !more || len(entries) != 2 {
			t.Fatalf("entries=%+v clipped=%v more=%v", entries, clipped, more)
		}
		if entries[1].Status != "R " || entries[1].Path != "new name.go" || entries[1].OriginalPath != "old name.go" {
			t.Fatalf("rename mismatch: %+v", entries[1])
		}
	})

	t.Run("dirty verify writes before/after diff and retry preserves evidence", func(t *testing.T) {
		f := newFixture(t)
		r := f.run(call{mode: "success", context: map[string]string{"verify_cell1": "echo cov > verifier.log; exit 3"}})
		if r["outcome"] != "settled" {
			t.Fatalf("expected settled, got %v", r)
		}
		errMsg, _ := r["error"].(string)
		if !strings.Contains(errMsg, "verification finished with uncommitted work") {
			t.Fatalf("expected dirty worktree recovery hint in error: %q", errMsg)
		}
		sidecarPath := filepath.Join(f.root, "wt", "01234567", "cell1-a1.diagnostics.json")
		raw, err := os.ReadFile(sidecarPath)
		if err != nil {
			t.Fatalf("missing sidecar: %v", err)
		}
		var diag map[string]any
		if err := json.Unmarshal(raw, &diag); err != nil {
			t.Fatal(err)
		}
		ver, _ := diag["verification"].(map[string]any)
		before, _ := ver["before"].(map[string]any)
		after, _ := ver["after"].(map[string]any)
		if before["dirty"] != false || after["dirty"] != true || after["complete"] != true {
			t.Fatalf("unexpected verification snapshots: %v", ver)
		}
		dirtied, _ := ver["dirtied_entries"].([]any)
		if len(dirtied) != 1 || dirtied[0].(map[string]any)["path"] != "verifier.log" {
			t.Fatalf("unexpected dirtied_entries: %v", ver["dirtied_entries"])
		}
		// Retry must refuse because worktree is dirty, leaving sidecar intact.
		retry := f.run(call{mode: "second", attempt: 2})
		if retry["outcome"] != "not_started" {
			t.Fatalf("expected retry refused: %v", retry)
		}
	})

	t.Run("O_EXCL refuses symlink or existing sidecar overwrite and omits invalid identity", func(t *testing.T) {
		f := newFixture(t)
		wtParent := filepath.Join(f.root, "wt", "badrun01")
		if err := os.MkdirAll(wtParent, 0o755); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(f.root, "sentinel.txt")
		if err := os.WriteFile(sentinel, []byte("keep me\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sidecarPath := filepath.Join(wtParent, "cell1-a1.diagnostics.json")
		if err := os.Symlink(sentinel, sidecarPath); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FAKE_CLAUDE_EVENTS", `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":5,"output_tokens":5}}}`)
		r := f.run(call{runID: "badrun01", mode: "noresult"})
		if errMsg, _ := r["error"].(string); !strings.Contains(errMsg, "existing evidence was not replaced") {
			t.Fatalf("expected O_EXCL warning in error: %q", errMsg)
		}
		got, _ := os.ReadFile(sentinel)
		if string(got) != "keep me\n" {
			t.Fatalf("symlink target was overwritten: %q", got)
		}
		id := buildDiagnosticsIdentity(model.WorkRequest{
			RunID:       "bad\nrun",
			RequestID:   "req-ok",
			OperationID: strings.Repeat("x", 300),
			Attempt:     -2,
			Task:        model.Task{ID: "cell1"},
		})
		if id.RunID != "" || id.OperationID != "" || id.Attempt != 0 || id.RequestID != "req-ok" || len(id.OmittedInvalidFields) != 3 {
			t.Fatalf("unexpected identity sanitization: %+v", id)
		}
	})
}

func TestSeedIgnoredWorktreeSeeding(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, ".gitignore"), []byte("node_modules/\n.cache.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(f.repo, "add", ".gitignore")
	f.git(f.repo, "commit", "-q", "-m", "add gitignore")

	// Create gitignored directory + relative symlink + ignored file + unignored file.
	nm := filepath.Join(f.repo, "node_modules", "pkg")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "index.js"), []byte("module.exports = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, ".cache.bin"), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "unignored.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Valid seeding via --seed-ignored + missing optional entry + context seed_ignored keeps git status clean.
	r := f.run(call{
		mode:        "success",
		seedIgnored: []string{"node_modules,missing_optional"},
		context:     map[string]string{"seed_ignored": ".cache.bin"},
	})
	if r["outcome"] != "settled" {
		t.Fatalf("seeded run failed: %v", r)
	}
	wt := filepath.Join(f.root, "wt", "01234567", "cell1-a1")
	if data, err := os.ReadFile(filepath.Join(wt, "node_modules", "pkg", "index.js")); err != nil || string(data) != "module.exports = 1;\n" {
		t.Fatalf("seeded directory file missing: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(wt, ".cache.bin")); err != nil || string(data) != "cached" {
		t.Fatalf("seeded file missing: %q %v", data, err)
	}
	if status := f.git(wt, "status", "--porcelain"); status != "" {
		t.Fatalf("seeded worktree has dirty git status: %q", status)
	}

	// Refusal cases: tracked file, unignored file, traversal, .git.
	for name, badSeed := range map[string]string{
		"tracked":   "README.md",
		"unignored": "unignored.txt",
		"traversal": "../outside",
		"dotgit":    ".git/config",
	} {
		badResp := f.run(call{mode: "success", cell: "bad-" + name, seedIgnored: []string{badSeed}})
		if badResp["outcome"] != "not_started" || !strings.Contains(badResp["error"].(string), "worktree seeding failed") {
			t.Fatalf("%s: expected not_started seeding failure, got %v", name, badResp)
		}
	}
}
