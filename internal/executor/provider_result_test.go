package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestManagedNonzeroCompleteErrorSettles(t *testing.T) {
	for _, result := range []string{
		`{"type":"result","is_error":true,"subtype":"error_max_turns","total_cost_usd":0.03}`,
		`{"type":"result","subtype":"error_during_execution","total_cost_usd":0.03}`,
	} {
		b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, "printf '%s' '"+result+"'; exit 1")
		response, err := managedWork(context.Background(), dir, prefix, req, b)
		if !managed.ManagedSettledError(err) || response.Outcome != model.OutcomeSettled || response.CostUSD != .03 || response.DiagnosticCode != model.DiagProviderError {
			t.Fatalf("complete error must settle: %+v %v", response, err)
		}
	}
}

func TestManagedExitFailureDoesNotTrustSuccessfulOutput(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.03}'; exit 1`)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if !model.IsUncertain(err) || response.CostUSD != .03 || response.Output != nil {
		t.Fatalf("contradictory exit must stay uncertain: %+v %v", response, err)
	}
}

func TestManagedDeadlineRetainsReason(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `sleep 5`)
	req.TimeoutMS = 100
	_, err := managedWork(context.Background(), dir, prefix, req, b)
	if !model.IsUncertain(err) || model.OutcomeReason(err) != "deadline" {
		t.Fatalf("deadline incorrectly classified: %v (%s)", err, model.OutcomeReason(err))
	}
}

func TestManagedAuthenticationAdviceWithoutInventedSettlement(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","is_error":true,"result":"Not logged in · Please run /login"}'; exit 1`)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if !model.IsUncertain(err) || response.DiagnosticCode != model.DiagProviderAuth {
		t.Fatalf("auth needs a diagnostic but absent billing stays unknown: %+v %v", response, err)
	}
}

func TestManagedZeroCostAuthFailureSettles(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","is_error":true,"result":"Not logged in · Please run /login","total_cost_usd":0}'; exit 1`)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if !managed.ManagedSettledError(err) || response.CostUSD != 0 || response.DiagnosticCode != model.DiagProviderAuth {
		t.Fatalf("known free auth failure must settle: %+v %v", response, err)
	}
}

func TestHaikuAuthDiagnosticReachesCheckpoint(t *testing.T) {
	runfixture.HaikuClaudeStub(t, `cat >/dev/null; printf '%s' '{"type":"result","is_error":true,"result":"Not logged in · Please run /login","total_cost_usd":0}'; exit 1`)
	root := runfixture.HaikuRoot(t)
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second, Budget: .25}); err != nil {
		t.Fatal(err)
	}
	row := runfixture.Read(t, root).Tasks["a"]
	if row.Status != "failed" || row.DiagnosticCode != model.DiagProviderAuth {
		t.Fatalf("auth diagnostic lost: %+v", row)
	}
}

func TestHaikuNonzeroCompleteErrorSettles(t *testing.T) {
	runfixture.HaikuClaudeStub(t, `cat >/dev/null; printf '%s' '{"type":"result","is_error":true,"total_cost_usd":0.03,"result":"failure"}'; exit 1`)
	_, cost, err := callHaiku(context.Background(), t.TempDir(), 1, runfixture.Task("a", 0), nil, .25)
	if !managed.ManagedSettledError(err) || cost != .03 {
		t.Fatalf("complete error lost settlement: cost=%v err=%v", cost, err)
	}
}

func TestProviderCommandCostHelper(t *testing.T) {
	mode := os.Getenv("IRRIGATOR_COST_FIXTURE")
	if mode == "" {
		return
	}
	var req model.WorkRequest
	if json.NewDecoder(os.Stdin).Decode(&req) != nil {
		os.Exit(2)
	}
	response := map[string]any{"protocol": model.ProtocolVersion, "request_id": req.RequestID, "output": map[string]string{"answer": "ok"}}
	switch mode {
	case "zero":
		response["cost_usd"] = 0
	case "over":
		response["cost_usd"] = .30
	case "null":
		response["cost_usd"] = nil
	case "negative":
		response["cost_usd"] = -1
	case "unavailable":
		response["cost_usd"] = "unavailable"
	case "invalid_outcome":
		response["cost_usd"], response["outcome"] = .07, "invalid"
	case "deadline":
		response["cost_usd"] = .07
	case "malformed":
		fmt.Print("{bad")
		os.Exit(0)
	case "alias":
		fmt.Printf(`{"protocol":"swarm.worker/v1","request_id":%q,"cost_usd":0.3,"COST_USD":0}`, req.RequestID)
		os.Exit(0)
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	if mode == "deadline" {
		time.Sleep(5 * time.Second)
	}
	os.Exit(0)
}

func TestProviderCommandCostsRequireExplicitEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "null", "zero", "over", "negative", "unavailable", "invalid_outcome", "deadline", "malformed", "alias"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("IRRIGATOR_COST_FIXTURE", mode)
			exe, _ := os.Executable()
			root := filepath.Join(t.TempDir(), "run")
			p := model.Plan{Tasks: []model.Task{runfixture.Task("a", 0)}, Providers: map[string]model.CommandSpec{"fixture": {Command: []string{exe, "-test.run=^TestProviderCommandCostHelper$"}, Env: []string{"IRRIGATOR_COST_FIXTURE"}, Replay: ""}}}
			if err := checkpoint.Initialize(p, root, "fixture"); err != nil {
				t.Fatal(err)
			}
			if err := checkpoint.SetControl(root, "run"); err != nil {
				t.Fatal(err)
			}
			timeout := 5 * time.Second
			if mode == "deadline" {
				timeout = time.Second
			}
			if err := worker(root, "a", model.Options{Timeout: timeout, Budget: .25}); err != nil {
				t.Fatal(err)
			}
			s := runfixture.Read(t, root)
			row := s.Tasks["a"]
			switch mode {
			case "missing", "null", "negative", "unavailable":
				if row.Status != "verified" || provider.LegacyUnknownOperations(s) != 1 || row.Result["answer"] != "ok" || row.Cost != 0 {
					t.Fatalf("unknown estimate blocked completed output or became free: %+v", row)
				}
			case "invalid_outcome", "deadline", "malformed", "alias":
				if row.Status != "uncertain" || provider.LegacyUnknownOperations(s) != 1 || row.Result != nil {
					t.Fatalf("missing invoice booked as zero: %+v", row)
				}
				if (mode == "deadline" || mode == "invalid_outcome") && row.Cost != .07 {
					t.Fatalf("lost partial cost evidence: %+v", row)
				}
			case "zero":
				if row.Status != "verified" || provider.LegacyUnknownOperations(s) != 0 {
					t.Fatalf("explicit zero rejected: %+v", row)
				}
			case "over":
				if row.Status != "failed" || row.Cost != .30 || row.DiagnosticCode != model.DiagProviderOperationBudget {
					t.Fatalf("cap not enforced/accounted: %+v", row)
				}
			}
		})
	}
}

func TestProviderCommandCostCannotBeOverriddenByAliasesOrDuplicates(t *testing.T) {
	for _, cost := range []string{`"cost_usd":0.3,"COST_USD":0`, `"cost_usd":0.3,"cost_usd":0`} {
		_, err := callOutcomeExternal(t, "task", "", `{"protocol":"swarm.worker/v1","request_id":"rid","output":{"answer":"ok"},`+cost+`}`)
		if !model.IsUncertain(err) {
			t.Fatalf("ambiguous charge accepted: %v", err)
		}
	}
}
