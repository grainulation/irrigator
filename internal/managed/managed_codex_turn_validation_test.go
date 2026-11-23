package managed

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func codexTurnControlStream(t *testing.T, proposal string) []byte {
	t.Helper()
	message, err := json.Marshal(map[string]string{"answer": "ok", "turn_request": proposal})
	if err != nil {
		t.Fatal(err)
	}
	item, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": "message", "type": "agent_message", "text": string(message)}})
	if err != nil {
		t.Fatal(err)
	}
	return []byte("{\"type\":\"thread.started\",\"thread_id\":\"fixture\"}\n{\"type\":\"turn.started\"}\n" + string(item) + "\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":10}}\n")
}

func TestManagedCodexRejectsMalformedTurnControls(t *testing.T) {
	task := codexTurnRequest().Task
	for _, tc := range []struct {
		name, proposal, want string
	}{
		{"null-continuation", `{"continue":null}`, "continue must be an object"},
		{"string-continuation", `{"continue":"next"}`, "continue must be an object"},
		{"array-continuation", `{"continue":[]}`, "continue must be an object"},
		{"null-helpers", `{"helpers":null}`, "helpers must be an array"},
		{"object-helpers", `{"helpers":{}}`, "helpers must be an array"},
		{"null-helper", `{"helpers":[null]}`, "helpers[0] must be an object"},
		{"string-helper", `{"helpers":["audit"]}`, "helpers[0] must be an object"},
		{"null-wait", `{"continue":{"prompt":"next","requires":["answer"]},"helpers":[{"id":"h","prompt":"help","requires":["answer"]}],"wait_for_helpers":null}`, "wait_for_helpers must be a boolean"},
		{"string-wait", `{"wait_for_helpers":"true"}`, "wait_for_helpers must be a boolean"},
		{"number-wait", `{"wait_for_helpers":0}`, "wait_for_helpers must be a boolean"},
		{"invalid-json", `{`, "Codex turn_request"},
		{"null-proposal", `null`, "Codex turn_request"},
		{"array-proposal", `[]`, "Codex turn_request"},
		{"duplicate-controls", `{"wait_for_helpers":true,"wait_for_helpers":null}`, "Codex turn_request"},
		{"trailing-json", `{} {}`, "Codex turn_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, _, err := ParseManagedCodexStream(codexTurnControlStream(t, tc.proposal), task)
			if err == nil || !strings.Contains(err.Error(), tc.want) || output != nil {
				t.Fatalf("invalid control accepted or wrong diagnostic: output=%v err=%v", output, err)
			}
		})
	}
}

func TestManagedCodexValidTurnControlsPreserveWorkerStrings(t *testing.T) {
	task := codexTurnRequest().Task
	for _, proposal := range []string{
		"", "  ", `{}`, `{"helpers":[]}`, `{"wait_for_helpers":false}`,
		`{"continue":{"prompt":"next","requires":["answer"]}}`,
		`{"helpers":[{"id":"h","prompt":"help","requires":["answer"]}]}`,
		`{"continue":{"prompt":"next","requires":["answer"]},"helpers":[{"id":"h","prompt":"help","requires":["answer"]}],"wait_for_helpers":true}`,
	} {
		t.Run(proposal, func(t *testing.T) {
			output, usage, err := ParseManagedCodexStream(codexTurnControlStream(t, proposal), task)
			if err != nil || output["turn_request"] != proposal || usage == nil {
				t.Fatalf("valid control or legacy stop changed: output=%v err=%v", output, err)
			}
			if err := artifactstore.Accept(task, output); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A field called turn_request remains ordinary user data outside a turn.
	task.Turn = nil
	output, _, err := ParseManagedCodexStream(codexTurnControlStream(t, `{"continue":null}`), task)
	if err != nil || output["turn_request"] != `{"continue":null}` {
		t.Fatalf("ordinary output interpretation changed: %v %v", output, err)
	}
}

func TestManagedCodexNullControlsRetainUncertainCost(t *testing.T) {
	for _, proposal := range []string{
		`{"continue":null}`,
		`{"helpers":null}`,
		`{"helpers":[null]}`,
		`{"continue":{"prompt":"next","requires":["answer"]},"helpers":[{"id":"h","prompt":"help","requires":["answer"]}],"wait_for_helpers":null}`,
	} {
		t.Run(proposal, func(t *testing.T) {
			b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, "")
			req.Task.Requires = []string{"answer", "turn_request"}
			req.Task.Turn = &model.TurnIdentity{AgentID: req.Task.ID, Role: "main", Index: 1}
			req.Boundaries = codexTurnRequest().Boundaries
			stream := codexTurnControlStream(t, proposal)
			script := "#!/bin/sh\ncat >/dev/null\ncat <<'FIXTURE_EVENTS'\n" + string(stream) + "FIXTURE_EVENTS\n"
			if err := os.WriteFile(b.Adapter.Command[0], []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			b.Adapter.Kind = "codex_cli"
			var err error
			b.Adapter.ExecutableSHA256, err = launch.ExecutableDigest(b.Adapter.Command[0])
			if err != nil {
				t.Fatal(err)
			}
			b.Instance.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "host_session", Name: "codex"}}
			b.SupportedCapabilities = launch.ExecutionCapabilities("codex_cli")
			b.Digest = ""
			b.Digest = launch.ExecutionDigest(b)
			response, err := managedWork(context.Background(), dir, prefix, req, b)
			if !model.IsUncertain(err) || response.Outcome != model.OutcomeUncertain || !response.CostUnknown || response.Output != nil {
				t.Fatalf("malformed result settled or lost uncertainty: response=%+v err=%v", response, err)
			}
			var receipt ManagedDispatchReceipt
			raw, err := os.ReadFile(prefix + ".managed.json")
			if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Outcome != model.OutcomeUncertain || !receipt.CostUnknown || receipt.ReportedCostUSD != nil {
				t.Fatalf("uncertain accounting receipt lost: %s err=%v", raw, err)
			}
			raw, err = os.ReadFile(prefix + ".stdout.json")
			if err != nil || string(raw) != string(stream) {
				t.Fatalf("raw provider evidence lost: %s err=%v", raw, err)
			}
		})
	}
}
