package artifactstore

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/grainulation/irrigator/internal/model"
)

func TestTurnSchemaProposalRequirements(t *testing.T) {
	req := model.WorkRequest{Task: model.Task{Requires: []string{TurnRequestField}, Turn: &model.TurnIdentity{Role: "main"}}}
	raw, err := json.Marshal(ModelSchemaForRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, proposal string
		valid          bool
	}{
		{"continuation without ID", `{"continue":{"prompt":"next","requires":["answer"]}}`, true},
		{"continuation with ID", `{"continue":{"id":"old","prompt":"next","requires":["answer"]}}`, true},
		{"helper with ID", `{"helpers":[{"id":"review","prompt":"review","requires":["answer"]}]}`, true},
		{"helper without ID", `{"helpers":[{"prompt":"review","requires":["answer"]}]}`, false},
		{"continuation without prompt", `{"continue":{"requires":["answer"]}}`, false},
		{"continuation without requires", `{"continue":{"prompt":"next"}}`, false},
		{"wait branch continuation without ID", `{"wait_for_helpers":true,"continue":{"prompt":"next","requires":["answer"]}}`, true},
		{"wait branch helper without ID", `{"wait_for_helpers":true,"continue":{"prompt":"next","requires":["answer"]},"helpers":[{"prompt":"review","requires":["answer"]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var proposal any
			if err := json.Unmarshal([]byte(tc.proposal), &proposal); err != nil {
				t.Fatal(err)
			}
			err := resolved.Validate(map[string]any{TurnRequestField: proposal})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, validation error=%v", tc.valid, err)
			}
		})
	}
}
