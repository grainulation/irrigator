package coder

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	executionlaunch "github.com/grainulation/irrigator/internal/launch"
)

func TestCodingCLIRequiresSelectionBeforeCreatingRun(t *testing.T) {
	t.Setenv(executionlaunch.ExecutionPreferencesEnv, "")
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	called := false
	code, err := CodeCLI([]string{"--task", "work=do work", "--run", run}, func([]string) (int, error) { called = true; return 0, nil })
	if code != 2 || err == nil || !strings.Contains(err.Error(), "no execution provider selected") || called {
		t.Fatalf("implicit provider dispatch: %d %v", code, err)
	}
	if _, err := os.Stat(run); !os.IsNotExist(err) {
		t.Fatal("created run before selection")
	}
}

func TestCodingPlanAndArgumentsRetainExplicitEffort(t *testing.T) {
	p, err := buildPlan(planOptions{agent: "codex", model: "gpt-6.2-sol", effort: "high", repo: t.TempDir(), base: "main", executable: "/bin/irrigator", tasks: []string{"a=work"}, selectionJSON: `{"source":"launcher"}`})
	if err != nil {
		t.Fatal(err)
	}
	providers := p["providers"].(map[string]any)
	command := providers["coder"].(map[string]any)["command"].([]string)
	if !slices.Contains(command, "--effort") || !slices.Contains(command, "high") {
		t.Fatal("plan lost effort", command)
	}
	_, args, err := agentArgs(launch{agent: "codex", model: "gpt-6.2-sol", effort: "high", worktree: "/tmp/work"})
	if err != nil || !slices.Contains(args, `model_reasoning_effort="high"`) {
		t.Fatalf("coding child lost effort %q %v", args, err)
	}
}
