package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestProfileRoutingCLINewPlanAndStrictPolicy(t *testing.T) {
	dir := runfixture.CanonicalContextDir(t)
	raw, err := os.ReadFile("../../examples/managed-profiles-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	policy := `{"schema":"irrigator.roles/v1","roles":{"implementation":{"easy":"reviewer"}},"tasks":{"agent_a":{"role":"implementation","difficulty":"easy"}}}`
	policyPath := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policyPath, []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "routed.json")
	if code, err := profileRoutingCLI([]string{planPath, "--policy", policyPath, "--out", out}); err != nil || code != 0 {
		t.Fatalf("route: %d %v", code, err)
	}
	plan, err := checkpoint.LoadPlan(out)
	if err != nil || plan.Tasks[0].Profile != "reviewer" {
		t.Fatalf("routed plan: %+v %v", plan.Tasks, err)
	}
	after, _ := os.ReadFile(planPath)
	if string(after) != string(raw) {
		t.Fatal("source overwritten")
	}
	if code, err := profileRoutingCLI([]string{planPath, "--policy", policyPath, "--out", out}); err == nil || code == 0 {
		t.Fatal("overwrote existing output")
	}
	if err := os.WriteFile(policyPath, []byte(`{"schema":"irrigator.roles/v1","schema":"irrigator.roles/v1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := profileRoutingCLI([]string{planPath, "--policy", policyPath}); err == nil || code == 0 {
		t.Fatal("accepted duplicate key")
	}
	definitions, err := commandDefinitions(nil)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(definitions)
	if len(data) == 0 {
		t.Fatal("no CLI metadata")
	}
}
