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

func TestManagedHistoryLegacyIdentity(t *testing.T) {
	_, c, _ := runfixture.ExecutionFixture(t, "mock")
	b := checkpoint.ManagedExpectedBinding(c, "small")
	if b.Digest != "b2618428c872e6808e20cc58fa6728263da8785b937a34d329017f67a132d751" {
		t.Fatalf("nonopted legacy binding changed: %s", b.Digest)
	}
	req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "task", Task: model.Task{ID: "a", Prompt: "legacy", Requires: []string{"answer"}}, RequestID: "r", OperationID: "o", Attempt: 1, TimeoutMS: 1000}
	wire, e := json.Marshal(req)
	if e != nil {
		t.Fatal(e)
	}
	if string(wire) != `{"protocol":"swarm.worker/v1","kind":"task","request_id":"r","operation_id":"o","task":{"id":"a","prompt":"legacy","requires":["answer"]},"attempt":1,"timeout_ms":1000,"max_cost_usd":0}` {
		t.Fatalf("legacy wire changed: %s", wire)
	}
}

func historyFixture(t *testing.T) (*model.State, model.WorkRequest, model.ExecutionBinding) {
	t.Helper()
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	p := catalog.Profiles["small"]
	p.RequiredCapabilities = append(p.RequiredCapabilities, "explicit_history")
	p.Limits.InputBytes = 65536
	catalog.Profiles["small"] = p
	b := checkpoint.ManagedExpectedBinding(catalog, "small")
	first := model.Task{ID: "first", Profile: "small", Prompt: "first accepted prompt", Requires: []string{"answer", artifactstore.TurnRequestField}, Turn: &model.TurnIdentity{AgentID: "agent", Role: "main", Index: 1}}
	next := model.Task{ID: "second", Profile: "small", Prompt: "second prompt", Requires: []string{"answer"}, DependsOn: []string{"first"}, Turn: &model.TurnIdentity{AgentID: "agent", Role: "main", Index: 2, PreviousTask: "first"}}
	plan := model.Plan{APIVersion: model.GovernedPlanVersion, Name: "history review", Tasks: []model.Task{first}, Context: map[string]string{"secret": "raw-context-canary"}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, CostPolicy: "retain_unknown", RunBudgetUSD: "0"}}
	s := &model.State{Version: 4, RunID: "history-review", Plan: plan, PlanHash: launch.ExecutionDigest(plan), Tasks: map[string]*model.TaskState{}}
	s.Tasks["first"] = &model.TaskState{Spec: first, Status: "verified", Generation: 1, Attempt: 1, OperationID: model.OperationID(s.RunID, "first", 1), BindingDigest: b.Digest, Result: map[string]string{"answer": "accepted answer", artifactstore.TurnRequestField: "helper-control-canary", "undeclared": "undeclared-output-canary"}}
	s.Tasks["second"] = &model.TaskState{Spec: next, Status: "running", Generation: 1, Attempt: 1, OperationID: model.OperationID(s.RunID, "second", 1), BindingDigest: b.Digest}
	inputs, e := managed.InputsForTask(s, next)
	if e != nil {
		t.Fatal(e)
	}
	req := request(s, next, 1, inputs, model.Options{Timeout: time.Second})
	return s, req, b
}
func TestManagedHistoryProjectionAndFences(t *testing.T) {
	s, req, b := historyFixture(t)
	h, e := managed.BuildManagedHistory(s, req, b)
	if e != nil {
		t.Fatal(e)
	}
	if len(h.Entries) != 1 || h.Entries[0].Prompt != "first accepted prompt" || !reflect.DeepEqual(h.Entries[0].Output, map[string]string{"answer": "accepted answer"}) {
		t.Fatalf("wrong projection: %+v", h)
	}
	raw, _ := json.Marshal(h)
	for _, secret := range []string{"raw-context-canary", "helper-control-canary", "undeclared-output-canary"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("history leaked %s", secret)
		}
	}
	cases := map[string]func(*model.State, *model.WorkRequest){
		"wrong_agent":             func(s *model.State, r *model.WorkRequest) { s.Tasks["first"].Spec.Turn.AgentID = "other" },
		"unverified":              func(s *model.State, r *model.WorkRequest) { s.Tasks["first"].Status = "produced" },
		"missing_source":          func(s *model.State, r *model.WorkRequest) { delete(s.Tasks, "first") },
		"stale_operation":         func(s *model.State, r *model.WorkRequest) { s.Tasks["first"].OperationID = strings.Repeat("0", 64) },
		"missing_output":          func(s *model.State, r *model.WorkRequest) { delete(s.Tasks["first"].Result, "answer") },
		"changed_current_attempt": func(s *model.State, r *model.WorkRequest) { r.Attempt++ },
		"unadmitted_context":      func(s *model.State, r *model.WorkRequest) { r.Context = map[string]string{"inject": "forbidden"} },
		"old_plan": func(s *model.State, r *model.WorkRequest) {
			s.Plan.APIVersion = launch.UnknownCostPlanVersion
			s.PlanHash = launch.ExecutionDigest(s.Plan)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s, r, b := historyFixture(t)
			mutate(s, &r)
			if _, e := managed.BuildManagedHistory(s, r, b); e == nil {
				t.Fatal("invalid history accepted")
			}
		})
	}
}
func TestManagedHistoryNarrowingAndBounds(t *testing.T) {
	s, req, b := historyFixture(t)
	s.Tasks["first"].Spec.Context = []string{"secret"}
	h, e := managed.BuildManagedHistory(s, req, b)
	if e != nil {
		t.Fatal(e)
	}
	if h.Boundary != "context_boundary" || len(h.Entries) != 0 {
		t.Fatalf("context narrowing retained history: %+v", h)
	}
	s, req, b = historyFixture(t)
	s.Tasks["first"].Spec.Requires = append(s.Tasks["first"].Spec.Requires, "private")
	s.Tasks["first"].Result["private"] = "private answer"
	s.Tasks["second"].Spec.InputFields = map[string][]string{"first": {"answer"}}
	req.Task = s.Tasks["second"].Spec
	req.Dependencies, e = managed.InputsForTask(s, req.Task)
	if e != nil {
		t.Fatal(e)
	}
	h, e = managed.BuildManagedHistory(s, req, b)
	if e != nil {
		t.Fatal(e)
	}
	if h.Boundary != "output_selection_boundary" || len(h.Entries) != 0 {
		t.Fatal("selector narrowing retained private output")
	}
	s, req, b = historyFixture(t)
	h, e = managed.BuildManagedHistory(s, req, b)
	if e != nil {
		t.Fatal(e)
	}
	b.Profile.Limits.InputBytes = 1
	if _, e = managed.ManagedInput(req, b, h); e == nil {
		t.Fatal("oversized history silently truncated or accepted")
	}
}

func historyOptIn(b *model.ExecutionBinding, req *model.WorkRequest) *managed.ManagedHistory {
	b.Profile.RequiredCapabilities = append(b.Profile.RequiredCapabilities, "explicit_history")
	b.SupportedCapabilities = launch.ExecutionProfileCapabilities(b.Adapter.Kind, b.Profile)
	b.Digest = ""
	b.Digest = launch.ExecutionDigest(*b)
	req.Task.Turn = &model.TurnIdentity{AgentID: req.Task.ID, Role: "main", Index: 1}
	h := &managed.ManagedHistory{Schema: "irrigator.managed-history/v1", RunID: req.RunID, PlanSHA256: strings.Repeat("a", 64), AgentID: req.Task.ID, TaskID: req.Task.ID, OperationID: req.OperationID, Attempt: req.Attempt, Index: 1, BindingDigest: b.Digest, Boundary: "start_of_agent", Entries: []managed.ManagedHistoryEntry{}}
	h.Digest = managed.ManagedHistoryDigest(*h)
	return h
}
func TestManagedHistoryExactAdapterReceipts(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			var b model.ExecutionBinding
			var req model.WorkRequest
			var dir, prefix string
			capture := "captured.json"
			if kind == "codex" {
				b, req, dir, prefix = codexAdapterFixture(t, runfixture.CodexFixtureSuccess)
				capture = "request.json"
			} else {
				b, req, dir, prefix = runfixture.ManagedAdapterFixture(t, `printf '%s' '{"type":"result","structured_output":{"answer":"ok"},"total_cost_usd":0.01}'`)
			}
			h := historyOptIn(&b, &req)
			response, e := managed.ManagedWorkWithHistory(context.Background(), dir, prefix, req, b, h)
			if e != nil {
				t.Fatal(e)
			}
			if response.Output["answer"] != "ok" {
				t.Fatal("adapter result lost")
			}
			input, e := os.ReadFile(filepath.Join(dir, capture))
			if e != nil {
				t.Fatal(e)
			}
			var delivered struct {
				model.WorkRequest

				History *managed.ManagedHistory `json:"history"`
			}
			if e = json.Unmarshal(input, &delivered); e != nil {
				t.Fatal(e)
			}
			if delivered.History == nil || delivered.History.Digest != h.Digest {
				t.Fatal("history not delivered")
			}
			var receipt managed.ManagedDispatchReceipt
			raw, e := os.ReadFile(prefix + ".managed.json")
			if e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(raw, &receipt); e != nil {
				t.Fatal(e)
			}
			schema, _ := json.Marshal(artifactstore.ModelSchema(req.Task))
			if receipt.Schema != "irrigator.managed-dispatch/v2" || receipt.InputSHA256 != managed.ManagedBytesDigest(input) || receipt.HistorySHA256 != h.Digest || receipt.OutputSchemaSHA256 != managed.ManagedBytesDigest(schema) {
				t.Fatalf("receipt does not attest exact delivery: %+v", receipt)
			}
			if kind == "claude" && receipt.SystemPromptSHA256 != managed.ManagedBytesDigest([]byte(managed.ManagedSystemPrompt)) {
				t.Fatal("system prompt digest wrong")
			}
		})
	}
}
func TestManagedHistoryReceiptFailurePreservesUnknownOutcome(t *testing.T) {
	b, req, dir, prefix := codexAdapterFixture(t, runfixture.CodexFixtureSuccess)
	h := historyOptIn(&b, &req)
	if e := os.Mkdir(prefix+".managed.json", 0700); e != nil {
		t.Fatal(e)
	}
	response, e := managed.ManagedWorkWithHistory(context.Background(), dir, prefix, req, b, h)
	if e == nil || response.Outcome != model.OutcomeUncertain || !response.CostUnknown || response.Output != nil {
		t.Fatalf("receipt failure made unknown execution replayable: %+v %v", response, e)
	}
}
func TestManagedHistoryNoOptInAndHelperIsolation(t *testing.T) {
	s, req, b := historyFixture(t)
	h, e := managed.BuildManagedHistory(s, req, b)
	if e != nil {
		t.Fatal(e)
	}
	b.Profile.RequiredCapabilities = []string{"structured_strings", "outcome_certainty"}
	if _, e = managed.ManagedInput(req, b, h); e == nil {
		t.Fatal("unsolicited history accepted")
	}
	s, req, b = historyFixture(t)
	req.Task.Turn.Role = "helper"
	s.Tasks[req.Task.ID].Spec = req.Task
	helper, e := managed.BuildManagedHistory(s, req, b)
	if e != nil || helper != nil {
		t.Fatalf("helper inherited history: %+v %v", helper, e)
	}
	if _, e = managed.ManagedInput(req, b, h); e == nil {
		t.Fatal("helper accepted main history")
	}
}

func TestManagedHistoryExternalAuthorityBoundary(t *testing.T) {
	s, req, b := historyFixture(t)
	external := model.Task{ID: "external", Prompt: "external prompt", Requires: []string{"private"}}
	s.Tasks["external"] = &model.TaskState{Spec: external, Status: "verified", Result: map[string]string{"private": "external-input-secret"}}
	s.Tasks["first"].Spec.DependsOn = []string{"external"}
	h, e := managed.BuildManagedHistory(s, req, b)
	if e != nil {
		t.Fatal(e)
	}
	if h.Boundary != "external_input_boundary" || len(h.Entries) != 0 {
		t.Fatal("removed external input authority reappeared through history")
	}
	s.Tasks["first"].Status = "produced"
	if _, e = managed.BuildManagedHistory(s, req, b); e == nil {
		t.Fatal("privacy boundary concealed unaccepted chain")
	}
}
func TestManagedHistoryBoundedBeforeDispatch(t *testing.T) {
	b, req, dir, prefix := codexAdapterFixture(t, runfixture.CodexFixtureSuccess)
	b.Profile.Limits.InputBytes = 64
	h := historyOptIn(&b, &req)
	response, e := managed.ManagedWorkWithHistory(context.Background(), dir, prefix, req, b, h)
	if e == nil || response.Outcome != model.OutcomeNotStarted {
		t.Fatalf("oversized input dispatched: %+v %v", response, e)
	}
	if _, e = os.Stat(filepath.Join(dir, "request.json")); !os.IsNotExist(e) {
		t.Fatal("oversized history launched adapter")
	}
	if _, e = os.Stat(prefix + ".managed.json"); !os.IsNotExist(e) {
		t.Fatal("not-started request wrote dispatch receipt")
	}
}

func TestManagedHistoryTransitiveNarrowingCannotRestoreSecret(t *testing.T) {
	s, _, b := historyFixture(t)
	s.Tasks["first"].Spec.Requires = []string{"public", "secret"}
	s.Tasks["first"].Result = map[string]string{"public": "public first", "secret": "transitive-secret-canary"}
	second := s.Tasks["second"]
	second.Status = "verified"
	second.Spec.Requires = []string{"public"}
	second.Result = map[string]string{"public": "public second"}
	third := model.Task{ID: "third", Profile: "small", Prompt: "third public only", Requires: []string{"answer"}, DependsOn: []string{"second"}, InputFields: map[string][]string{"second": {"public"}}, Turn: &model.TurnIdentity{AgentID: "agent", Role: "main", Index: 3, PreviousTask: "second"}}
	s.Tasks["third"] = &model.TaskState{Spec: third, Status: "running", Generation: 1, Attempt: 1, OperationID: model.OperationID(s.RunID, "third", 1), BindingDigest: b.Digest}
	inputs, e := managed.InputsForTask(s, third)
	if e != nil {
		t.Fatal(e)
	}
	req := request(s, third, 1, inputs, model.Options{Timeout: time.Second})
	h, e := managed.BuildManagedHistory(s, req, b)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(h)
	if strings.Contains(string(raw), "transitive-secret-canary") {
		t.Fatalf("narrowed third turn regained first-turn secret: %s", raw)
	}
}

func TestManagedHistoryReceiptFailureRetainsLedgerHold(t *testing.T) {
	old := runfixture.CompletionCodexFixture(t, "0.25", false)
	p := runfixture.Read(t, old).Plan
	p.APIVersion = model.GovernedPlanVersion
	for name, profile := range p.Execution.Catalog.Profiles {
		profile.RequiredCapabilities = append(profile.RequiredCapabilities, "explicit_history")
		p.Execution.Catalog.Profiles[name] = profile
	}
	root := filepath.Join(t.TempDir(), "history-run")
	if e := checkpoint.Initialize(p, root, "managed"); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.SetControl(root, "run"); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(root, "cells", "a", "1.managed.json"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := worker(root, "a", model.Options{Timeout: 10 * time.Second}); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, root)
	row := s.Tasks["a"]
	used, e := checkpoint.ManagedBudgetUsed(s.Execution)
	if e != nil || row.Status != "uncertain" || !row.CostUnknown || used != 250000 || s.Execution.Reservations[row.OperationID].Status != "uncertain" {
		t.Fatalf("receipt persistence failure released unknown hold: row=%+v used=%d err=%v", row, used, e)
	}
	if e = worker(root, "b", model.Options{Timeout: 10 * time.Second}); e != nil {
		t.Fatal(e)
	}
	if runfixture.Read(t, root).Tasks["b"].Attempt != 0 {
		t.Fatal("receipt failure admitted peer using retained allowance")
	}
}

func TestManagedHistoryRemovedPriorInputCannotLeakThroughPrompt(t *testing.T) {
	s, _, b := historyFixture(t)
	s.Tasks["first"].Spec.Requires = []string{"public", "secret"}
	s.Tasks["first"].Result = map[string]string{"public": "public first", "secret": "prior-secret-canary"}
	second := s.Tasks["second"]
	second.Status = "verified"
	second.Spec.Prompt = "Review prior-secret-canary privately"
	second.Spec.Requires = []string{"public"}
	second.Result = map[string]string{"public": "public second"}
	third := model.Task{ID: "third", Profile: "small", Prompt: "third public only", Requires: []string{"answer"}, DependsOn: []string{"second"}, InputFields: map[string][]string{"second": {"public"}}, Turn: &model.TurnIdentity{AgentID: "agent", Role: "main", Index: 3, PreviousTask: "second"}}
	s.Tasks["third"] = &model.TaskState{Spec: third, Status: "running", Generation: 1, Attempt: 1, OperationID: model.OperationID(s.RunID, "third", 1), BindingDigest: b.Digest}
	inputs, e := managed.InputsForTask(s, third)
	if e != nil {
		t.Fatal(e)
	}
	req := request(s, third, 1, inputs, model.Options{Timeout: time.Second})
	h, e := managed.BuildManagedHistory(s, req, b)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(h)
	if h.Boundary != "external_input_boundary" || len(h.Entries) != 0 || strings.Contains(string(raw), "prior-secret-canary") {
		t.Fatalf("historical prompt leaked removed prior-input authority: %s", raw)
	}
}
