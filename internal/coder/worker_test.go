//go:build unix

package coder

import (
	"bytes"
	"encoding/json"
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
	"claude": fakeHead + `echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"hello.txt"}}]}}'` + fakeWork +
		`echo "{\"type\":\"result\",\"is_error\":$ERR,\"result\":\"$TEXT\",\"total_cost_usd\":0.0123,\"usage\":{\"input_tokens\":120,\"output_tokens\":40},\"model\":\"claude-fake\"}"` + "\n",
	"codex": fakeHead + `echo '{"type":"thread.started","thread_id":"t1"}'
echo '{"type":"turn.started"}'
echo '{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"git status"}}'` + fakeWork + `
if [ "$ERR" = true ]; then
  echo "{\"type\":\"turn.failed\",\"error\":{\"message\":\"$TEXT\"}}"
else
  echo "{\"type\":\"item.completed\",\"item\":{\"id\":\"i2\",\"type\":\"agent_message\",\"text\":\"$TEXT\"}}"
  echo '{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":5,"output_tokens":40,"reasoning_output_tokens":3}}'
fi
`,
	"agy": fakeHead + `echo '{"event":"init","init":{"cwd":"x"}}'
echo '{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"write_file","tool_info":{"parameters":{"Path":"hello.txt"}}}}'` + fakeWork + `
STATUS=SUCCESS; [ "$ERR" = true ] && STATUS=ERROR
echo "{\"event\":\"result\",\"result\":{\"status\":\"$STATUS\",\"response\":\"$TEXT\",\"usage\":{\"input_tokens\":120,\"output_tokens\":40,\"thinking_tokens\":3,\"total_tokens\":163}}}"
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
	requires, addDirs                            []string
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
	if r["outcome"] != "settled" || r["error"] != "verification failed (exit 3): nope" {
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
		addDirs: []string{"/ref"}, continueFrom: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan)
	for _, want := range []string{`"api_version":"swarm/v1"`, `"max_tasks":2`, `"command":["/bin/irrigator","_code-worker","--agent","codex","--repo","/repo","--base","dev","--model","gpt-x","--add-dir","/ref","--continue-from","abc"]`,
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
	} {
		if _, err := buildPlan(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
