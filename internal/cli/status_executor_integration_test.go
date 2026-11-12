//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/mcpserver"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestStatusShowsRecordedExecutor(t *testing.T) {
	root := fixture(t, task("a", 0))
	text := runfixture.CaptureStdout(t, func() {
		if code, err := runCommand([]string{"status", root}); err != nil || code != 0 {
			t.Fatalf("status: %d %v", code, err)
		}
	})
	if !strings.Contains(text, "executor: local") {
		t.Fatalf("unrecorded run should show the default executor:\n%s", text)
	}
	if err := executor.WriteExecutorRecord(root, "tmux"); err != nil {
		t.Fatal(err)
	}
	text = runfixture.CaptureStdout(t, func() { _, _ = runCommand([]string{"status", root}) })
	if !strings.Contains(text, "executor: tmux") {
		t.Fatalf("text status:\n%s", text)
	}
	raw := runfixture.CaptureStdout(t, func() { _, _ = runCommand([]string{"status", root, "--json"}) })
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, raw)
	}
	if doc["executor"] != "tmux" || doc["control"] == nil && doc["Control"] == nil {
		t.Fatalf("status --json: executor=%v keys=%d", doc["executor"], len(doc))
	}
}

func TestMCPApplyExecutorIsValidatedAndPassed(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(model.Plan{Name: "t", Tasks: []model.Task{task("a", 0)}})
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var started [][]string
	old := mcpserver.McpStartDetached
	mcpserver.McpStartDetached = func(args []string) error { started = append(started, args); return nil }
	t.Cleanup(func() { mcpserver.McpStartDetached = old })
	runs := filepath.Join(dir, "runs")
	cs := mcpConnect(t, mcpserver.McpConfig{RunRoots: []string{runs}, Workspace: dir, AllowControl: true})
	call := func(run, executor string) map[string]any {
		return map[string]any{"plan": "plan.json", "backend": "mock", "budget": 0.1, "run": run, "executor": executor}
	}
	if _, _, errText := mcpCall(t, cs, "apply", call("bad", "zellij")); !strings.Contains(errText, "not a known executor") {
		t.Fatalf("unknown executor accepted: %q", errText)
	}
	if _, err := os.Stat(filepath.Join(runs, "bad")); err == nil || len(started) != 0 {
		t.Fatal("rejected apply created a run or started a controller")
	}
	out, _ := mcpMustCall(t, cs, "apply", call("good", "tmux"))
	root := filepath.Join(runs, "good")
	if out["executor"] != "tmux" {
		t.Fatalf("apply output: %v", out)
	}
	if len(started) != 1 || !strings.Contains(strings.Join(started[0], " "), "--executor tmux") {
		t.Fatalf("controller args: %v", started)
	}
	if got, _ := executor.ReadExecutorRecord(root); got != "tmux" {
		t.Fatalf("recorded executor %q", got)
	}
	status, text := mcpMustCall(t, cs, "run_status", map[string]any{"run": root})
	if status["executor"] != "tmux" || !strings.Contains(text, "tmux") {
		t.Fatalf("run_status: %v %q", status, text)
	}
	list, _ := mcpMustCall(t, cs, "list_runs", map[string]any{})
	runsOut, _ := list["runs"].([]any)
	if len(runsOut) != 1 || runsOut[0].(map[string]any)["executor"] != "tmux" {
		t.Fatalf("list_runs: %v", list)
	}
}
