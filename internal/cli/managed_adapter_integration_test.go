//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestManagedCLIRejectsBudgetAndBackendOverrides(t *testing.T) {
	plan := filepath.Join("..", "..", "examples", "managed-profiles-v3.json")
	for _, extra := range [][]string{{"--budget", "0.25"}, {"--backend", "mock"}} {
		run := filepath.Join(t.TempDir(), "run")
		args := append([]string{"apply", plan, "--run", run}, extra...)
		if code, err := runCommand(args); err == nil || code == 0 {
			t.Fatal("accepted managed override")
		}
		if _, err := os.Stat(filepath.Join(run, "state.json")); !os.IsNotExist(err) {
			t.Fatal("override initialized run")
		}
	}
	run := filepath.Join(t.TempDir(), "prepared")
	if code, err := runCommand([]string{"prepare", plan, "--run", run}); err != nil || code != 0 {
		t.Fatalf("prepare: %d %v", code, err)
	}
	if code, err := runCommand([]string{"resume", run, "--budget", "0.25"}); err == nil || code == 0 {
		t.Fatal("accepted resume override")
	}
}

func TestManagedTwoProfileExampleCompletes(t *testing.T) {
	p, err := checkpoint.LoadPlan(filepath.Join("..", "..", "examples", "managed-profiles-v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "run")
	if err = checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, root, "--workers", "2")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	if s.Version != 2 || len(s.Tasks) != 12 || len(s.Execution.Bindings) != 2 || len(s.Execution.Reservations) != 12 {
		t.Fatalf("incomplete managed graph: version%d tasks%d bindings%d reservations%d", s.Version, len(s.Tasks), len(s.Execution.Bindings), len(s.Execution.Reservations))
	}
	for id, row := range s.Tasks {
		if row.Status != "verified" || row.Attempt != 1 || row.BindingDigest == "" {
			t.Fatalf("%s not verified once with binding: %+v", id, row)
		}
		profile := "writer"
		if strings.HasPrefix(id, "agent_b") {
			profile = "reviewer"
		}
		if row.BindingDigest != s.Execution.Bindings[profile].Digest {
			t.Fatalf("%s lost profile inheritance", id)
		}
	}
}
