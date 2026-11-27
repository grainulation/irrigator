package managed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
)

func TestManagedLauncherModelEffortIsPinnedWithoutDispatch(t *testing.T) {
	dir := t.TempDir()
	called := filepath.Join(dir, "called")
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n: > '"+called+"'\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	prefs := filepath.Join(canonicalDir, "preferences.json")
	if err := os.WriteFile(prefs, []byte(`{"schema":"irrigator.preferences/v1","launcher":{"provider":"codex","model":"gpt-6.2-sol","effort":"high"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := managedInitialPlanWithOptions("", "", prefs)
	if err != nil {
		t.Fatal(err)
	}
	p := plan.Execution.Catalog.Profiles["default"]
	if p.Model != "gpt-6.2-sol" || p.Effort != "high" || len(plan.Execution.AllowedProfiles) != 1 {
		t.Fatalf("lost single-model selection: %+v", plan)
	}
	var selection launch.ExecutionSelection
	if err := json.Unmarshal([]byte(plan.Context["execution_selection"]), &selection); err != nil || selection.Source != "launcher" {
		t.Fatalf("missing admitted selection provenance: %v", err)
	}
	if err := checkpoint.Validate(plan); err != nil {
		t.Fatal(err)
	}
	binding, err := launch.ResolveExecutionCatalog(plan.Execution.Catalog, "inline", "default")
	if err != nil {
		t.Fatal(err)
	}
	args := managedCodexArgs(binding, "/tmp/work", "/tmp/schema")
	if !slices.Contains(args, `model_reasoning_effort="high"`) || slices.Contains(args, `model_reasoning_effort="low"`) {
		t.Fatalf("child argv downgraded effort: %q", args)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatal("planning executed provider")
	}
	plan, err = managedInitialPlanWithOptions("", "medium", prefs)
	if err != nil || plan.Execution.Catalog.Profiles["default"].Effort != "medium" {
		t.Fatal("explicit effort override failed", err)
	}
}
