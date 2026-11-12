package managed

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
)

func TestManagedInitPinsSymlinkWithoutExecuting(t *testing.T) {
	dir := t.TempDir()
	called := filepath.Join(dir, "called")
	real := filepath.Join(dir, "provider-version")
	script := []byte("#!/bin/sh\ntouch '" + called + "'\nexit 1\n")
	if err := os.WriteFile(real, script, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "claude")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	p, err := managedInitialPlan("claude:sonnet")
	if err != nil {
		t.Fatal(err)
	}
	a := p.Execution.Catalog.Adapters["claude"]
	resolved, _ := filepath.EvalSymlinks(real)
	if len(a.Command) != 1 || a.Command[0] != resolved || a.ExecutableSHA256 != fmt.Sprintf("%x", sha256.Sum256(script)) {
		t.Fatalf("unresolved pin: %+v", a)
	}
	if p.Execution.CostPolicy != "track" || p.Execution.RunBudgetUSD != "0.25" || p.Execution.Catalog.Profiles["default"].OperationBudgetUSD != "0.25" || p.Execution.Catalog.Profiles["default"].TimeoutSeconds != 600 {
		t.Fatal("missing bounded defaults")
	}
	if err := os.Symlink(real, filepath.Join(dir, "codex")); err != nil {
		t.Fatal(err)
	}
	codex, err := managedInitialPlan("codex:gpt-6-luna")
	if err != nil || codex.Execution.Catalog.Adapters["codex"].Kind != "codex_cli" {
		t.Fatalf("codex scaffold: %v", err)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatal("scaffold executed provider")
	}
	out := filepath.Join(dir, "plan.json")
	if err := WriteManagedInitialPlan(out, "claude:sonnet"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(out)
	if err := WriteManagedInitialPlan(out, "claude:sonnet"); err == nil {
		t.Fatal("overwrote existing plan")
	}
	after, _ := os.ReadFile(out)
	if string(before) != string(after) {
		t.Fatal("modified existing plan")
	}
	if _, err := checkpoint.LoadPlan(out); err != nil {
		t.Fatal("generated plan invalid", err)
	}
}

func TestManagedInitRejectsUnavailableOrUnsupported(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, selector := range []string{"", "sonnet", "unrecognized:model", "claude:", "claude:sonnet"} {
		if _, err := managedInitialPlan(selector); err == nil {
			t.Fatalf("accepted %q", selector)
		}
	}
}
