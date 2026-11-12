//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestWorkerSecurityLegacyBackendOmissionCannotCreateRun(t *testing.T) {
	for _, command := range []string{"apply", "prepare", "deliver"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			plan := filepath.Join(dir, "plan.json")
			run := filepath.Join(dir, "run")
			data, _ := json.Marshal(model.Plan{Name: "test", Tasks: []model.Task{task("node", 0)}})
			if err := os.WriteFile(plan, data, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{command, plan, "--run", run}
			if command == "deliver" {
				args = append(args, "--dir", filepath.Join(dir, "delivery"))
			}
			code, err := runCommand(args)
			if code != 2 || err == nil || !strings.Contains(err.Error(), "--backend") {
				t.Fatalf("omission not rejected: %d %v", code, err)
			}
			if _, err := os.Stat(run); !os.IsNotExist(err) {
				t.Fatal("omitted backend created run state")
			}
		})
	}
}

func TestWorkerSecurityManagedIntentAndCallBudgetBoundary(t *testing.T) {
	existing := runfixture.ManagedStateFixture(t, "mock", "0.1", 1)
	state := read(t, existing)
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	data, _ := json.Marshal(state.Plan)
	if err := os.WriteFile(plan, data, 0600); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(dir, "prepared")
	if code, err := runCommand([]string{"prepare", plan, "--run", run}); code != 0 || err != nil {
		t.Fatalf("pinned managed intent rejected: %d %v", code, err)
	}
	if got := read(t, run).Backend; got != "managed" {
		t.Fatalf("inferred backend=%s", got)
	}
	for _, command := range []string{"apply", "resume"} {
		args := []string{command, plan, "--call-budget", "0.01"}
		rejected := filepath.Join(dir, "rejected")
		if command == "apply" {
			args = append(args, "--run", rejected)
		} else {
			args[1] = run
		}
		code, err := runCommand(args)
		if code != 2 || err == nil || !strings.Contains(err.Error(), "profile operation budgets") {
			t.Fatalf("managed budget override accepted: %d %v", code, err)
		}
		if _, err := os.Stat(rejected); !os.IsNotExist(err) {
			t.Fatal("rejected budget created run")
		}
	}
	code, err := runCommand([]string{"apply", plan, "--run", filepath.Join(dir, "conflict"), "--call-budget", "0.01", "--budget", "0.01"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("ambiguous budget aliases accepted: %d %v", code, err)
	}
}
