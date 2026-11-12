//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

func completionFailedOwnerWithVerifiedHelper(t *testing.T) (string, string) {
	t.Helper()
	main := runfixture.Task("a", 1.0)
	main.Equals = map[string]string{"answer": "unreachable-value"}
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, main)
	_, done := clitest.Start(t, root, "--workers", "2")
	waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
	owner := runfixture.Read(t, root).Tasks["a"]
	if err := graph.AdmitTurnHelpers(root, "a", owner.OperationID, owner.Attempt, graph.TurnHelperRequest{ID: "live", Helpers: []model.Task{runfixture.Task("h", 0)}}); err != nil {
		t.Fatal(err)
	}
	id := "a__turn1__helper_h"
	waitFor(t, root, func(s *model.State) bool { return s.Tasks[id] != nil && s.Tasks[id].Status == "verified" })
	clitest.Finished(t, done, false)
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "failed" || s.Tasks[id].Status != "verified" {
		t.Fatal("fixture did not reach owner failure and verified helper")
	}
	return root, id
}

func TestManagedCompletionOwnerRetryPreservesAsyncChild(t *testing.T) {
	for _, mode := range []string{"explicit-node", "retry-failed"} {
		t.Run(mode, func(t *testing.T) {
			root, child := completionFailedOwnerWithVerifiedHelper(t)
			before := runfixture.Read(t, root)
			childBefore, _ := json.Marshal(before.Tasks[child])
			if mode == "explicit-node" {
				err := retryNode(root, "a", false, false)
				if err != nil {
					after, readErr := checkpoint.ReadState(root)
					if readErr != nil {
						t.Fatalf("rejected retry corrupted checkpoint: %v", readErr)
					}
					if after.Tasks["a"].Status != "failed" || after.Tasks["a"].OperationID != before.Tasks["a"].OperationID {
						t.Fatal("rejected retry mutated owner")
					}
					return
				}
				if err := graph.ReconcileParents(root); err != nil {
					t.Fatal(err)
				}
			} else {
				_, _ = runops.Run(root, model.Options{Workers: 2, Timeout: 5 * time.Second, Budget: 100, Retry: true})
			}
			after, err := checkpoint.ReadState(root)
			if err != nil {
				t.Fatalf("owner retry corrupted checkpoint: %v", err)
			}
			childAfter, _ := json.Marshal(after.Tasks[child])
			if string(childBefore) != string(childAfter) {
				t.Fatal("verified helper history changed")
			}
			if after.Tasks["a"].Status == "verified" {
				t.Fatal("failed owner falsely verified solely because helper verified")
			}
			if mode == "retry-failed" && after.Tasks["a"].Attempt != 2 {
				t.Fatal("retry-failed did not actually rerun owner")
			}
		})
	}
}

func TestManagedCompletionCancelOwnerFencesRunningAsyncChild(t *testing.T) {
	main := runfixture.Task("a", 1.5)
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, main)
	_, done := clitest.Start(t, root, "--workers", "2")
	waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
	owner := runfixture.Read(t, root).Tasks["a"]
	if err := graph.AdmitTurnHelpers(root, "a", owner.OperationID, owner.Attempt, graph.TurnHelperRequest{ID: "live", Helpers: []model.Task{runfixture.Task("h", 1.5)}}); err != nil {
		t.Fatal(err)
	}
	child := "a__turn1__helper_h"
	waitFor(t, root, func(s *model.State) bool { return s.Tasks[child] != nil && s.Tasks[child].Status == "running" })
	if err := runops.CancelNode(root, "a"); err != nil {
		t.Fatal(err)
	}
	cancelled := runfixture.Read(t, root)
	if !cancelled.Tasks["a"].CancelRequested || !cancelled.Tasks[child].CancelRequested {
		t.Fatal("owner cancellation did not fence its running helper")
	}
	if err := retryNode(root, "a", false, false); err == nil {
		t.Fatal("owner retry admitted while controller/child still live")
	}
	clitest.Finished(t, done, false)
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status == "verified" || s.Tasks[child].Status == "verified" {
		t.Fatal("cancelled unfinished execution verified")
	}
	if s.Tasks["a"].OperationID != owner.OperationID || s.Tasks[child].Spec.Turn.Async.OperationID != owner.OperationID {
		t.Fatal("cancellation rewrote admission lineage")
	}
}

func TestManagedCompletionManagedControllerMixedKinds(t *testing.T) {
	base, _ := runfixture.CompletionManagedFixture(t, `printf '%s' '{"type":"result","model":"fixture-model","structured_output":{"answer":"ok"},"total_cost_usd":0.01}'`)
	p := runfixture.Read(t, base).Plan
	p.Execution.Catalog.Adapters["free"] = model.ExecutionAdapter{Kind: "mock", Protocol: model.ProtocolVersion}
	p.Execution.Catalog.Instances["local"] = model.ExecutionInstance{Adapter: "free", CredentialRefs: []model.ExecutionCredentialRef{}}
	p.Execution.Catalog.Profiles["free"] = model.ExecutionProfile{Instance: "local", Model: "mock", Effort: "none", RequiredCapabilities: []string{"structured_strings"}, Limits: model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}, Tools: []string{}, OperationBudgetUSD: "0"}
	p.Execution.AllowedProfiles = []string{"small", "free"}
	p.Tasks[1].Profile = "free"
	root := filepath.Join(t.TempDir(), "mixed")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, root, "--workers", "2")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 10000 || s.Tasks["a"].Status != "verified" || s.Tasks["b"].Status != "verified" {
		t.Fatal("controller failed mixed adapter execution")
	}
	for id, row := range s.Tasks {
		raw, err := os.ReadFile(filepath.Join(root, "cells", id, "1.managed.json"))
		if err != nil {
			t.Fatal(err)
		}
		var receipt managed.ManagedDispatchReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.BindingDigest != row.BindingDigest || receipt.TaskID != id || receipt.OperationID != row.OperationID || receipt.ReportedCostUSD == nil || receipt.Outcome != model.OutcomeSettled {
			t.Fatal("managed receipt lost cell binding")
		}
		if id == "a" && (receipt.ReportedModel == nil || *receipt.ReportedModel != "fixture-model") {
			t.Fatal("provider-reported model not captured")
		}
		if id == "b" && receipt.ReportedModel != nil {
			t.Fatal("mock fabricated provider model")
		}
	}
}

func TestManagedCompletionManagedLegacyAndBudgetCLI(t *testing.T) {
	root, _ := runfixture.CompletionManagedFixture(t, `exit 0`)
	before, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if code, err := runCommand([]string{"resume", root, "--budget", "0.1"}); err == nil || code == 0 {
		t.Fatal("managed --budget silently accepted")
	}
	after, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected CLI budget changed checkpoint")
	}
	binary := os.Getenv("IRRIGATOR_LEGACY_REVIEW_BINARY")
	if binary == "" {
		t.Skip("optional previous-release binary not supplied")
	}
	out, err := exec.Command(binary, "status", root).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "unsupported checkpoint version 2") {
		t.Fatalf("old binary did not reject managed checkpoint: %s %v", out, err)
	}
}
