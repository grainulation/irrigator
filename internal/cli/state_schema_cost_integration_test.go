//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// The original paid-run case, corrected for the real contract: storage schema is
// not consent. The fixture is a paid (haiku) backend whose tasks are already
// verified, so no case here can dispatch a model call.
func TestUnbudgetedLegacyUnbudgetedResumeFailClosed(t *testing.T) {
	root := unbudgetedLegacyFixture(t, "haiku")
	statePath := filepath.Join(root, "state.json")
	registry := filepath.Join(t.TempDir(), "registry.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(label string) {
		t.Helper()
		after, err := os.ReadFile(statePath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("%s: refused resume must not mutate the checkpoint (err %v)", label, err)
		}
	}

	// Resuming unbudgeted legacy run without --legacy-unbudgeted=allow must fail closed with exit code 2
	code, err := runCommand([]string{"resume", root, "--session-registry", registry})
	if code != 2 {
		t.Fatalf("expected exit code 2 for legacy unbudgeted run without flag, got %d (err: %v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "--legacy-unbudgeted=allow") {
		t.Fatalf("expected error mentioning --legacy-unbudgeted=allow, got: %v", err)
	}
	unchanged("no flag")

	// Passing --legacy-unbudgeted=wrong must still fail closed with exit code 2
	codeWrong, errWrong := runCommand([]string{"resume", root, "--legacy-unbudgeted=yes", "--session-registry", registry})
	if codeWrong != 2 || errWrong == nil {
		t.Fatalf("expected exit code 2 for --legacy-unbudgeted=yes, got %d %v", codeWrong, errWrong)
	}
	unchanged("invalid flag value")

	// Passing --call-budget provides explicit budget and bypasses legacy unbudgeted block.
	// The positive budget is durably persisted before anything runs.
	codeBudget, errBudget := runCommand([]string{"resume", root, "--call-budget=0.10", "--session-registry", registry})
	if codeBudget == 2 && errBudget != nil && strings.Contains(errBudget.Error(), "--legacy-unbudgeted=allow") {
		t.Fatalf("unexpected legacy-unbudgeted error when --call-budget was provided: %v", errBudget)
	}
	saved, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CallLimits == nil || saved.CallLimits.Budget != 0.10 {
		t.Fatalf("explicit call budget was not persisted: %+v", saved.CallLimits)
	}
	if executor.UnbudgetedGateApplies(saved) {
		t.Fatal("run with a persisted positive call budget is still gated")
	}
}

func TestUnbudgetedLegacyUnbudgetedGateIgnoresSchemaStamp(t *testing.T) {
	for _, mutate := range []string{"pause", "down"} {
		t.Run("ordinary "+mutate+" update", func(t *testing.T) {
			root := unbudgetedLegacyFixture(t, "haiku")
			registry := filepath.Join(t.TempDir(), "registry.json")
			if code, err := runCommand([]string{mutate, root}); code != 0 || err != nil {
				t.Fatalf("%s: %d %v", mutate, code, err)
			}
			stamped, err := checkpoint.ReadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if stamped.SchemaVersion != model.CurrentSchemaVersion {
				t.Fatalf("precondition: ordinary update must stamp schema %d, got %d", model.CurrentSchemaVersion, stamped.SchemaVersion)
			}
			if stamped.UnbudgetedDecision != "" || stamped.CallLimits != nil {
				t.Fatalf("an ordinary update recorded consent: %+v", stamped)
			}
			if !executor.UnbudgetedGateApplies(stamped) || executor.AdmitUnbudgetedExecution(stamped) == nil {
				t.Fatal("schema stamp bypassed the legacy unbudgeted gate")
			}
			// Restart: a fresh process view must gate the same way.
			code, err := runCommand([]string{"resume", root, "--session-registry", registry})
			if code != 2 || err == nil || !strings.Contains(err.Error(), "--legacy-unbudgeted=allow") {
				t.Fatalf("resume after %s: %d %v, want gate", mutate, code, err)
			}
		})
	}

	t.Run("state already stamped schema 2 by old code", func(t *testing.T) {
		root := unbudgetedLegacyFixture(t, "haiku")
		runfixture.MutateStateJSON(t, root, func(raw map[string]any) { raw["schema_version"] = 2 })
		registry := filepath.Join(t.TempDir(), "registry.json")
		code, err := runCommand([]string{"resume", root, "--session-registry", registry})
		if code != 2 || err == nil || !strings.Contains(err.Error(), "--legacy-unbudgeted=allow") {
			t.Fatalf("schema-2 state without a decision: %d %v, want gate", code, err)
		}
	})

	t.Run("paid classification is conservative", func(t *testing.T) {
		root := fixture(t, task("a", 0))
		s, err := checkpoint.ReadState(root)
		if err != nil {
			t.Fatal(err)
		}
		if executor.UnbudgetedGateApplies(s) {
			t.Fatal("pure mock run must be exempt")
		}
		if !checkpoint.PotentiallyPaid("haiku", nil) || !checkpoint.PotentiallyPaid("named", nil) {
			t.Fatal("non-mock backend must be treated as paid")
		}
		if !checkpoint.PotentiallyPaid("mock", []model.Task{{ID: "x", Provider: "haiku"}}) || !checkpoint.PotentiallyPaid("mock", []model.Task{{ID: "x", Provider: "named"}}) {
			t.Fatal("mock backend with a non-mock task must be treated as paid")
		}
		if checkpoint.PotentiallyPaid("mock", []model.Task{{ID: "x"}, {ID: "y", Provider: "mock"}}) {
			t.Fatal("all-mock tasks must be free")
		}
	})
}

func TestUnbudgetedFreeAndManagedRunsAreNotGated(t *testing.T) {
	root := unbudgetedLegacyFixture(t, "mock")
	registry := filepath.Join(t.TempDir(), "registry.json")
	// Both the pristine legacy shape and the schema-stamped shape are free.
	s, err := checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if executor.UnbudgetedGateApplies(s) {
		t.Fatal("legacy mock run was gated")
	}
	if code, err := runCommand([]string{"pause", root}); code != 0 || err != nil {
		t.Fatalf("pause: %d %v", code, err)
	}
	s, err = checkpoint.ReadState(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.SchemaVersion != model.CurrentSchemaVersion || executor.UnbudgetedGateApplies(s) || executor.AdmitUnbudgetedExecution(s) != nil {
		t.Fatal("stamped mock run was gated")
	}
	code, err := runCommand([]string{"resume", root, "--session-registry", registry})
	if err != nil && strings.Contains(err.Error(), "legacy-unbudgeted") {
		t.Fatalf("free mock run was blocked by the legacy unbudgeted gate: %d %v", code, err)
	}
	if code == 2 {
		t.Fatalf("free mock resume returned usage error 2: %v", err)
	}
	// An invalid flag value is rejected uniformly, never silently ignored.
	if code, err := runCommand([]string{"resume", root, "--legacy-unbudgeted=maybe", "--session-registry", registry}); code != 2 || err == nil {
		t.Fatalf("invalid flag value accepted for a free run: %d %v", code, err)
	}

	managed := &model.State{Plan: model.Plan{APIVersion: "swarm/v3"}, Backend: "managed"}
	if !checkpoint.ManagedRun(managed) {
		t.Fatal("test fixture is not a managed run")
	}
	if executor.UnbudgetedGateApplies(managed) || executor.AdmitUnbudgetedExecution(managed) != nil {
		t.Fatal("managed profile-budget run was gated")
	}
}
