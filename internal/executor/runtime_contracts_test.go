package executor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestRuntimeGuardrailsFailClosedAndPreserveLegacy(t *testing.T) {
	p := model.Plan{Name: "legacy", Tasks: []model.Task{runfixture.Task("a", 0)}}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "guardrails") {
		t.Fatal("legacy encoding changed")
	}
	template, in := runfixture.FormationFixture(t)
	p, err := runfixture.CompilePackageUpgrade(template, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, require := range []string{"host_resource_limits", "hard_token_limit", "complete_tool_mediation", "automatic_git_settlement", "automatic_worktree_quarantine", "invented"} {
		p.Guardrails = &model.RuntimeGuardrails{Schema: launch.RuntimeGuardrailsSchema, Require: []string{require}}
		root := filepath.Join(t.TempDir(), "rejected")
		if err := checkpoint.Initialize(p, root, "managed"); err == nil {
			t.Fatalf("admitted unavailable %s", require)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("rejection created a run")
		}
	}
	p.Guardrails = &model.RuntimeGuardrails{Schema: launch.RuntimeGuardrailsSchema, Require: []string{"attempt_fencing", "unknown_cost_holds", "bounded_artifact_validation"}, MaxInflight: 1}
	if err := checkpoint.Validate(p); err != nil {
		t.Fatal(err)
	}
	p.Execution.CostPolicy = "track"
	if checkpoint.Validate(p) == nil {
		t.Fatal("track mode satisfied retained monetary holds")
	}
}
func TestRuntimeGuardrailsAdmissionCouplesUnknownHolds(t *testing.T) {
	source := runfixture.ManagedStateFixture(t, "claude_cli", "1", 2)
	p := runfixture.Read(t, source).Plan
	p.APIVersion = launch.ArtifactPlanVersion
	p.Execution.CostPolicy = "retain_unknown"
	p.Guardrails = &model.RuntimeGuardrails{Schema: launch.RuntimeGuardrailsSchema, MaxInflight: 1}
	root := filepath.Join(t.TempDir(), "guarded")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	// Pure reducer fixture uses its own snapshots; production calls it inside
	// the existing checkpoint transaction while holding the cell lease.
	s := runfixture.Read(t, root)
	a, err := beginCellAttempt(s, "a", "", 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s)
	if _, err := beginCellAttempt(s, "b", "", 2, time.Now()); err != launch.ErrNoWork {
		t.Fatalf("slot cap missed: %v", err)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("denied admission mutated state")
	}
	outcome := managed.CellOutcome{Fence: a.Fence, Err: &model.OutcomeError{Outcome: model.OutcomeUncertain, Err: errors.New("fixture interruption")}}
	if err := recordCellOutcome(s, outcome, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := managed.ManagedSettle(s, s.Tasks["a"], outcome); err != nil {
		t.Fatal(err)
	}
	if _, err := beginCellAttempt(s, "b", "", 2, time.Now()); err != launch.ErrNoWork {
		t.Fatalf("uncertain operation released capacity: %v", err)
	}
	// A replacement operation for the same task must not reuse the old hold.
	row := s.Tasks["a"]
	row.Generation++
	row.OperationID = model.OperationID(s.RunID, "a", row.Generation)
	row.Status, row.CostUnknown = "pending", false
	if err := launch.AdmitRuntimeGuardrails(s, "a"); err != launch.ErrNoWork {
		t.Fatalf("new generation reused retained slot: %v", err)
	}
}
func TestRuntimeEvidenceIsDeterministicAndRedacted(t *testing.T) {
	root := runfixture.ManagedStateFixture(t, "claude_cli", "1", 2)
	s := runfixture.Read(t, root)
	s.Tasks["a"].Status = "uncertain"
	s.Tasks["a"].CostUnknown = true
	s.Tasks["a"].Spec.Prompt = "SECRET_PROMPT"
	s.Tasks["a"].Result = map[string]string{"answer": "SECRET_OUTPUT"}
	s.Tasks["a"].Error = "SECRET_DIAGNOSTIC"
	first, err := launch.BuildRuntimeEvidence(s)
	if err != nil {
		t.Fatal(err)
	}
	second, err := launch.BuildRuntimeEvidence(s)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(first)
	again, _ := json.Marshal(second)
	if string(raw) != string(again) || first.Digest == "" || first.StateDigest == "" {
		t.Fatal("receipt identity not deterministic")
	}
	if strings.Contains(string(raw), "SECRET_") {
		t.Fatal("receipt leaked task content")
	}
	if len(first.Tasks[0].Unresolved) != 2 {
		t.Fatal("uncertain effects not visible")
	}
	s.Tasks["a"].Attempt++
	changed, _ := launch.BuildRuntimeEvidence(s)
	if changed.Digest == first.Digest {
		t.Fatal("receipt not bound to changed attempt")
	}
}
