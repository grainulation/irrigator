package coder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
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
	agent, model, effort, prompt, worktree, gitCommonDir string
	addDirs                                              []string
	budgetUSD                                            float64
	seconds                                              int
}

// agentArgs returns the executable and arguments for one agent run in the
// worktree (also its working directory).
func agentArgs(l launch) (string, []string, error) {
	switch l.agent {
	case "claude":
		args := []string{"-p", l.prompt, "--model", l.model, "--output-format", "stream-json", "--verbose",
			"--max-budget-usd", fmt.Sprintf("%.2f", l.budgetUSD), "--permission-mode", "acceptEdits", "--setting-sources", ""}
		if l.effort != "" {
			args = append(args, "--effort", l.effort)
		}
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
		if l.effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(l.effort))
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

const maxUsageMessages = 1024

var validMsgID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

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
	obs        *usageObservations
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
	} else {
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil || ev == nil {
			return ""
		}
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
		if msg != nil {
			if rawU, hasUsage := msg["usage"]; hasUsage {
				if r.obs == nil {
					r.obs = newUsageObservations()
				}
				id := str(msg["id"])
				if id == "" {
					id = str(ev["requestId"])
				}
				r.obs.observe(id, rawU, str(msg["model"]))
			}
		}
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
		if c, ok := parseCost(ev["total_cost_usd"]); ok {
			r.costUSD = c
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
		if item != nil {
			if rawU, hasUsage := item["usage"]; hasUsage {
				if r.obs == nil {
					r.obs = newUsageObservations()
				}
				r.obs.observe(str(item["id"]), rawU, r.model)
			}
		}
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
			r.usage = u
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
		if su != nil {
			if rawU, hasUsage := su["usage"]; hasUsage {
				if r.obs == nil {
					r.obs = newUsageObservations()
				}
				id := firstString(su, "message_id", "id")
				if id == "" {
					conv := firstString(su, "conversation_id")
					if conv == "" {
						conv = str(ev["conversation_id"])
					}
					if conv != "" {
						if idx, ok := parseNonNegInt64(su["step_index"]); ok {
							id = fmt.Sprintf("%s:%d", conv, idx)
						} else {
							id = conv
						}
					}
				}
				r.obs.observe(id, rawU, firstString(su, "model"))
			}
		}
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
		turns, hasTurns := res["num_turns"].(float64)
		id, hasID := res["conversation_id"].(string)
		reason := str(res["finished_reason"])
		noTokens := agyRejectionHasZeroUsage(res)
		switch {
		case reason == "print_timeout" || reason == "interrupted":
			// The turn was cut off mid-work: partial effects are possible, so no clean result.
			r.uncertain = true
			r.text = str(res["error"])
			if rawU, hasUsage := res["usage"]; hasUsage {
				if r.obs == nil {
					r.obs = newUsageObservations()
				}
				obsID := firstString(res, "request_id", "conversation_id")
				r.obs.observe(obsID, rawU, firstString(res, "model"))
			}
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
		r.usage, _ = res["usage"].(map[string]any)
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

type usageCounts struct {
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	ThinkingTokens   int64  `json:"thinking_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	Model            string `json:"model,omitempty"`
}

func formatUsage(raw map[string]any, modelName string) (*usageCounts, bool) {
	if raw == nil {
		return nil, false
	}
	type fieldSpec struct {
		aliases []string
		dst     *int64
	}
	var out usageCounts
	specs := []fieldSpec{
		{[]string{"input_tokens"}, &out.InputTokens},
		{[]string{"output_tokens"}, &out.OutputTokens},
		{[]string{"thinking_tokens", "reasoning_output_tokens"}, &out.ThinkingTokens},
		{[]string{"cache_read_input_tokens", "cache_read_tokens", "cached_input_tokens"}, &out.CacheReadTokens},
		{[]string{"cache_creation_input_tokens", "cache_write_tokens"}, &out.CacheWriteTokens},
		{[]string{"total_tokens"}, &out.TotalTokens},
	}
	supplied := false
	for _, spec := range specs {
		var seen []int64
		for _, k := range spec.aliases {
			v, exists := raw[k]
			if !exists {
				continue
			}
			n, ok := parseNonNegInt64(v)
			if !ok {
				return nil, false
			}
			seen = append(seen, n)
		}
		if len(seen) > 0 {
			for _, v := range seen[1:] {
				if v != seen[0] {
					return nil, false
				}
			}
			*spec.dst = seen[0]
			supplied = true
		}
	}
	if !supplied {
		return nil, false
	}
	// Output tokens already include reasoning/thinking tokens; keep thinking_tokens
	// as a separate observation rather than double-counting it in total_tokens,
	// and never inflate input_tokens with cache_read_tokens.
	if out.TotalTokens == 0 {
		sum, ok := checkedAdd(out.InputTokens, out.OutputTokens, out.CacheReadTokens, out.CacheWriteTokens)
		if !ok {
			return nil, false
		}
		out.TotalTokens = sum
	}
	if out.TotalTokens == 0 && out.ThinkingTokens > 0 {
		return nil, false
	}
	m := modelName
	if m == "" {
		if s, ok := raw["model"].(string); ok {
			m = s
		}
	}
	if m != "" && model.ValidUsageModel(m) {
		out.Model = m
	}
	return &out, true
}

func parseNonNegInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		if x < 0 {
			return 0, false
		}
		return int64(x), true
	case int64:
		if x < 0 {
			return 0, false
		}
		return x, true
	case float64:
		// 2^63 (9223372036854775808.0) overflows int64 when cast; reject >= 0x1p63.
		if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 || x >= 0x1p63 || math.Trunc(x) != x {
			return 0, false
		}
		return int64(x), true
	case json.Number:
		s := x.String()
		if s == "" || strings.ContainsAny(s, ".eE+-") {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func checkedAdd(vals ...int64) (int64, bool) {
	var total int64
	for _, v := range vals {
		if v < 0 || total > math.MaxInt64-v {
			return 0, false
		}
		total += v
	}
	return total, true
}

func parseCost(v any) (*float64, bool) {
	if v == nil {
		return nil, false
	}
	var c float64
	switch x := v.(type) {
	case float64:
		c = x
	case json.Number:
		var err error
		c, err = x.Float64()
		if err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 || c > float64(math.MaxInt64) {
		return nil, false
	}
	return &c, true
}

type usageObservationReport struct {
	Basis                string       `json:"basis"`
	Complete             bool         `json:"complete"`
	Aggregation          string       `json:"aggregation"`
	CostBasis            string       `json:"cost_basis,omitempty"`
	CountsUnavailable    string       `json:"counts_unavailable,omitempty"`
	MessagesObserved     int          `json:"messages_observed"`
	InvalidEventsIgnored int          `json:"invalid_events_ignored"`
	Truncated            bool         `json:"truncated"`
	ObservedTokens       *usageCounts `json:"observed_tokens,omitempty"`
}

type usageObservations struct {
	order     []string
	messages  map[string]usageCounts
	invalid   int
	truncated bool
}

func newUsageObservations() *usageObservations {
	return &usageObservations{messages: make(map[string]usageCounts)}
}

func (u *usageObservations) observe(id string, rawUsage any, modelName string) {
	rawMap, ok := rawUsage.(map[string]any)
	if !ok || rawMap == nil || !validMsgID.MatchString(id) {
		u.invalid++
		return
	}
	parsed, ok := formatUsage(rawMap, modelName)
	if !ok {
		u.invalid++
		return
	}
	parsed.Model = ""
	if _, exists := u.messages[id]; !exists {
		if len(u.messages) >= maxUsageMessages {
			u.truncated = true
			return
		}
		u.order = append(u.order, id)
	}
	u.messages[id] = *parsed
}

func (u *usageObservations) report() *usageObservationReport {
	if u == nil || len(u.messages) == 0 {
		return nil
	}
	var sum usageCounts
	for _, id := range u.order {
		m := u.messages[id]
		var ok1, ok2, ok3, ok4, ok5, ok6 bool
		sum.InputTokens, ok1 = checkedAdd(sum.InputTokens, m.InputTokens)
		sum.OutputTokens, ok2 = checkedAdd(sum.OutputTokens, m.OutputTokens)
		sum.ThinkingTokens, ok3 = checkedAdd(sum.ThinkingTokens, m.ThinkingTokens)
		sum.CacheReadTokens, ok4 = checkedAdd(sum.CacheReadTokens, m.CacheReadTokens)
		sum.CacheWriteTokens, ok5 = checkedAdd(sum.CacheWriteTokens, m.CacheWriteTokens)
		sum.TotalTokens, ok6 = checkedAdd(sum.TotalTokens, m.TotalTokens)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
			return &usageObservationReport{
				Basis:                "assistant_stream",
				Complete:             false,
				Aggregation:          "last_valid_snapshot_per_message_id",
				CostBasis:            model.CostBasisUnknown,
				CountsUnavailable:    "overflow",
				MessagesObserved:     len(u.messages),
				InvalidEventsIgnored: u.invalid,
				Truncated:            u.truncated,
			}
		}
	}
	return &usageObservationReport{
		Basis:                "assistant_stream",
		Complete:             false,
		Aggregation:          "last_valid_snapshot_per_message_id",
		CostBasis:            model.CostBasisUnknown,
		MessagesObserved:     len(u.messages),
		InvalidEventsIgnored: u.invalid,
		Truncated:            u.truncated,
		ObservedTokens:       &sum,
	}
}

// usageFor maps an agent's token counts onto the swarm.worker/v1 usage fields.
func usageFor(raw map[string]any, modelName string) map[string]any {
	c, ok := formatUsage(raw, modelName)
	if !ok {
		return nil
	}
	u := map[string]any{
		"input_tokens":       c.InputTokens,
		"output_tokens":      c.OutputTokens,
		"thinking_tokens":    c.ThinkingTokens,
		"cache_read_tokens":  c.CacheReadTokens,
		"cache_write_tokens": c.CacheWriteTokens,
		"total_tokens":       c.TotalTokens,
	}
	if c.Model != "" {
		u["model"] = c.Model
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
