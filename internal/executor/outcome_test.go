package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Independent subprocess sends raw protocol JSON, so tests do not depend on the implementation's enum type.
func TestOutcomeProviderHelper(t *testing.T) {
	if os.Getenv("IRRIGATOR_INDEPENDENT_OUTCOME") == "" {
		return
	}
	var req model.WorkRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Exit(2)
	}
	outcome := os.Getenv("IRRIGATOR_INDEPENDENT_OUTCOME")
	response := map[string]any{"protocol": model.ProtocolVersion, "request_id": req.RequestID, "outcome": outcome, "cost_usd": 0.03}
	if outcome == "invented" {
		response["output"] = map[string]string{"answer": "looks valid"}
	} else {
		response["error"] = "test provider failure"
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	os.Exit(0)
}

func outcomeRun(t *testing.T, outcome string) *model.TaskState {
	t.Helper()
	t.Setenv("IRRIGATOR_INDEPENDENT_OUTCOME", outcome)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec := runfixture.Task("a", 0)
	root := filepath.Join(t.TempDir(), "run")
	p := model.Plan{Tasks: []model.Task{spec}, Providers: map[string]model.CommandSpec{"fixture": {Command: []string{exe, "-test.run=^TestOutcomeProviderHelper$"}, Env: []string{"IRRIGATOR_INDEPENDENT_OUTCOME"}, Replay: "safe"}}}
	if err := checkpoint.Initialize(p, root, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second, Budget: 0.1}); err != nil {
		t.Fatal(err)
	}
	return runfixture.Read(t, root).Tasks["a"]
}
func TestOutcomeExplicitUncertaintyOverridesReplaySafe(t *testing.T) {
	row := outcomeRun(t, "uncertain")
	if row.Status != "uncertain" {
		t.Fatalf("paid uncertainty downgraded: status=%s error=%s", row.Status, row.Error)
	}
	if row.Cost != 0.03 {
		t.Fatalf("known reported cost lost: %v", row.Cost)
	}
}
func TestOutcomeMalformedOutcomeCannotVerify(t *testing.T) {
	row := outcomeRun(t, "invented")
	if row.Status != "uncertain" {
		t.Fatalf("unknown protocol certainty must fail closed; got %s", row.Status)
	}
}
func TestOutcomeSettledFailureKeepsCost(t *testing.T) {
	row := outcomeRun(t, "settled")
	if row.Status != "failed" || row.Cost != 0.03 {
		t.Fatalf("known failure accounting changed: %+v", row)
	}
}
func TestOutcomePoolUncertaintySurvivesProtocol(t *testing.T) {
	p := fakeProviderPool(t)
	setFakeProviderMode(t, p, "eof")
	req := runfixture.FakeProviderRequest("uncertain-op")
	for i := 0; i < 2; i++ {
		response := p.Work(context.Background(), req)
		raw, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		_ = json.Unmarshal(raw, &wire)
		if wire["outcome"] != "uncertain" {
			t.Errorf("attempt %d: uncertain receipt lost at protocol boundary: %s", i, raw)
		}
		receipt := p.Ledger.Receipts[req.OperationID]
		if receipt.Status != "uncertain" || receipt.Reserved != req.MaxCostUSD {
			t.Fatalf("unknown hold changed: %+v", receipt)
		}
	}
}
func TestOutcomeBlockedDescendantDoesNotCompleteOwner(t *testing.T) {
	r := runfixture.LcRow("r", "", "waiting", "structural", nil, "r__m")
	m := runfixture.LcRow("r__m", "r", "waiting", "structural", nil, "r__m__bad", "r__m__ok")
	bad := runfixture.LcRow("r__m__bad", "r__m", "uncertain", "", nil)
	ok := runfixture.LcRow("r__m__ok", "r__m", "verified", "structural", nil)
	s := runfixture.LcState(r, m, bad, ok)
	before, _ := json.Marshal(s)
	projected := observer.ProjectLifecycles(s)
	if projected["r"].Join == "satisfied" || projected["r__m"].Join == "satisfied" {
		t.Fatal("uncertain subtree misrepresented as complete")
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("observer changed execution state")
	}
}
