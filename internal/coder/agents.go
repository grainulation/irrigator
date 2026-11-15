package coder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/grainulation/irrigator/internal/jsonio"
)

// agents lists the coding agents the built-in worker can run.
var agents = []string{"claude", "codex", "agy"}

// defaultModel is the model used when --model is empty. Codex and agy use
// their own configured default.
func defaultModel(agent string) string {
	if agent == "claude" {
		return "sonnet"
	}
	return ""
}

// Commands the agent may run so it can build and test, and commands it may not.
// A convenience guardrail, not a sandbox: python3, node and npx can still run
// any command. Codex additionally runs inside its own workspace-write sandbox.
var (
	allowedCommands = []string{
		"go", "gofmt", "make", "python3", "pytest", "node", "npm", "npx", "cargo",
		"git status", "git diff", "git show", "git log", "git add", "git commit", "git mv", "git ls-files", "git grep", "git rev-parse",
		"ls", "cat", "head", "tail", "wc", "grep", "find", "diff", "sed", "sort", "uniq", "cut", "tr", "pwd", "echo", "which", "test",
		"mkdir", "cp", "mv", "chmod"}
	deniedCommands = []string{"git push", "git reset", "git checkout", "git switch", "rm"}
)

func claudeAllowedTools() string {
	tools := []string{"Read", "Edit", "Write", "Glob", "Grep", "TodoWrite"}
	for _, c := range allowedCommands {
		tools = append(tools, "Bash("+c+":*)")
	}
	return strings.Join(tools, ",")
}

func claudeDeniedTools() string {
	var tools []string
	for _, c := range deniedCommands {
		tools = append(tools, "Bash("+c+":*)")
	}
	return strings.Join(append(tools, "WebFetch", "WebSearch"), ",")
}

// agySettings are session-only agy settings: deny the dangerous commands (they
// are refused even here, and they replace the user's own allow and deny rules for
// the session) and keep file tools inside the workspace. agy print mode ends the
// turn on any refusal, so an allow list would stop cells on harmless commands;
// the prompt tells every agent which commands are denied so it avoids them.
func agySettings() string {
	deny := []string{}
	for _, c := range deniedCommands {
		deny = append(deny, "command("+c+")")
	}
	raw, _ := json.Marshal(map[string]any{
		"toolPermission":          "always-proceed",
		"allowNonWorkspaceAccess": false,
		"permissions":             map[string]any{"allow": []string{}, "deny": deny},
	})
	return string(raw)
}

// launch holds what one agent run needs.
type launch struct {
	agent, model, prompt, worktree, gitCommonDir string
	addDirs                                      []string
	budgetUSD                                    float64
	seconds                                      int
}

// agentArgs returns the executable and arguments for one agent run in the
// worktree (also its working directory).
func agentArgs(l launch) (string, []string, error) {
	switch l.agent {
	case "claude":
		args := []string{"-p", l.prompt, "--model", l.model, "--output-format", "stream-json", "--verbose",
			"--max-budget-usd", fmt.Sprintf("%.2f", l.budgetUSD), "--permission-mode", "acceptEdits", "--setting-sources", ""}
		for _, d := range l.addDirs {
			args = append(args, "--add-dir", d)
		}
		return "claude", append(args, "--allowedTools", claudeAllowedTools(), "--disallowedTools", claudeDeniedTools()), nil
	case "codex":
		args := []string{"exec", "--json", "--color", "never", "--cd", l.worktree, "--sandbox", "workspace-write",
			"-c", `approval_policy="never"`}
		if l.model != "" {
			args = append(args, "--model", l.model)
		}
		// A worktree's git data lives in the main repository; the sandbox must
		// allow writing it or the agent cannot commit.
		if l.gitCommonDir != "" {
			args = append(args, "--add-dir", l.gitCommonDir)
		}
		for _, d := range l.addDirs {
			args = append(args, "--add-dir", d)
		}
		return "codex", append(args, l.prompt), nil
	case "agy":
		args := []string{"-p", l.prompt, "--output-format", "stream-json", "--mode", "accept-edits",
			"--settings", agySettings(), "--print-timeout", fmt.Sprintf("%ds", l.seconds)}
		if l.model != "" {
			args = append(args, "--model", l.model)
		}
		for _, d := range l.addDirs {
			args = append(args, "--add-dir", d)
		}
		return "agy", args, nil
	}
	return "", nil, fmt.Errorf("unknown agent %q (use %s)", l.agent, strings.Join(agents, ", "))
}

// result is what the worker learned from the agent's output stream.
type result struct {
	lines      int    // non-empty stdout lines seen so far
	done       bool   // a terminal result event arrived
	uncertain  bool   // contradictory or malformed evidence prevents settlement
	notStarted bool   // the agent rejected the run before starting a session
	isError    bool   // the agent reported failure
	text       string // final summary, or the error text
	costUSD    *float64
	usage      map[string]any
	model      string
}

// parseLine reads one stdout line from the agent, updating r, and returns a
// progress line to show (or "").
func parseLine(agent string, line []byte, r *result, worktree string) string {
	if len(bytes.TrimSpace(line)) == 0 {
		return ""
	}
	r.lines++
	if r.lines > 1 {
		if r.notStarted {
			r.uncertain = true
		}
		r.notStarted = false
	}
	var ev map[string]any
	if agent == "agy" {
		decoder := json.NewDecoder(bytes.NewReader(line))
		parsed, err := jsonio.ParseStrictJSON(decoder, 0)
		var ok bool
		ev, ok = parsed.(map[string]any)
		if err != nil || !ok || decoder.Decode(new(any)) != io.EOF {
			r.uncertain = true
			return ""
		}
	} else if json.Unmarshal(line, &ev) != nil {
		return ""
	}
	switch agent {
	case "claude":
		return parseClaude(ev, r, worktree)
	case "codex":
		return parseCodex(ev, r, worktree)
	case "agy":
		return parseAgy(ev, r, worktree)
	}
	return ""
}

func parseClaude(ev map[string]any, r *result, worktree string) string {
	switch str(ev["type"]) {
	case "assistant":
		msg, _ := ev["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		var out []string
		for _, b := range blocks {
			block, _ := b.(map[string]any)
			switch str(block["type"]) {
			case "tool_use":
				in, _ := block["input"].(map[string]any)
				detail := firstString(in, "file_path", "command", "pattern")
				out = append(out, "▸ "+str(block["name"])+" "+relative(detail, worktree))
			case "text":
				if t := strings.TrimSpace(str(block["text"])); t != "" {
					out = append(out, "· "+strings.SplitN(t, "\n", 2)[0])
				}
			}
		}
		return strings.Join(out, "\n")
	case "result":
		r.done = true
		r.isError, _ = ev["is_error"].(bool)
		r.text = str(ev["result"])
		if r.isError && strings.TrimSpace(r.text) == "" {
			r.text = str(ev["subtype"])
		}
		if c, ok := ev["total_cost_usd"].(float64); ok && c >= 0 {
			r.costUSD = &c
		}
		r.usage, _ = ev["usage"].(map[string]any)
		r.model = str(ev["model"])
	}
	return ""
}

func parseCodex(ev map[string]any, r *result, worktree string) string {
	switch str(ev["type"]) {
	case "item.completed":
		item, _ := ev["item"].(map[string]any)
		switch str(item["type"]) {
		case "agent_message":
			r.text = str(item["text"])
			if t := strings.TrimSpace(r.text); t != "" {
				return "· " + strings.SplitN(t, "\n", 2)[0]
			}
		case "command_execution":
			return "▸ run " + relative(str(item["command"]), worktree)
		case "file_change":
			changes, _ := item["changes"].([]any)
			var paths []string
			for _, c := range changes {
				m, _ := c.(map[string]any)
				paths = append(paths, relative(str(m["path"]), worktree))
			}
			return "▸ edit " + strings.Join(paths, " ")
		case "error":
			return "! " + str(item["message"])
		}
	case "turn.completed":
		r.done = true
		if u, ok := ev["usage"].(map[string]any); ok {
			r.usage = map[string]any{"input_tokens": u["input_tokens"], "output_tokens": u["output_tokens"],
				"cache_read_tokens": u["cached_input_tokens"], "thinking_tokens": u["reasoning_output_tokens"]}
		}
	case "turn.failed":
		r.done, r.isError = true, true
		if e, ok := ev["error"].(map[string]any); ok {
			r.text = str(e["message"])
		}
	case "error":
		r.text = str(ev["message"])
	}
	return ""
}

func parseAgy(ev map[string]any, r *result, worktree string) string {
	switch str(ev["event"]) {
	case "step_update":
		su, _ := ev["step_update"].(map[string]any)
		switch str(su["step_type"]) {
		case "subagent": // its own steps run in a separate session; session deny rules still apply there
			if str(su["state"]) != "DONE" {
				return ""
			}
			info, _ := su["subagent_info"].(map[string]any)
			subs, _ := info["subagents"].([]any)
			var roles []string
			for _, s := range subs {
				m, _ := s.(map[string]any)
				roles = append(roles, firstString(m, "role", "type_name"))
			}
			return "▸ subagent started: " + strings.Join(roles, ", ")
		case "system_message":
			return "· message from a subagent or the system"
		}
		if str(su["step_type"]) != "tool" || (str(su["state"]) != "DONE" && str(su["state"]) != "ERROR") {
			return ""
		}
		info, _ := su["tool_info"].(map[string]any)
		params, _ := info["parameters"].(map[string]any)
		line := "▸ " + str(su["tool_name"]) + " " + relative(firstString(params, "CommandLine", "Path", "AbsolutePath", "Query"), worktree)
		if str(su["state"]) == "ERROR" {
			line += " (refused or failed)"
		}
		return line
	case "result":
		res, _ := ev["result"].(map[string]any)
		r.usage, _ = res["usage"].(map[string]any)
		turns, hasTurns := res["num_turns"].(float64)
		id, hasID := res["conversation_id"].(string)
		reason := str(res["finished_reason"])
		noTokens := agyRejectionHasZeroUsage(res)
		switch {
		case reason == "print_timeout" || reason == "interrupted":
			// The turn was cut off mid-work: partial effects are possible, so no clean result.
			r.uncertain = true
			r.text = str(res["error"])
			return ""
		case r.lines == 1 && str(res["status"]) == "ERROR" && hasID && id == "" && hasTurns && turns == 0 && noTokens:
			// Standalone rejection before a session existed (for example an unknown --model): nothing ran.
			r.notStarted, r.text = true, str(res["error"])
			return ""
		case str(res["status"]) == "ERROR" && (!hasID || id == "" || !hasTurns || turns == 0):
			// A claimed rejection contradicted by the stream cannot establish completion.
			r.uncertain = true
			r.text = str(res["error"])
			return ""
		}
		r.done = true
		r.isError = str(res["status"]) != "SUCCESS"
		r.text = str(res["response"])
		if r.isError && strings.TrimSpace(r.text) == "" {
			r.text = firstString(res, "error", "status")
		}
	}
	return ""
}

// agyRejectionHasZeroUsage validates rejection evidence without the lossy
// normalization used for reporting. Older rejections may omit usage entirely.
func agyRejectionHasZeroUsage(res map[string]any) bool {
	value, present := res["usage"]
	if !present {
		return true
	}
	usage, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if _, present := usage["input_tokens"]; !present {
		return false
	}
	for _, key := range []string{"input_tokens", "output_tokens", "thinking_tokens", "cache_read_tokens", "cache_read_input_tokens", "cache_write_tokens", "cache_creation_input_tokens", "total_tokens"} {
		if value, present := usage[key]; present {
			count, ok := value.(float64)
			if !ok || count != 0 {
				return false
			}
		}
	}
	return true
}

// usageFor maps an agent's token counts onto the swarm.worker/v1 usage fields.
func usageFor(raw map[string]any, model string) map[string]any {
	if raw == nil {
		return nil
	}
	n := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := raw[k].(float64); ok && v >= 0 {
				return int64(v)
			}
		}
		return 0
	}
	u := map[string]any{
		"input_tokens":       n("input_tokens"),
		"output_tokens":      n("output_tokens"),
		"thinking_tokens":    n("thinking_tokens"),
		"cache_read_tokens":  n("cache_read_input_tokens", "cache_read_tokens"),
		"cache_write_tokens": n("cache_creation_input_tokens", "cache_write_tokens"),
	}
	total := n("total_tokens")
	if total == 0 {
		for _, k := range []string{"input_tokens", "output_tokens", "thinking_tokens", "cache_read_tokens", "cache_write_tokens"} {
			total += u[k].(int64)
		}
	}
	u["total_tokens"] = total
	if model != "" {
		u["model"] = model
	}
	return u
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := str(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func relative(s, worktree string) string {
	return strings.ReplaceAll(s, filepath.Clean(worktree)+string(filepath.Separator), "")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CodeFlags returns the help command for agent and the flags `irrigator code`
// passes to it, so `irrigator agents` can warn when an agent CLI stops listing
// one. Keep it in step with agentArgs.
func CodeFlags(agent string) (help []string, flags []string, ok bool) {
	switch agent {
	case "claude":
		return []string{"--help"}, []string{"--model", "--output-format", "--verbose", "--max-budget-usd", "--permission-mode",
			"--setting-sources", "--add-dir", "--allowedTools", "--disallowedTools"}, true
	case "codex":
		return []string{"exec", "--help"}, []string{"--json", "--color", "--cd", "--sandbox", "--model", "--add-dir", "--config"}, true
	case "agy":
		return []string{"--help"}, []string{"--print", "--output-format", "--mode", "--settings", "--print-timeout", "--model", "--add-dir"}, true
	}
	return nil, nil, false
}

// MissingFlags returns the flags absent from help text (matched as whole flags).
func MissingFlags(helpText string, flags []string) []string {
	var missing []string
	for _, f := range flags {
		re := regexp.MustCompile(regexp.QuoteMeta(f) + `([^A-Za-z0-9-]|$)`)
		if !re.MatchString(helpText) {
			missing = append(missing, f)
		}
	}
	return missing
}
