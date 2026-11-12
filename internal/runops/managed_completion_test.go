package runops

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Concurrent callers cannot overspend one remaining slot.
func TestManagedCompletionConcurrentHelperAllowance(t *testing.T) {
	root := runfixture.RunningHelperFixture(t)
	row := runfixture.Read(t, root).Tasks["a"]
	if err := graph.AdmitTurnHelpers(root, "a", row.OperationID, 1, graph.TurnHelperRequest{ID: "reserved", Helpers: []model.Task{runfixture.Task("reserved", 0)}}); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"left", "right"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-ready
			results <- graph.AdmitTurnHelpers(root, "a", row.OperationID, 1, graph.TurnHelperRequest{ID: id, Helpers: []model.Task{runfixture.Task(id, 0)}})
		}(id)
	}
	close(ready)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	s := runfixture.Read(t, root)
	if success != 1 || len(s.Tasks) != 3 || len(s.Tasks["a"].Children) != 2 {
		t.Fatalf("allowance lost: successful=%d tasks=%d", success, len(s.Tasks))
	}
	if err := checkpoint.ValidateTurnState(s); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCompletionHelperStrictCLI(t *testing.T) {
	root := runfixture.RunningHelperFixture(t)
	row := runfixture.Read(t, root).Tasks["a"]
	for _, raw := range []string{`{"id":"x","id":"y","helpers":[]}`, `{"id":"x","Helpers":[]}`, `{"id":"x","helpers":[],"unexpected":true}`, `{"id":"x","helpers":[]} {}`} {
		path := filepath.Join(t.TempDir(), "request.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if code, err := graph.TurnHelpersCLI([]string{root, "a", "--operation", row.OperationID, "--attempt", "1", "--request", path}); err == nil || code == 0 {
			t.Fatalf("accepted invalid JSON %s", raw)
		}
	}
	if len(runfixture.Read(t, root).Tasks) != 1 {
		t.Fatal("invalid CLI changed graph")
	}
}

func TestManagedCompletionAsyncRevisionImmutable(t *testing.T) {
	root := runfixture.RunningHelperFixture(t)
	row := runfixture.Read(t, root).Tasks["a"]
	if err := graph.AdmitTurnHelpers(root, "a", row.OperationID, 1, graph.TurnHelperRequest{ID: "batch", Helpers: []model.Task{runfixture.Task("h", 0)}}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	tasks := s.AllTasks()
	for i := range tasks {
		if tasks[i].Turn.Role == "helper" {
			tasks[i].Prompt = "rewrite pending helper"
		}
	}
	if graph.ValidateTurnRevision(s, tasks) == nil {
		t.Fatal("pending async helper receipt rewritten")
	}
}

func TestManagedCompletionTurnRevisionCapabilities(t *testing.T) {
	for _, kind := range []string{"context", "provider", "command", "output"} {
		t.Run(kind, func(t *testing.T) {
			main := runfixture.Task("a", 0)
			main.Outputs = map[string]string{"answer": "answer.txt"}
			root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, main)
			s := runfixture.Read(t, root)
			raw, _ := json.Marshal(s.AllTasks())
			var tasks []model.Task
			_ = json.Unmarshal(raw, &tasks)
			switch kind {
			case "context":
				tasks[0].Context = []string{"secret"}
			case "provider":
				tasks[0].Provider = "other"
			case "command":
				tasks[0].Checks = append(tasks[0].Checks, model.Check{Kind: "command", Command: []string{"false"}})
			case "output":
				tasks[0].Outputs["answer"] = "elsewhere.txt"
			}
			if graph.ValidateTurnRevision(s, tasks) == nil {
				t.Fatal("revision expanded capability or replaced output contract")
			}
		})
	}
}

func TestManagedCompletionConversationAmbiguityAndBounds(t *testing.T) {
	req := runfixture.ConversationRequest("a2", "a", 2, "a1")
	output := map[string]string{"answer": "accepted"}
	req.Dependencies = map[string]map[string]string{"a1": output}
	base := model.ProviderConversation{RunID: req.RunID, AgentID: "a", TaskID: "a1", Index: 1, ContextHash: launch.WorkerContextInputDigest(req.Context, nil), Entries: []model.ProviderConversationEntry{{TaskID: "a1", Prompt: "first", Output: output}}}
	base.Digest = provider.ConversationDigest(base)
	receipt := model.ProviderReceipt{Status: "completed", Response: model.WorkResponse{Output: output}, Conversation: &base}
	p := &provider.ProviderPool{Ledger: observer.ProviderLedger{Receipts: map[string]model.ProviderReceipt{"one": receipt, "two": receipt}}}
	if _, err := p.PrepareConversation(req); err == nil {
		t.Fatal("ambiguous accepted predecessor chosen nondeterministically")
	}
	delete(p.Ledger.Receipts, "two")
	base.Index = 2
	base.Digest = provider.ConversationDigest(base)
	if _, err := p.PrepareConversation(req); err == nil {
		t.Fatal("wrong predecessor index accepted")
	}
	base.Index = 1
	base.Digest = provider.ConversationDigest(base)
	req.Task.Prompt = strings.Repeat("x", model.MaxInputBytes)
	if _, err := p.PrepareConversation(req); err == nil {
		t.Fatal("oversized persisted conversation accepted")
	}
	if _, err := provider.ProviderTaskPrompt(req, nil); err == nil {
		t.Fatal("oversized dispatched prompt accepted")
	}
}

// Store level: the checkpoint lock must make duplicate
// outcome settlement atomic, not merely rely on a caller's cell lease.
func TestManagedCompletionCellConcurrentSettlement(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("cell", 0))
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	store := executor.CheckpointCells{Root: root}
	a, err := store.BeginAttempt("cell", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-ready
			results <- store.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: .25, Result: map[string]string{"answer": "kept"}})
		}()
	}
	close(ready)
	success := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			success++
		}
	}
	s := runfixture.Read(t, root)
	if success != 1 || s.Tasks["cell"].Cost != .25 || s.Tasks["cell"].Status != "produced" {
		t.Fatal("duplicate settlement changed cost or state")
	}
	before, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	stale := a.Fence
	stale.Attempt++
	if err := store.RecordValidation(executor.CellValidation{Fence: stale, CleanupUnresolved: true}); err == nil {
		t.Fatal("stale cleanup committed")
	}
	after, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected command changed checkpoint bytes")
	}
}

// Exact attempt fencing must cover both expansion implementations, even when
// the logical operation has not changed. No stale candidate may admit children.
func TestManagedCompletionCellStaleExpansion(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			var root string
			if version == "v1" {
				root = runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("child", 0)))
			} else {
				root = runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, runfixture.TurnRequestTask(t, "parent", checkpoint.TurnRequest{Helpers: []model.Task{runfixture.Task("child", 0)}}))
			}
			runfixture.StageExpansion(t, root, "parent")
			s := runfixture.Read(t, root)
			fence := graph.FenceForCell(s, s.Tasks["parent"])
			if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["parent"].Attempt++; return nil }); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(root, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if version == "v1" {
				err = graph.CommitExpansionFenced(root, "parent", "structural", &fence)
			} else {
				err = graph.CommitTurnExpansionFenced(root, "parent", "structural", &fence)
			}
			if err == nil {
				t.Fatal("stale validation admitted graph")
			}
			after, err := os.ReadFile(filepath.Join(root, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || len(runfixture.Read(t, root).Tasks) != 1 {
				t.Fatal("stale expansion changed checkpoint")
			}
		})
	}
}

func TestManagedCompletionCellCancelledExpansionRecovery(t *testing.T) {
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("child", 0)))
	runfixture.StageExpansion(t, root, "parent")
	if err := commitExpansion(root, "parent", "structural"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["parent"].Status = "produced"
		s.Tasks["parent"].CancelRequested = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := executor.Finalize(root, "parent"); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).Tasks["parent"].Status != "cancelled" {
		t.Fatal("idempotent expansion recovery bypassed cancellation")
	}
}

func TestManagedCompletionProfileBindingConstraints(t *testing.T) {
	for _, field := range []string{"credential_ref", "model", "effort", "input_limit", "output_limit", "capability", "budget"} {
		t.Run(field, func(t *testing.T) {
			path, c, marker := runfixture.ExecutionFixture(t, "claude_cli")
			before, err := launch.ResolveExecutionProfile(path, "")
			if err != nil {
				t.Fatal(err)
			}
			p := c.Profiles["small"]
			switch field {
			case "credential_ref":
				i := c.Instances["account"]
				i.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "env", Name: "DIFFERENT_SECRET_REF"}}
				c.Instances["account"] = i
			case "model":
				p.Model = "other-model"
			case "effort":
				p.Effort = "high"
			case "input_limit":
				p.Limits.InputBytes++
			case "output_limit":
				p.Limits.OutputBytes++
			case "capability":
				p.RequiredCapabilities = append(p.RequiredCapabilities, "explicit_history")
			case "budget":
				p.OperationBudgetUSD = "0.250001"
			}
			c.Profiles["small"] = p
			runfixture.WriteExecutionFixture(t, path, c)
			after, err := launch.ResolveExecutionProfile(path, "")
			if err != nil {
				t.Fatal(err)
			}
			if before.Digest == after.Digest || before.CatalogSHA256 == after.CatalogSHA256 {
				t.Fatal("resolved constraint omitted from digest")
			}
			if after.AuthVerified || after.Dispatched || after.ActualModel != nil {
				t.Fatal("offline resolver claimed live proof")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("resolver executed adapter")
			}
		})
	}
}

func TestManagedCompletionProfileExecutableReplacement(t *testing.T) {
	path, c, _ := runfixture.ExecutionFixture(t, "claude_cli")
	before, err := launch.ResolveExecutionProfile(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Adapters["adapter"].Command[0], []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.ResolveExecutionProfile(path, ""); err == nil {
		t.Fatal("changed executable accepted under original pin")
	}
	if !before.ExecutableVerified || before.Dispatched {
		t.Fatal("prior immutable resolution unexpectedly changed")
	}
}

func TestManagedCompletionManagedLastReservation(t *testing.T) {
	root, _ := runfixture.CompletionManagedFixture(t, `sleep 0.1; printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.25}'`)
	ready := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		go func(id string) { <-ready; results <- worker(root, id, model.Options{Timeout: 5 * time.Second}) }(id)
	}
	close(ready)
	for k := 0; k < 2; k++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	s := runfixture.Read(t, root)
	dispatched, verified, attempts := 0, 0, 0
	for id, row := range s.Tasks {
		attempts += row.Attempt
		if row.Status == "verified" {
			verified++
		}
		if _, err := os.Stat(filepath.Join(root, "cells", id, "invoked.json")); err == nil {
			dispatched++
		}
	}
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 250000 || attempts != 1 || dispatched != 1 || verified != 1 || len(s.Execution.Reservations) != 1 {
		t.Fatalf("last reservation violated: used=%d attempts=%d dispatched=%d verified=%d err=%v", used, attempts, dispatched, verified, err)
	}
}

func TestManagedCompletionManagedUnknownHold(t *testing.T) {
	root, _ := runfixture.CompletionManagedFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"}}'`)
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "uncertain" {
		t.Fatalf("missing cost became %s", s.Tasks["a"].Status)
	}
	if err := worker(root, "b", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	s = runfixture.Read(t, root)
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 250000 || s.Tasks["b"].Attempt != 0 {
		t.Fatal("unknown cost released shared admission")
	}
	if _, err := os.Stat(filepath.Join(root, "cells", "b", "invoked.json")); !os.IsNotExist(err) {
		t.Fatal("blocked peer dispatched")
	}
}

func TestManagedCompletionManagedNotStartedRelease(t *testing.T) {
	root, c := runfixture.CompletionManagedFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.25}'`)
	exe := c.Adapters["adapter"].Command[0]
	original, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 0 || s.Tasks["a"].Status != "failed" {
		t.Fatal("positive predispatch rejection did not release hold")
	}
	if _, err := os.Stat(filepath.Join(root, "cells", "a", "invoked.json")); !os.IsNotExist(err) {
		t.Fatal("changed executable dispatched")
	}
	if err := os.WriteFile(exe, original, 0700); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "b", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).Tasks["b"].Status != "verified" {
		t.Fatal("released allowance not usable by peer")
	}
}

func TestManagedCompletionManagedPlanStrictness(t *testing.T) {
	original, err := os.ReadFile("../../examples/managed-profiles-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"duplicate_budget": strings.Replace(string(original), `"run_budget_usd": "0"`, `"run_budget_usd": "1", "run_budget_usd": "0"`, 1),
		"alias_profile":    strings.Replace(string(original), `"profile": "writer"`, `"Profile": "writer"`, 1),
		"null_tools":       strings.Replace(string(original), `"tools": []`, `"tools": null`, 1),
		"missing_tools":    strings.Replace(string(original), `"tools": [],`, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := checkpoint.LoadPlan(p); err == nil {
				t.Fatal("accepted ambiguous or implicit managed policy")
			}
		})
	}
}

func TestManagedCompletionManagedCorruptCertainty(t *testing.T) {
	root, _ := runfixture.CompletionManagedFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"}}'`)
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	r := s.Execution.Reservations[s.Tasks["a"].OperationID]
	r.Status = "settled"
	r.ReportedMicros = 0
	s.Execution.Reservations[r.OperationID] = r
	if err := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.ReadState(root); err == nil {
		t.Fatal("uncertain task admitted settled-zero receipt and released hold")
	}
}

func TestManagedCompletionManagedValidationUncertaintyAndResolve(t *testing.T) {
	for _, kind := range []string{"cleanup", "validator"} {
		t.Run(kind, func(t *testing.T) {
			root, _ := runfixture.CompletionManagedFixture(t, `exit 0`)
			store := executor.CheckpointCells{Root: root}
			a, err := store.BeginAttempt("a", "", 123)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: .01, Result: map[string]string{"answer": "kept"}}); err != nil {
				t.Fatal(err)
			}
			v := executor.CellValidation{Fence: a.Fence}
			if kind == "cleanup" {
				v.CleanupUnresolved = true
			} else {
				v.Err = model.WithOutcome(model.OutcomeUncertain, context.DeadlineExceeded)
			}
			if err := store.RecordValidation(v); err != nil {
				t.Fatal(err)
			}
			s, err := checkpoint.ReadState(root)
			if err != nil {
				t.Fatalf("store wrote unreadable uncertainty: %v", err)
			}
			used, err := checkpoint.ManagedBudgetUsed(s.Execution)
			if err != nil || used != 250000 || s.Tasks["a"].Status != "uncertain" {
				t.Fatal("uncertain validation did not restore conservative hold")
			}
			if err := ResolveNode(root, "a", map[string]string{"answer": "reconciled"}); err != nil {
				t.Fatal(err)
			}
			s = runfixture.Read(t, root)
			used, err = checkpoint.ManagedBudgetUsed(s.Execution)
			if err != nil || used != 250000 || s.Tasks["a"].Status != "verified" {
				t.Fatal("result reconciliation changed unknown cost hold")
			}
			if err := worker(root, "b", model.Options{Timeout: time.Second}); err != nil {
				t.Fatal(err)
			}
			if runfixture.Read(t, root).Tasks["b"].Attempt != 0 {
				t.Fatal("reconciliation admitted another paid execution")
			}
		})
	}
}

func TestManagedCompletionCodexAcceptedUnknownHold(t *testing.T) {
	root := runfixture.CompletionCodexFixture(t, "0.25", false)
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	row := s.Tasks["a"]
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || s.Version != 3 || row.Status != "verified" || !row.CostUnknown || used != 250000 || s.Execution.Reservations[row.OperationID].Status != "unknown_cost" {
		t.Fatal("accepted Codex result treated as known zero spend")
	}
	var receipt managed.ManagedDispatchReceipt
	raw, err := os.ReadFile(filepath.Join(root, "cells", "a", "1.managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &receipt) != nil || !receipt.CostUnknown || receipt.ReportedCostUSD != nil || receipt.TokenUsage == nil || receipt.TokenUsage.OutputTokens != 10 {
		t.Fatal("Codex receipt fabricated USD or lost token usage")
	}
	if err := worker(root, "b", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).Tasks["b"].Attempt != 0 {
		t.Fatal("unknown-cost accepted output freed peer budget")
	}
	binary := os.Getenv("IRRIGATOR_MANAGED_V3_REVIEW_BINARY")
	if binary != "" {
		out, err := exec.Command(binary, "status", root).CombinedOutput()
		if err == nil || !strings.Contains(string(out), "unsupported checkpoint version 3") {
			t.Fatalf("old managed binary accepted3: %s %v", out, err)
		}
	}
}

func TestManagedCompletionCodexUnknownCostRetry(t *testing.T) {
	root := runfixture.CompletionCodexFixture(t, "0.50", true)
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	first := runfixture.Read(t, root).Tasks["a"]
	oldOperation := first.OperationID
	if first.Status != "failed" || !first.CostUnknown {
		t.Fatalf("fixture did not produce failed acceptance with unknown USD: row=%+v", first)
	}
	if err := retryNode(root, "a", false, false); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "a", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 500000 || s.Tasks["a"].Attempt != 2 || s.Tasks["a"].Status != "failed" || s.Execution.Reservations[oldOperation].Status != "unknown_cost" {
		t.Fatal("explicit retry lost historic unknown hold or failed current operation")
	}
	summary := provider.ProjectCost(s)
	if summary.KnownReportedUSD != 0 || summary.UnknownOperations != 2 || summary.UnknownHeldUSD != .5 || !strings.Contains(provider.CostStatusText(s), "USD unknown: 2 operations") {
		t.Fatal("cost projection hid historical unknown exposure")
	}
}

func TestManagedCompletionCodexUnknownValidationResolve(t *testing.T) {
	root := runfixture.CompletionCodexFixture(t, "0.25", false)
	store := executor.CheckpointCells{Root: root}
	a, err := store.BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordValidation(executor.CellValidation{Fence: a.Fence, CleanupUnresolved: true}); err != nil {
		t.Fatal(err)
	}
	if err := ResolveNode(root, "a", map[string]string{"answer": "ok"}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	used, err := checkpoint.ManagedBudgetUsed(s.Execution)
	if err != nil || used != 250000 || !s.Tasks["a"].CostUnknown || s.Tasks["a"].Status != "verified" {
		t.Fatal("output resolution erased unknown Codex USD")
	}
}
