package executor

import (
	"context"
	"encoding/json"
	"fmt"
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

func codexAdapterFixture(t *testing.T, stream string) (model.ExecutionBinding, model.WorkRequest, string, string) {
	t.Helper()
	b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, "")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nprintf '%%s' \"$PWD\" > %q\nprintf '%%s' \"${OPENAI_API_KEY-unset}\" > %q\ncat > %q\ncat <<'FIXTURE_EVENTS'\n%sFIXTURE_EVENTS\n", filepath.Join(dir, "argv"), filepath.Join(dir, "cwd"), filepath.Join(dir, "key-state"), filepath.Join(dir, "request.json"), stream)
	if err := os.WriteFile(b.Adapter.Command[0], []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	b.Adapter.Kind = "codex_cli"
	b.Adapter.ExecutableSHA256, _ = launch.ExecutableDigest(b.Adapter.Command[0])
	b.Instance.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "host_session", Name: "codex"}}
	b.Profile.Model = "gpt-6-luna"
	b.SupportedCapabilities = launch.ExecutionCapabilities("codex_cli")
	b.Digest = ""
	b.Digest = launch.ExecutionDigest(b)
	return b, req, dir, prefix
}
func TestManagedCodexParserRejectsAmbiguity(t *testing.T) {
	task := model.Task{ID: "node", Prompt: "answer", Requires: []string{"answer"}}
	for name, stream := range map[string]string{
		"missing_usage":      strings.Replace(runfixture.CodexFixtureSuccess, `"usage":{"input_tokens":100,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0}`, `"usage":null`, 1),
		"negative_usage":     strings.Replace(runfixture.CodexFixtureSuccess, `"output_tokens":10`, `"output_tokens":-1`, 1),
		"alias_usage":        strings.Replace(runfixture.CodexFixtureSuccess, `"output_tokens":10`, `"output_tokens":10,"OUTPUT_TOKENS":0`, 1),
		"duplicate":          strings.Replace(runfixture.CodexFixtureSuccess, `"type":"turn.completed"`, `"type":"turn.failed","type":"turn.completed"`, 1),
		"missing_terminal":   strings.Split(runfixture.CodexFixtureSuccess, `{"type":"turn.completed"`)[0],
		"duplicate_terminal": runfixture.CodexFixtureSuccess + `{"type":"turn.completed","usage":{"input_tokens":0,"output_tokens":0}}`,
		"tool":               strings.Replace(runfixture.CodexFixtureSuccess, `"type":"agent_message"`, `"type":"command_execution"`, 1),
		"failed":             strings.Replace(runfixture.CodexFixtureSuccess, `"type":"turn.completed"`, `"type":"turn.failed"`, 1),
		"alias_type":         strings.Replace(runfixture.CodexFixtureSuccess, `"type":"turn.completed"`, `"Type":"turn.completed"`, 1),
		"duplicate_final":    strings.Replace(runfixture.CodexFixtureSuccess, `{"type":"turn.completed"`, `{"type":"item.completed","item":{"type":"agent_message","text":"{\"answer\":\"extra\"}"}}`+"\n"+`{"type":"turn.completed"`, 1),
		"unknown_diagnostic": strings.Replace(runfixture.CodexFixtureSuccess, `{"type":"turn.started"}`, `{"type":"item.completed","item":{"type":"error","message":"unknown failure"}}`+"\n"+`{"type":"turn.started"}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := managed.ParseManagedCodexStream([]byte(stream), task); err == nil {
				t.Fatal("accepted ambiguous Codex stream")
			}
		})
	}
	valid := strings.Replace(runfixture.CodexFixtureSuccess, `{"type":"turn.started"}`, `{"type":"item.completed","item":{"type":"error","message":"Code Mode is unavailable because code-mode host is disabled. Code mode will fail closed; enable `+"`features.code_mode_host`"+` and install `+"`codex-code-mode-host`"+`."}}`+"\n"+`{"type":"turn.started"}`, 1)
	output, usage, err := managed.ParseManagedCodexStream([]byte(valid), task)
	if err != nil || output["answer"] != "ok" || usage.OutputTokens != 10 {
		t.Fatalf("valid fixture rejected: %v", err)
	}
}
func TestManagedCodexIsolationAndUnknownCost(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	b, req, dir, prefix := codexAdapterFixture(t, runfixture.CodexFixtureSuccess)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err != nil || response.Output["answer"] != "ok" || !response.CostUnknown || response.Usage == nil || response.Usage.OutputTokens != 10 {
		t.Fatalf("result %+v %v", response, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "request.json"))
	var got model.WorkRequest
	if json.Unmarshal(raw, &got) != nil || got.RunID != req.RunID || got.Task.ID != req.Task.ID || got.OperationID != req.OperationID || got.Attempt != req.Attempt || launch.ValidateWorkerLaunchContext(got) != nil {
		t.Fatal("identity lost")
	}
	args, _ := os.ReadFile(filepath.Join(dir, "argv"))
	for _, must := range []string{"--ignore-user-config", "--ignore-rules", "--ephemeral", "--sandbox\nread-only", "features.shell_tool=false", "features.plugins=false", "features.hooks=false", "features.multi_agent=false", "web_search=\"disabled\"", "features.unbounded_connection_retries=false"} {
		if !strings.Contains(string(args), must) {
			t.Fatalf("missing isolation %q", must)
		}
	}
	if strings.Contains(string(args), "model_providers.irrigator_fixture") || strings.Contains(string(args), "base_url") {
		t.Fatal("fixture provider leaked to product command")
	}
	cwd, _ := os.ReadFile(filepath.Join(dir, "cwd"))
	if string(cwd) == dir || !strings.HasPrefix(string(cwd), "/private/tmp/irrigator-codex-") && !strings.HasPrefix(string(cwd), "/tmp/irrigator-codex-") {
		t.Fatalf("not isolated cwd %s", cwd)
	}
	if _, err := os.Stat(string(cwd)); !os.IsNotExist(err) {
		t.Fatal("private cwd not cleaned")
	}
	key, _ := os.ReadFile(filepath.Join(dir, "key-state"))
	if string(key) != "unset" {
		t.Fatal("inherited API key")
	}
	var receipt managed.ManagedDispatchReceipt
	raw, _ = os.ReadFile(prefix + ".managed.json")
	if json.Unmarshal(raw, &receipt) != nil || !receipt.CostUnknown || receipt.ReportedCostUSD != nil || receipt.ReportedModel != nil || receipt.TokenUsage == nil {
		t.Fatalf("false accounting receipt %s", raw)
	}
}
func TestManagedCodexCancellationAndFailure(t *testing.T) {
	for _, kind := range []string{"cancel", "failed", "missing_usage", "tool"} {
		t.Run(kind, func(t *testing.T) {
			stream := runfixture.CodexFixtureSuccess
			if kind == "failed" {
				stream = "{\"type\":\"turn.failed\"}\n"
			}
			if kind == "missing_usage" {
				stream = strings.Replace(stream, `"output_tokens":10`, `"wrong":10`, 1)
			}
			if kind == "tool" {
				stream = strings.Replace(stream, `"agent_message"`, `"mcp_tool_call"`, 1)
			}
			b, req, dir, prefix := codexAdapterFixture(t, stream)
			ctx := context.Background()
			if kind == "cancel" {
				if err := os.WriteFile(b.Adapter.Command[0], []byte("#!/bin/sh\nsleep 5\n"), 0700); err != nil {
					t.Fatal(err)
				}
				b.Adapter.ExecutableSHA256, _ = launch.ExecutableDigest(b.Adapter.Command[0])
				b.Digest = ""
				b.Digest = launch.ExecutionDigest(b)
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			response, err := managedWork(ctx, dir, prefix, req, b)
			if !model.IsUncertain(err) || !response.CostUnknown {
				t.Fatalf("lost unknown hold %+v %v", response, err)
			}
		})
	}
}

func TestManagedCodexPreflightRefusesBeforeInvocation(t *testing.T) {
	for _, kind := range []string{"replacement", "input", "credential", "effort"} {
		t.Run(kind, func(t *testing.T) {
			b, req, dir, prefix := codexAdapterFixture(t, runfixture.CodexFixtureSuccess)
			switch kind {
			case "replacement":
				if err := os.WriteFile(b.Adapter.Command[0], []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			case "input":
				b.Profile.Limits.InputBytes = 1
			case "credential":
				b.Instance.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "env", Name: "OPENAI_API_KEY"}}
			case "effort":
				b.Profile.Effort = "high"
			}
			b.Digest = ""
			b.Digest = launch.ExecutionDigest(b)
			response, err := managedWork(context.Background(), dir, prefix, req, b)
			if !model.KnownNotStarted(err) || response.CostUnknown {
				t.Fatalf("not positive preflight failure: %+v %v", response, err)
			}
			if _, err = os.Stat(filepath.Join(dir, "request.json")); !os.IsNotExist(err) {
				t.Fatal("preflight invoked CLI")
			}
		})
	}
}

func TestManagedCodexWorkerRetainsUnknownCostHold(t *testing.T) {
	b, _, _, _ := codexAdapterFixture(t, runfixture.CodexFixtureSuccess)
	p := model.Plan{APIVersion: launch.UnknownCostPlanVersion, Name: "Codex fixture", TurnShape: &model.TurnShape{Roots: 1, MainTurns: 1, HelpersPerTurn: 0}, MaxTasks: 1, Execution: &model.ManagedExecutionPlan{CostPolicy: "retain_unknown", RunBudgetUSD: "0.25", AllowedProfiles: []string{b.ProfileName}, Catalog: model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{b.AdapterName: b.Adapter}, Instances: map[string]model.ExecutionInstance{b.InstanceName: b.Instance}, Profiles: map[string]model.ExecutionProfile{b.ProfileName: b.Profile}, DefaultProfile: b.ProfileName}}, Tasks: []model.Task{{ID: "node", Prompt: "return answer", Requires: []string{"answer"}, Turn: &model.TurnIdentity{AgentID: "node", Role: "main", Index: 1}}}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	if err := worker(root, "node", model.Options{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	row := s.Tasks["node"]
	reservation := s.Execution.Reservations[row.OperationID]
	if s.Version != 3 || row.Status != "verified" || !row.CostUnknown || reservation.Status != "unknown_cost" || reservation.ReservedMicros != 250000 {
		t.Fatalf("unknown accounting lost: row%+v reservation%+v", row, reservation)
	}
}
