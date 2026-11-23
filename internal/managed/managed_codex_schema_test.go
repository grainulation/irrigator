package managed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// This checks the strict object rules involved in the provider's HTTP 400. It
// deliberately walks schema positions only; JSON quoted inside descriptions is
// guidance for the runtime string contract, not part of the provider schema.
func codexStrictObjects(schema map[string]any, at string) error {
	if schema["type"] == "object" {
		if schema["additionalProperties"] != false {
			return fmt.Errorf("%s: additionalProperties must be false", at)
		}
		required, ok := schema["required"].([]any)
		if !ok {
			return fmt.Errorf("%s: required array missing", at)
		}
		properties, _ := schema["properties"].(map[string]any)
		for key := range properties {
			found := false
			for _, r := range required {
				found = found || r == key
			}
			if !found {
				return fmt.Errorf("%s: required missing %s", at, key)
			}
		}
	}
	for _, keyword := range []string{"properties", "$defs"} {
		named, _ := schema[keyword].(map[string]any)
		keys := make([]string, 0, len(named))
		for key := range named {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if child, ok := named[key].(map[string]any); ok {
				if err := codexStrictObjects(child, at+"."+keyword+"."+key); err != nil {
					return err
				}
			}
		}
	}
	if child, ok := schema["items"].(map[string]any); ok {
		if err := codexStrictObjects(child, at+".items"); err != nil {
			return err
		}
	}
	for _, keyword := range []string{"anyOf", "allOf", "oneOf"} {
		children, _ := schema[keyword].([]any)
		for i, child := range children {
			if m, ok := child.(map[string]any); ok {
				if err := codexStrictObjects(m, fmt.Sprintf("%s.%s[%d]", at, keyword, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func codexSchemaJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func codexTurnRequest() model.WorkRequest {
	return model.WorkRequest{
		Task:       model.Task{ID: "lane", Requires: []string{"answer", "turn_request"}, Turn: &model.TurnIdentity{AgentID: "lane", Role: "main", Index: 1}},
		Boundaries: &model.WorkerBoundaries{Topology: "turns", RemainingTurns: 2, HelpersRemaining: 2, TaskSlotsRemaining: 3},
	}
}

func TestManagedCodexTurnSchemaAvoidsStrictObjectRejection(t *testing.T) {
	req := codexTurnRequest()
	legacy, err := json.Marshal(artifactstore.ModelSchemaForRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	if err := codexStrictObjects(codexSchemaJSON(t, legacy), "$"); err == nil || !strings.Contains(err.Error(), "turn_request.anyOf[0]") {
		t.Fatalf("fixture no longer reproduces the incident's rejected turn_request branch: %v", err)
	}
	raw, err := managedCodexSchema(req)
	if err != nil {
		t.Fatal(err)
	}
	schema := codexSchemaJSON(t, raw)
	if err := codexStrictObjects(schema, "$"); err != nil {
		t.Fatal(err)
	}
	if schema["$defs"] != nil {
		t.Fatal("unused optional Task definitions still sent to the provider")
	}
	props := schema["properties"].(map[string]any)
	if len(props) != 2 || props["answer"].(map[string]any)["type"] != "string" || props["turn_request"].(map[string]any)["type"] != "string" {
		t.Fatalf("worker output contract changed: %s", raw)
	}
}

func TestManagedCodexSchemaLeavesOrdinaryOutputsUnchanged(t *testing.T) {
	for _, turn := range []*model.TurnIdentity{nil, {AgentID: "lane", Role: "main", Index: 1}} {
		req := model.WorkRequest{Task: model.Task{Requires: []string{"answer", "files"}, FileFields: []string{"files"}, Turn: turn}}
		want, err := json.Marshal(artifactstore.ModelSchemaForRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		got, err := managedCodexSchema(req)
		if err != nil || string(got) != string(want) {
			t.Fatalf("ordinary schema changed: %s / %s, %v", got, want, err)
		}
		if err := codexStrictObjects(codexSchemaJSON(t, got), "$"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManagedCodexTurnSchemaRejectsLegacySpawn(t *testing.T) {
	req := codexTurnRequest()
	req.Task.Spawn = &model.SpawnPolicy{Field: "answer", MaxChildren: 1}
	if _, err := managedCodexSchema(req); err == nil {
		t.Fatal("turn plus legacy spawning must be refused before removing shared definitions")
	}
}

func codexTurnAdvice(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	schema := codexSchemaJSON(t, raw)
	description, _ := schema["properties"].(map[string]any)["turn_request"].(map[string]any)["description"].(string)
	for i, c := range description {
		if c != '{' {
			continue
		}
		var advice map[string]any
		if json.Unmarshal([]byte(description[i:]), &advice) == nil && (advice["type"] == "object" || advice["anyOf"] != nil) {
			return advice
		}
	}
	t.Fatalf("missing parseable bounded turn schema in guidance: %s", description)
	return nil
}

func TestManagedCodexTurnSchemaPreservesBoundaryGuidance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		role      string
		turns     int
		helpers   int
		slots     int
		continued bool
	}{
		{"main", "main", 2, 2, 3, true},
		{"one-slot", "main", 2, 2, 1, true},
		{"final", "main", 0, 2, 3, false},
		{"helper", "helper", 0, 0, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := codexTurnRequest()
			req.Task.Turn.Role = tc.role
			req.Boundaries.RemainingTurns = tc.turns
			req.Boundaries.HelpersRemaining = tc.helpers
			req.Boundaries.TaskSlotsRemaining = tc.slots
			raw, err := managedCodexSchema(req)
			if err != nil {
				t.Fatal(err)
			}
			advice := codexTurnAdvice(t, raw)
			variants := []any{advice}
			if branches, ok := advice["anyOf"].([]any); ok {
				variants = branches
			}
			for _, variant := range variants {
				props := variant.(map[string]any)["properties"].(map[string]any)
				_, continued := props["continue"]
				if continued != tc.continued {
					t.Fatalf("continuation guidance disagrees with role/turn bound: %s", raw)
				}
				wantHelpers := tc.helpers
				wait := props["wait_for_helpers"].(map[string]any)["enum"].([]any)
				if len(wait) == 1 && wait[0] == true {
					wantHelpers = min(wantHelpers, max(0, tc.slots-1))
				}
				if got := props["helpers"].(map[string]any)["maxItems"]; got != float64(wantHelpers) {
					t.Fatalf("helper allowance = %v, want %d", got, wantHelpers)
				}
			}
		})
	}
}

func TestManagedCodexAdapterUsesTurnStringSchema(t *testing.T) {
	for _, proposal := range []string{
		`{}`,
		`{"continue":{"prompt":"finish","requires":["answer","turn_request"]}}`,
		`{"helpers":[{"id":"audit","prompt":"audit","requires":["answer"]}]}`,
		`{"continue":{"prompt":"finish","requires":["answer","turn_request"]},"helpers":[{"id":"audit","prompt":"audit","requires":["answer"]}],"wait_for_helpers":true}`,
	} {
		t.Run(proposal, func(t *testing.T) {
			b, req, dir, prefix := runfixture.ManagedAdapterFixture(t, "")
			req.Task.Requires = []string{"answer", "turn_request"}
			req.Task.Turn = &model.TurnIdentity{AgentID: req.Task.ID, Role: "main", Index: 1}
			req.Boundaries = codexTurnRequest().Boundaries
			message, err := json.Marshal(map[string]string{"answer": "ok", "turn_request": proposal})
			if err != nil {
				t.Fatal(err)
			}
			item, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": "message", "type": "agent_message", "text": string(message)}})
			if err != nil {
				t.Fatal(err)
			}
			stream := "{\"type\":\"thread.started\",\"thread_id\":\"fixture\"}\n{\"type\":\"turn.started\"}\n" + string(item) + "\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":10}}\n"
			capture := filepath.Join(dir, "captured-schema.json")
			script := fmt.Sprintf("#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = --output-schema ]; then\n    shift\n    cat \"$1\" > %q\n  fi\n  shift\ndone\ncat > /dev/null\ncat <<'FIXTURE_EVENTS'\n%sFIXTURE_EVENTS\n", capture, stream)
			if err := os.WriteFile(b.Adapter.Command[0], []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			b.Adapter.Kind = "codex_cli"
			b.Adapter.ExecutableSHA256, err = launch.ExecutableDigest(b.Adapter.Command[0])
			if err != nil {
				t.Fatal(err)
			}
			b.Instance.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "host_session", Name: "codex"}}
			b.SupportedCapabilities = launch.ExecutionCapabilities("codex_cli")
			b.Digest = ""
			b.Digest = launch.ExecutionDigest(b)
			response, err := managedWork(context.Background(), dir, prefix, req, b)
			if err != nil || response.Output["turn_request"] != proposal || response.Outcome != model.OutcomeSettled || !response.CostUnknown {
				t.Fatalf("turn string not accepted with preserved accounting: %+v / %v", response, err)
			}
			if err := artifactstore.Accept(req.Task, response.Output); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if err := codexStrictObjects(codexSchemaJSON(t, got), "$"); err != nil {
				t.Fatalf("adapter dispatched rejected object schema: %v", err)
			}
			want, err := managedCodexSchema(req)
			if err != nil || !reflect.DeepEqual(codexSchemaJSON(t, got), codexSchemaJSON(t, want)) {
				t.Fatalf("adapter did not dispatch its Codex-specific schema: %s / %s, %v", got, want, err)
			}
		})
	}
}
