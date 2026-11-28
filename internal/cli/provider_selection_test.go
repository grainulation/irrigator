package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestNewSingleModelRoutingRequiresExplicitOptIn(t *testing.T) {
	t.Setenv(launch.ExecutionPreferencesEnv, "")
	dir := runfixture.CanonicalContextDir(t)
	raw, err := os.ReadFile("../../examples/managed-profiles-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	input["context"] = map[string]string{"execution_selection": `{"schema":"irrigator.selection/v1","source":"launcher"}`}
	raw, _ = json.Marshal(input)
	planPath, policyPath, out := filepath.Join(dir, "plan.json"), filepath.Join(dir, "policy.json"), filepath.Join(dir, "routed.json")
	if err := os.WriteFile(planPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	policy := `{"schema":"irrigator.roles/v1","roles":{"work":{"easy":"writer"}},"tasks":{"agent_a":{"role":"work","difficulty":"easy"}}}`
	if err := os.WriteFile(policyPath, []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := profileRoutingCLI([]string{planPath, "--policy", policyPath, "--out", out}); err == nil || code == 0 {
		t.Fatal("new single-model plan became mixed without opt-in")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("refused routing published output")
	}
	if code, err := profileRoutingCLI([]string{planPath, "--policy", policyPath, "--multimodel", "--include-profiles", "writer,reviewer", "--out", out}); err != nil || code != 0 {
		t.Fatalf("explicit opt-in failed: %d %v", code, err)
	}
	after, _ := os.ReadFile(planPath)
	if string(after) != string(raw) {
		t.Fatal("routing edited the source plan")
	}
}

func TestProfileExclusionCannotSilentlyRerouteTask(t *testing.T) {
	t.Setenv(launch.ExecutionPreferencesEnv, "")
	dir := runfixture.CanonicalContextDir(t)
	raw, err := os.ReadFile("../../examples/managed-profiles-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	planPath, policyPath, out := filepath.Join(dir, "plan.json"), filepath.Join(dir, "policy.json"), filepath.Join(dir, "routed.json")
	if err := os.WriteFile(planPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	policy := `{"schema":"irrigator.roles/v1","roles":{"work":{"easy":"writer"}},"tasks":{"agent_a":{"role":"work","difficulty":"easy"}}}`
	if err := os.WriteFile(policyPath, []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := profileRoutingCLI([]string{planPath, "--policy", policyPath, "--exclude-profiles", "reviewer", "--out", out}); err == nil || code == 0 {
		t.Fatal("opted-out task was silently rerouted")
	}
	policy = `{"schema":"irrigator.roles/v1","roles":{"work":{"easy":"writer"}},"tasks":{"agent_a":{"role":"work","difficulty":"easy"},"agent_b":{"role":"work","difficulty":"easy"}}}`
	if err := os.WriteFile(policyPath, []byte(policy), 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := profileRoutingCLI([]string{planPath, "--policy", policyPath, "--include-profiles", "writer", "--out", out}); err != nil || code != 0 {
		t.Fatalf("explicit uniform route failed: %d %v", code, err)
	}
	plan, err := checkpoint.LoadPlan(out)
	if err != nil || len(plan.Execution.AllowedProfiles) != 1 || plan.Execution.AllowedProfiles[0] != "writer" {
		t.Fatalf("allowed set was not narrowed: %+v %v", plan.Execution, err)
	}
}
