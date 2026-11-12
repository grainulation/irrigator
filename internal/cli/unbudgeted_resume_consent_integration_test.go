//go:build integration

package cli

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestUnbudgetedResumeAllowDecisionIsDurableIdempotentAndPreservesHistory(t *testing.T) {
	root := unbudgetedLegacyFixture(t, "haiku")
	before := read(t, root)

	// A refused admission writes nothing: no schema stamp, no decision, no event.
	if err := admitLegacyResume(root, false, false, runfixture.UnbudgetedTestOptions()); !errors.Is(err, executor.ErrLegacyUnbudgeted) {
		t.Fatalf("admission without consent = %v, want errLegacyUnbudgeted", err)
	}
	refused := read(t, root)
	if refused.SchemaVersion != 0 || refused.UnbudgetedDecision != "" || len(refused.Events) != len(before.Events) {
		t.Fatalf("refused admission mutated the checkpoint: %+v", refused)
	}

	if err := admitLegacyResume(root, true, false, runfixture.UnbudgetedTestOptions()); err != nil {
		t.Fatal(err)
	}
	after := read(t, root)
	if after.UnbudgetedDecision != checkpoint.UnbudgetedAllow {
		t.Fatalf("decision = %q, want %q", after.UnbudgetedDecision, checkpoint.UnbudgetedAllow)
	}
	if executor.UnbudgetedGateApplies(after) || executor.AdmitUnbudgetedExecution(after) != nil {
		t.Fatal("recorded allow decision does not open the gate")
	}
	if after.CallLimits != nil {
		t.Fatalf("allow must not invent a call budget: %+v", after.CallLimits)
	}
	if got := unbudgetedCountEvents(after, "legacy_unbudgeted_allowed"); got != 1 {
		t.Fatalf("allow events = %d, want 1", got)
	}

	// Plan, hash, accepted results and prior history are untouched.
	if after.PlanHash != before.PlanHash || !reflect.DeepEqual(after.Plan, before.Plan) {
		t.Fatal("plan or plan hash changed")
	}
	row := after.Tasks["a"]
	if row.Status != "verified" || row.Attempt != 1 || row.Result["answer"] != "accepted" || row.Generation != before.Tasks["a"].Generation || row.OperationID != before.Tasks["a"].OperationID {
		t.Fatalf("verified result or identity changed: %+v", row)
	}
	for i, e := range before.Events {
		if after.Events[i].Kind != e.Kind || after.Events[i].Task != e.Task {
			t.Fatalf("history event %d rewritten", i)
		}
	}

	// Idempotent: recording again adds nothing.
	events := len(after.Events)
	if err := admitLegacyResume(root, true, false, runfixture.UnbudgetedTestOptions()); err != nil {
		t.Fatal(err)
	}
	if again := read(t, root); len(again.Events) != events || again.UnbudgetedDecision != checkpoint.UnbudgetedAllow {
		t.Fatalf("second allow was not idempotent: %d events, decision %q", len(again.Events), again.UnbudgetedDecision)
	}

	// The decision survives ordinary checkpoint mutations and a restart.
	for _, command := range []string{"pause", "down"} {
		if code, err := runCommand([]string{command, root}); code != 0 || err != nil {
			t.Fatalf("%s: %d %v", command, code, err)
		}
		restarted := read(t, root)
		if restarted.UnbudgetedDecision != checkpoint.UnbudgetedAllow || executor.UnbudgetedGateApplies(restarted) {
			t.Fatalf("decision lost after %s: %+v", command, restarted)
		}
	}
}

func TestUnbudgetedResumeAllowFlagThroughCLIRecordsDecision(t *testing.T) {
	root := unbudgetedLegacyFixture(t, "haiku")
	registry := filepath.Join(t.TempDir(), "registry.json")
	code, err := runCommand([]string{"resume", root, "--legacy-unbudgeted=allow", "--session-registry", registry})
	if err != nil && strings.Contains(err.Error(), "legacy-unbudgeted") {
		t.Fatalf("explicit allow was refused: %d %v", code, err)
	}
	s := read(t, root)
	if s.UnbudgetedDecision != checkpoint.UnbudgetedAllow {
		t.Fatalf("decision = %q, want %q", s.UnbudgetedDecision, checkpoint.UnbudgetedAllow)
	}
	// Restart without the flag: the durable decision still admits the run.
	code, err = runCommand([]string{"resume", root, "--session-registry", registry})
	if err != nil && strings.Contains(err.Error(), "legacy-unbudgeted") {
		t.Fatalf("durable decision not honored on restart: %d %v", code, err)
	}
}

func TestUnbudgetedResumeUnknownUnbudgetedDecisionFailsValidation(t *testing.T) {
	root := unbudgetedLegacyFixture(t, "haiku")
	runfixture.MutateStateJSON(t, root, func(raw map[string]any) { raw["unbudgeted_decision"] = "maybe" })
	if _, err := checkpoint.ReadState(root); err == nil || !strings.Contains(err.Error(), "unbudgeted_decision") {
		t.Fatalf("unknown decision accepted: %v", err)
	}
	registry := filepath.Join(t.TempDir(), "registry.json")
	if code, err := runCommand([]string{"resume", root, "--legacy-unbudgeted=allow", "--session-registry", registry}); err == nil || code == 0 {
		t.Fatalf("resume accepted a checkpoint with an unknown decision: %d %v", code, err)
	}
	for _, valid := range []string{checkpoint.UnbudgetedAllow, checkpoint.UnbudgetedInitial} {
		runfixture.MutateStateJSON(t, root, func(raw map[string]any) { raw["unbudgeted_decision"] = valid })
		if _, err := checkpoint.ReadState(root); err != nil {
			t.Fatalf("decision %q rejected: %v", valid, err)
		}
	}
}
