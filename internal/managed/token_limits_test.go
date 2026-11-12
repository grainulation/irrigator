package managed

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func tokenLimitBinding(b model.ExecutionBinding, limit int64) model.ExecutionBinding {
	b.Profile.Limits.MaxOutputTokens = limit
	b.SupportedCapabilities = launch.ExecutionProfileCapabilities(b.Adapter.Kind, b.Profile)
	b.Digest = ""
	b.Digest = launch.ExecutionDigest(b)
	return b
}

func TestOllamaExplicitOutputTokenLimit(t *testing.T) {
	b, req, dir, prefix, calls := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		options, _ := body["options"].(map[string]any)
		if options["num_predict"] != float64(8) {
			t.Errorf("generation limit = %v", options["num_predict"])
		}
		w.Write([]byte(ollamaSuccess))
	})
	b = tokenLimitBinding(b, 8)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if err != nil || response.Outcome != model.OutcomeSettled || response.Usage == nil || response.Usage.OutputTokens != 2 || calls.Load() != 1 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestOllamaOutputTokenLimitViolationRetainsEvidence(t *testing.T) {
	b, req, dir, prefix, calls := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Replace(ollamaSuccess, `"eval_count":2`, `"eval_count":9`, 1)))
	})
	b = tokenLimitBinding(b, 8)
	response, err := managedWork(context.Background(), dir, prefix, req, b)
	if !model.IsUncertain(err) || response.Outcome != model.OutcomeUncertain || !response.CostUnknown || response.Output != nil || calls.Load() != 1 {
		t.Fatalf("violating completion accepted: %+v %v", response, err)
	}
	if response.Usage == nil || response.Usage.OutputTokens != 9 {
		t.Fatal("violating usage evidence lost")
	}
}

func TestOllamaOutputTokenLimitValidationBeforeDispatch(t *testing.T) {
	for _, limit := range []int64{-1, 513} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			b, req, dir, prefix, calls := ollamaFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not generate") })
			b = tokenLimitBinding(b, limit)
			response, err := managedWork(context.Background(), dir, prefix, req, b)
			if err == nil || response.Outcome != model.OutcomeNotStarted || calls.Load() != 0 {
				t.Fatalf("unsupported cap dispatched: %+v %v", response, err)
			}
		})
	}
}

func TestOutputTokenLimitRejectsUnsupportedAdapters(t *testing.T) {
	for _, kind := range []string{"mock", "claude_cli"} {
		t.Run(kind, func(t *testing.T) {
			_, catalog, _ := runfixture.ExecutionFixture(t, kind)
			profile := catalog.Profiles["small"]
			profile.Limits.MaxOutputTokens = 8
			catalog.Profiles["small"] = profile
			if err := launch.ValidateExecutionCatalog(catalog); err == nil {
				t.Fatal("unsupported adapter accepted output token cap")
			}
		})
	}
}

func TestOutputTokenLimitLegacyEncodingAndCapabilities(t *testing.T) {
	raw, err := json.Marshal(model.ExecutionLimits{InputBytes: 1024, OutputBytes: 2048})
	if err != nil || string(raw) != `{"input_bytes":1024,"output_bytes":2048}` {
		t.Fatalf("legacy limits changed: %s %v", raw, err)
	}
	for _, kind := range []string{"mock", "claude_cli", "ollama_http"} {
		caps := launch.ExecutionProfileCapabilities(kind, model.ExecutionProfile{})
		for _, cap := range caps {
			if cap == "hard_output_token_limit" {
				t.Fatal("legacy profile acquired a new capability")
			}
		}
	}
	caps := launch.ExecutionProfileCapabilities("ollama_http", model.ExecutionProfile{Limits: model.ExecutionLimits{MaxOutputTokens: 8}})
	found := false
	for _, cap := range caps {
		found = found || cap == "hard_output_token_limit"
	}
	if !found {
		t.Fatal("configured Ollama cap not advertised")
	}
}
