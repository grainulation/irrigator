//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/approvals"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/mcpserver"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpConnect(t *testing.T, cfg mcpserver.McpConfig) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcpserver.NewMCPServer(cfg)
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// mcpCall calls a tool and returns the structured output, the text summary and
// the tool error text (empty on success). Protocol-level errors are also
// reported as an error string so callers can assert on either.
func mcpCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, string, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, "", err.Error()
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return nil, "", text
	}
	out, _ := res.StructuredContent.(map[string]any)
	return out, text, ""
}

func mcpMustCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	out, text, errText := mcpCall(t, cs, name, args)
	if errText != "" {
		t.Fatalf("%s failed: %s", name, errText)
	}
	return out, text
}

func mcpToolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestMCPControlToolsAbsentWithoutAllowControl(t *testing.T) {
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir()})
	got := strings.Join(mcpToolNames(t, cs), ",")
	if got != "inspect_cell,list_runs,review_list,run_logs,run_status,validate_plan" {
		t.Fatalf("read-only tool set: %s", got)
	}
	for _, name := range []string{"apply", "pause", "resume", "retry_cell", "decide_approval"} {
		if _, _, errText := mcpCall(t, cs, name, map[string]any{}); errText == "" {
			t.Fatalf("control tool %s is callable without --allow-control", name)
		}
	}
	cs = mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir(), AllowControl: true})
	if got := strings.Join(mcpToolNames(t, cs), ","); got != "apply,decide_approval,inspect_cell,list_runs,pause,resume,retry_cell,review_list,run_logs,run_status,validate_plan" {
		t.Fatalf("control tool set: %s", got)
	}
}

func TestMCPCLIRegisteredAndRejectsPositionals(t *testing.T) {
	if _, ok := commands()["mcp"]; !ok {
		t.Fatal("mcp command is not registered")
	}
	if code, err := mcpserver.McpCLI([]string{"extra"}); code != 2 || err == nil {
		t.Fatalf("positional accepted: %d %v", code, err)
	}
}

func TestMCPListRunsAndStatus(t *testing.T) {
	runs := t.TempDir()
	a := filepath.Join(runs, "alpha")
	if err := checkpoint.Initialize(model.Plan{Name: "t", Tasks: []model.Task{runfixture.Task("a", 0), runfixture.Task("b", 0, "a")}}, a, "mock"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(a, func(s *model.State) error {
		s.Tasks["a"].Status, s.Tasks["a"].Attempt = "verified", 1
		s.Tasks["b"].Status, s.Tasks["b"].Attempt = "failed", 2
		s.Tasks["b"].Error = strings.Repeat("e", 5000)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runs, "not-a-run"), 0o700); err != nil {
		t.Fatal(err)
	}
	cs := mcpConnect(t, mcpserver.McpConfig{RunRoots: []string{runs}, Workspace: t.TempDir()})
	out, _ := mcpMustCall(t, cs, "list_runs", nil)
	list, _ := out["runs"].([]any)
	if len(list) != 1 {
		t.Fatalf("runs: %v", out)
	}
	row := list[0].(map[string]any)
	if row["name"] != "alpha" || row["path"] != a || row["verified"] != float64(1) || row["total"] != float64(2) || row["control"] == "" {
		t.Fatalf("run row: %v", row)
	}
	// A run name resolves under --runs.
	out, text := mcpMustCall(t, cs, "run_status", map[string]any{"run": "alpha"})
	if !strings.Contains(text, "1/2 verified") {
		t.Fatalf("summary: %s", text)
	}
	cells := out["cells"].([]any)
	if len(cells) != 2 {
		t.Fatalf("cells: %v", cells)
	}
	b := cells[1].(map[string]any)
	if b["status"] != "failed" || b["attempts"] != float64(2) || len(b["error"].(string)) > mcpserver.McpErrorCap+100 {
		t.Fatalf("cell b: %v", b)
	}
	// An explicit path works without any --runs root.
	cs = mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir()})
	mcpMustCall(t, cs, "run_status", map[string]any{"run": a})
	if _, _, errText := mcpCall(t, cs, "run_status", map[string]any{"run": filepath.Join(runs, "missing")}); errText == "" {
		t.Fatal("missing run did not fail")
	}
}

func TestMCPRunLogsSinceAndLimit(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		for i := 0; i < 5; i++ {
			s.Event("probe", "a", map[string]int{"i": i})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	total := len(runfixture.Read(t, root).Events)
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir()})
	out, _ := mcpMustCall(t, cs, "run_logs", map[string]any{"run": root, "limit": 2})
	events := out["events"].([]any)
	if len(events) != 2 || out["next_since"] != float64(2) || out["more"] != true || out["total"] != float64(total) {
		t.Fatalf("first page: %v", out)
	}
	first := events[0].(map[string]any)
	if first["seq"] != float64(1) || first["event"].(map[string]any)["kind"] == "" {
		t.Fatalf("event shape: %v", first)
	}
	out, _ = mcpMustCall(t, cs, "run_logs", map[string]any{"run": root, "since": 2})
	if len(out["events"].([]any)) != total-2 || out["more"] != false || out["next_since"] != float64(total) {
		t.Fatalf("second page: %v", out)
	}
	if _, _, errText := mcpCall(t, cs, "run_logs", map[string]any{"run": root, "cell": "nope"}); !strings.Contains(errText, "unknown cell") {
		t.Fatalf("unknown cell: %q", errText)
	}
}

func TestMCPInspectCellTruncatesAndHidesEnvironment(t *testing.T) {
	n := runfixture.Task("a", 0)
	n.MockResult = map[string]string{"answer": "ok"}
	n.Checks = []model.Check{{Kind: "command", Command: []string{"true"}, Env: []string{"API_TOKEN=super-secret-value"}}}
	root := runfixture.New(t, n)
	big := strings.Repeat("x", 3*mcpserver.McpTextCap)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Status, s.Tasks["a"].Attempt = "verified", 1
		s.Tasks["a"].Result = map[string]string{"answer": big}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir()})
	out, text := mcpMustCall(t, cs, "inspect_cell", map[string]any{"run": root, "cell": "a"})
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "super-secret-value") {
		t.Fatal("environment value leaked")
	}
	if !strings.Contains(string(raw), "API_TOKEN") {
		t.Fatalf("environment name missing: %s", raw)
	}
	if len(raw) > 2*mcpserver.McpTextCap+4096 {
		t.Fatalf("result not capped: %d bytes", len(raw))
	}
	truncated, _ := out["truncated"].([]any)
	if len(truncated) == 0 || !strings.Contains(text, "truncated") || !strings.Contains(string(raw), "[truncated:") {
		t.Fatalf("truncation not reported: %v / %s", truncated, text)
	}
	if _, _, errText := mcpCall(t, cs, "inspect_cell", map[string]any{"run": root, "cell": "zzz"}); !strings.Contains(errText, "unknown node") {
		t.Fatalf("unknown cell: %q", errText)
	}
}

func TestMCPReviewList(t *testing.T) {
	ws := changeTestRepo(t)
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: ws})
	out, text := mcpMustCall(t, cs, "review_list", map[string]any{})
	queue := out["queue"].(map[string]any)
	if queue["base"] != "main" || len(queue["changes"].([]any)) != 2 || !strings.Contains(text, "2 change(s)") {
		t.Fatalf("queue: %v / %s", queue, text)
	}
	if _, _, errText := mcpCall(t, cs, "review_list", map[string]any{"workspace": t.TempDir()}); errText == "" {
		t.Fatal("non-repository workspace did not fail")
	}
}

func TestMCPValidatePlan(t *testing.T) {
	dir := t.TempDir()
	good := model.Plan{Name: "t", Tasks: []model.Task{runfixture.Task("a", 0)}}
	raw, _ := json.Marshal(good)
	path := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: dir})
	out, _ := mcpMustCall(t, cs, "validate_plan", map[string]any{"path": "plan.json"})
	if out["valid"] != true || out["tasks"] != float64(1) {
		t.Fatalf("file plan: %v", out)
	}
	out, _ = mcpMustCall(t, cs, "validate_plan", map[string]any{"json": string(raw)})
	if out["valid"] != true {
		t.Fatalf("inline plan: %v", out)
	}
	out, text := mcpMustCall(t, cs, "validate_plan", map[string]any{"json": `{"name": "t", "tasks": [`})
	errs := out["errors"].([]any)
	if out["valid"] != false || len(errs) != 1 || !strings.Contains(text, "invalid") {
		t.Fatalf("invalid plan: %v", out)
	}
	if _, _, errText := mcpCall(t, cs, "validate_plan", map[string]any{"path": "-"}); errText == "" {
		t.Fatal("stdin path accepted over the protocol transport")
	}
	if _, _, errText := mcpCall(t, cs, "validate_plan", map[string]any{}); errText == "" {
		t.Fatal("empty input accepted")
	}
}

func TestMCPApplyStartsDetachedRunWithRequiredBudget(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(model.Plan{Name: "t", Tasks: []model.Task{runfixture.Task("a", 0)}})
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(dir, "runs")
	cs := mcpConnect(t, mcpserver.McpConfig{RunRoots: []string{runs}, Workspace: dir, AllowControl: true})
	if _, _, errText := mcpCall(t, cs, "apply", map[string]any{"plan": "plan.json", "run": "one", "backend": "mock"}); !strings.Contains(errText, "budget is required") {
		t.Fatalf("budget not required: %q", errText)
	}
	if _, err := os.Stat(filepath.Join(runs, "one")); err == nil {
		t.Fatal("rejected apply created a run")
	}
	if _, _, errText := mcpCall(t, cs, "apply", map[string]any{"plan": "plan.json", "run": "one", "budget": 0.1}); !strings.Contains(errText, "backend is required") {
		t.Fatalf("backend not required: %q", errText)
	}
	out, _ := mcpMustCall(t, cs, "apply", map[string]any{"plan": "plan.json", "run": "one", "backend": "mock", "budget": 0.1, "workers": 1})
	root := filepath.Join(runs, "one")
	if out["run"] != root || out["started"] != true {
		t.Fatalf("apply output: %v", out)
	}
	waitFor(t, root, func(s *model.State) bool { return s.Control == "complete" })
	if got := runfixture.Read(t, root).Tasks["a"].Status; got != "verified" {
		t.Fatalf("task status %s", got)
	}
	if _, _, errText := mcpCall(t, cs, "apply", map[string]any{"plan": "plan.json", "run": "one", "backend": "mock", "budget": 0.1}); errText == "" {
		t.Fatal("apply overwrote an existing run")
	}
}

func TestMCPPauseAndResume(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	var started [][]string
	old := mcpserver.McpStartDetached
	mcpserver.McpStartDetached = func(args []string) error { started = append(started, args); return nil }
	t.Cleanup(func() { mcpserver.McpStartDetached = old })
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir(), AllowControl: true})
	mcpMustCall(t, cs, "pause", map[string]any{"run": root})
	if got := runfixture.Read(t, root).Control; got != "pause" {
		t.Fatalf("control %s", got)
	}
	mcpMustCall(t, cs, "resume", map[string]any{"run": root, "retry_failed": true})
	if len(started) != 1 || strings.Join(started[0], " ") != "resume "+root+" --retry-failed" {
		t.Fatalf("resume args: %v", started)
	}
	// A held controller lock must be reported, not raced.
	lock, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(lock)
	if _, _, errText := mcpCall(t, cs, "resume", map[string]any{"run": root}); !strings.Contains(errText, "already running") {
		t.Fatalf("running controller: %q", errText)
	}
	if len(started) != 1 {
		t.Fatal("resume started a second controller")
	}
}

func TestMCPRetryCellKeepsUncertaintyRules(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "uncertain"; s.Tasks["a"].Attempt = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root).Tasks["a"]
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir(), AllowControl: true})
	// The attestations are required arguments: omitting them is refused.
	if _, _, errText := mcpCall(t, cs, "retry_cell", map[string]any{"run": root, "cell": "a"}); errText == "" {
		t.Fatal("retry accepted without explicit allow_uncertain/confirm_stopped")
	}
	// confirm_stopped alone does not settle an uncertain outcome.
	if _, _, errText := mcpCall(t, cs, "retry_cell", map[string]any{"run": root, "cell": "a", "allow_uncertain": false, "confirm_stopped": true}); !strings.Contains(errText, "--allow-uncertain") {
		t.Fatalf("uncertain retry bypassed: %q", errText)
	}
	after := runfixture.Read(t, root).Tasks["a"]
	if after.Status != "uncertain" || after.OperationID != before.OperationID || after.Generation != before.Generation {
		t.Fatal("refused retry changed the operation")
	}
	out, _ := mcpMustCall(t, cs, "retry_cell", map[string]any{"run": root, "cell": "a", "allow_uncertain": true, "confirm_stopped": true})
	if got := out["retried"].([]any); len(got) != 1 || got[0] != "a" {
		t.Fatalf("retried: %v", out)
	}
	if got := runfixture.Read(t, root).Tasks["a"]; got.Status != "pending" || got.Generation != before.Generation+1 {
		t.Fatalf("attested retry state: %s gen %d", got.Status, got.Generation)
	}
}

func TestMCPDecideApprovalUsesMailboxAndSingleUseLedger(t *testing.T) {
	root := runfixture.MailboxFixture(t)
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir(), AllowControl: true})
	if _, _, errText := mcpCall(t, cs, "decide_approval", map[string]any{"run": root, "request": "a", "decision": "maybe"}); errText == "" {
		t.Fatal("invalid decision accepted")
	}
	if _, _, errText := mcpCall(t, cs, "decide_approval", map[string]any{"run": root, "request": "ghost", "decision": "approve"}); errText == "" {
		t.Fatal("unknown request accepted")
	}
	out, text := mcpMustCall(t, cs, "decide_approval", map[string]any{"run": root, "request": "a", "decision": "deny"})
	if out["decision"] != "deny" || !strings.Contains(text, "denied") {
		t.Fatalf("decision: %v / %s", out, text)
	}
	s := runfixture.Read(t, root)
	if len(s.Mailbox.Entries) != 1 || s.Mailbox.Entries[0].Message.Kind != "signal" || !strings.Contains(s.Mailbox.Entries[0].Message.Payload, "deny") {
		t.Fatalf("mailbox: %+v", s.Mailbox.Entries)
	}
	used, err := os.ReadFile(filepath.Join(approvals.ApprovalDir(root), "used.jsonl"))
	if err != nil || !strings.Contains(string(used), `"via":"mcp"`) {
		t.Fatalf("approval consumption not recorded: %q %v", used, err)
	}
}

func TestMCPDecideApprovalWithoutMailboxWritesLedger(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir(), AllowControl: true})
	mcpMustCall(t, cs, "decide_approval", map[string]any{"run": root, "request": "a", "decision": "approve"})
	ledger, err := os.ReadFile(filepath.Join(approvals.ApprovalDir(root), "decisions.jsonl"))
	if err != nil || strings.Count(string(ledger), "\n") != 1 || !strings.Contains(string(ledger), `"decision":"approve"`) {
		t.Fatalf("ledger: %q %v", ledger, err)
	}
}

func TestMCPToolsAreFast(t *testing.T) {
	// A connected client must get a prompt tool error for a bad run, not hang.
	cs := mcpConnect(t, mcpserver.McpConfig{Workspace: t.TempDir()})
	done := make(chan struct{})
	go func() {
		mcpCall(t, cs, "run_status", map[string]any{"run": ""})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tool call hung")
	}
}
