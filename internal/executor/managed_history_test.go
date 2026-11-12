package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func managedHistoryFixture(t *testing.T) (*model.State, model.WorkRequest, model.ExecutionBinding) {
	t.Helper()
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	profile := catalog.Profiles["small"]
	profile.RequiredCapabilities = append(profile.RequiredCapabilities, "explicit_history")
	profile.Limits = model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}
	catalog.Profiles["small"] = profile
	b := checkpoint.ManagedExpectedBinding(catalog, "small")
	p := model.Plan{APIVersion: model.GovernedPlanVersion, Name: "history", MaxTasks: 3, TurnShape: &model.TurnShape{Roots: 1, MainTurns: 3}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, CostPolicy: "retain_unknown", RunBudgetUSD: "0"}}
	for i, id := range []string{"a", "b", "c"} {
		node := model.Task{ID: id, Prompt: "accepted prompt " + id, Requires: []string{"answer", artifactstore.TurnRequestField}, Turn: &model.TurnIdentity{AgentID: "lane", Role: "main", Index: i + 1}}
		if i > 0 {
			node.Turn.PreviousTask = []string{"a", "b"}[i-1]
			node.DependsOn = []string{node.Turn.PreviousTask}
		}
		p.Tasks = append(p.Tasks, node)
	}
	p.Tasks[2].DependsOn = append(p.Tasks[2].DependsOn, "a")
	s := &model.State{Version: 4, Plan: p, PlanHash: launch.ExecutionDigest(p), RunID: "history-run", Tasks: map[string]*model.TaskState{}}
	for _, node := range p.Tasks {
		s.Tasks[node.ID] = &model.TaskState{Spec: node, Status: "verified", Generation: 1, Attempt: 1, OperationID: model.OperationID(s.RunID, node.ID, 1), BindingDigest: b.Digest, Result: map[string]string{"answer": "accepted answer " + node.ID, artifactstore.TurnRequestField: `{"helpers":[{"prompt":"secret helper control"}]}`, "undeclared": "not an accepted artifact"}}
	}
	s.Tasks["c"].Status = "running"
	inputs, err := managed.InputsForTask(s, p.Tasks[2])
	if err != nil {
		t.Fatal(err)
	}
	req := request(s, p.Tasks[2], 1, inputs, model.Options{Timeout: time.Second, Budget: 0})
	return s, req, b
}

func TestManagedHistoryProjectionAndBoundaries(t *testing.T) {
	s, req, b := managedHistoryFixture(t)
	h, err := managed.BuildManagedHistory(s, req, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Entries) != 2 || h.Entries[0].TaskID != "a" || h.Entries[1].TaskID != "b" {
		t.Fatalf("wrong history %+v", h)
	}
	raw, _ := json.Marshal(h)
	if strings.Contains(string(raw), "secret helper") || strings.Contains(string(raw), "undeclared") {
		t.Fatal("control/undeclared data leaked")
	}
	again, err := managed.BuildManagedHistory(s, req, b)
	if err != nil || !reflect.DeepEqual(h, again) {
		t.Fatal("history nondeterministic", err)
	}
	s.Tasks["b"].Spec.Context = []string{"removed"}
	s.Plan.Context = map[string]string{"removed": "private"}
	s.PlanHash = launch.ExecutionDigest(s.Plan)
	h, err = managed.BuildManagedHistory(s, req, b)
	if err != nil || h.Boundary != "context_boundary" || len(h.Entries) != 0 {
		t.Fatalf("context did not narrow %+v %v", h, err)
	}
}

func TestManagedHistoryRejectsMissingOrStaleSources(t *testing.T) {
	for _, kind := range []string{"missing", "status", "agent", "operation", "output", "current"} {
		t.Run(kind, func(t *testing.T) {
			s, req, b := managedHistoryFixture(t)
			switch kind {
			case "missing":
				delete(s.Tasks, "a")
			case "status":
				s.Tasks["b"].Status = "produced"
			case "agent":
				s.Tasks["a"].Spec.Turn.AgentID = "other"
			case "operation":
				s.Tasks["a"].OperationID = "bad"
			case "output":
				delete(s.Tasks["a"].Result, "answer")
			case "current":
				req.OperationID = "stale"
			}
			if _, err := managed.BuildManagedHistory(s, req, b); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
}

func TestManagedHistoryLegacyBytesAndPlanGate(t *testing.T) {
	_, req, b := managedHistoryFixture(t)
	b.Profile.RequiredCapabilities = []string{"structured_strings", "outcome_certainty"}
	expected, _ := json.Marshal(req)
	actual, err := managed.ManagedInput(req, b, nil)
	if err != nil || string(expected) != string(actual) {
		t.Fatal("legacy wire changed", err)
	}
	if !reflect.DeepEqual(launch.ExecutionProfileCapabilities("mock", b.Profile), launch.ExecutionCapabilities("mock")) {
		t.Fatal("legacy capability list changed")
	}
	s, _, _ := managedHistoryFixture(t)
	s.Plan.APIVersion = launch.UnknownCostPlanVersion
	if err := checkpoint.ValidateManagedPlan(s.Plan); err == nil {
		t.Fatal("history enabled on old plan")
	}
}

func TestManagedHistoryAdapterDeliveryAndReceipt(t *testing.T) {
	for _, kind := range []string{"claude_cli", "codex_cli"} {
		t.Run(kind, func(t *testing.T) {
			var b model.ExecutionBinding
			var req model.WorkRequest
			var dir, prefix string
			if kind == "claude_cli" {
				b, req, dir, prefix = runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.01}'`)
			} else {
				b, req, dir, prefix = codexAdapterFixture(t, runfixture.CodexFixtureSuccess)
			}
			b.Profile.RequiredCapabilities = append(b.Profile.RequiredCapabilities, "explicit_history")
			b.SupportedCapabilities = launch.ExecutionProfileCapabilities(kind, b.Profile)
			b.Digest = ""
			b.Digest = launch.ExecutionDigest(b)
			req.Task.Turn = &model.TurnIdentity{AgentID: "lane", Role: "main", Index: 2, PreviousTask: "first"}
			output := map[string]string{"answer": "earlier"}
			h := &managed.ManagedHistory{Schema: "irrigator.managed-history/v1", RunID: req.RunID, PlanSHA256: strings.Repeat("a", 64), AgentID: "lane", TaskID: req.Task.ID, OperationID: req.OperationID, Attempt: req.Attempt, Index: 2, BindingDigest: b.Digest, Boundary: "start_of_agent", Entries: []managed.ManagedHistoryEntry{{TaskID: "first", OperationID: model.OperationID(req.RunID, "first", 1), Generation: 1, Attempt: 1, Index: 1, Prompt: "previous accepted task", Output: output, OutputSHA256: launch.ExecutionDigest(output)}}}
			h.Digest = managed.ManagedHistoryDigest(*h)
			response, err := managed.ManagedWorkWithHistory(context.Background(), dir, prefix, req, b, h)
			if err != nil || response.Output["answer"] != "ok" {
				t.Fatal(response, err)
			}
			input, err := os.ReadFile(prefix + ".input.json")
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if json.Unmarshal(input, &payload) != nil || len(payload["task"]) == 0 || len(payload["history"]) == 0 {
				t.Fatal("history did not preserve top-level task")
			}
			var receipt managed.ManagedDispatchReceipt
			raw, err := os.ReadFile(prefix + ".managed.json")
			if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Schema != "irrigator.managed-dispatch/v2" || receipt.HistorySHA256 != h.Digest || receipt.InputSHA256 != managed.ManagedBytesDigest(input) || receipt.OutputSchemaSHA256 == "" {
				t.Fatalf("wrong receipt %s %v", raw, err)
			}
			captured := "captured.json"
			if kind == "codex_cli" {
				captured = "request.json"
			}
			delivered, err := os.ReadFile(filepath.Join(dir, captured))
			if err != nil || string(input) != string(delivered) {
				t.Fatal("receipt did not hash exact submitted stdin", err)
			}
		})
	}
}

func TestManagedHistoryInputBoundBeforeLaunch(t *testing.T) {
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, `exit 0`)
	b.Profile.RequiredCapabilities = append(b.Profile.RequiredCapabilities, "explicit_history")
	b.Profile.Limits.InputBytes = 1024
	b.Digest = ""
	b.Digest = launch.ExecutionDigest(b)
	req.Task.Turn = &model.TurnIdentity{AgentID: "lane", Role: "main", Index: 2, PreviousTask: "first"}
	out := map[string]string{"answer": strings.Repeat("x", 2048)}
	h := &managed.ManagedHistory{Schema: "irrigator.managed-history/v1", RunID: req.RunID, PlanSHA256: strings.Repeat("a", 64), AgentID: "lane", TaskID: req.Task.ID, OperationID: req.OperationID, Attempt: req.Attempt, Index: 2, BindingDigest: b.Digest, Boundary: "start_of_agent", Entries: []managed.ManagedHistoryEntry{{TaskID: "first", OperationID: model.OperationID(req.RunID, "first", 1), Generation: 1, Attempt: 1, Index: 1, Prompt: "old", Output: out, OutputSHA256: launch.ExecutionDigest(out)}}}
	h.Digest = managed.ManagedHistoryDigest(*h)
	_, err := managed.ManagedWorkWithHistory(context.Background(), dir, prefix, req, b, h)
	if !model.KnownNotStarted(err) {
		t.Fatal("overflow not rejected before launch", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "captured.json")); !os.IsNotExist(err) {
		t.Fatal("overflow executed provider")
	}
}

func TestManagedHistoryNeverRestoresExcludedOlderFields(t *testing.T) {
	s, req, b := managedHistoryFixture(t)
	s.Tasks["a"].Spec.Requires = append(s.Tasks["a"].Spec.Requires, "secret")
	s.Tasks["a"].Result["secret"] = "excluded confidential artifact"
	s.Tasks["b"].Spec.Prompt = "Previous prompt contains excluded confidential artifact"
	s.Tasks["c"].Spec.DependsOn = []string{"b"}
	req.Task = s.Tasks["c"].Spec
	var err error
	req.Dependencies, err = managed.InputsForTask(s, req.Task)
	if err != nil {
		t.Fatal(err)
	}
	h, err := managed.BuildManagedHistory(s, req, b)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(h)
	if h.Boundary != "external_input_boundary" || len(h.Entries) != 0 || strings.Contains(string(raw), "excluded confidential") {
		t.Fatalf("older output authority expanded: %s", raw)
	}
}
