package managed

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

// The claude CLI reports `modelUsage` keyed by model name with camelCase
// fields (shape taken from a real `claude -p --output-format json` result).
func TestApplyClaudeModelUsage(t *testing.T) {
	haiku := `{"inputTokens":2,"outputTokens":704,"cacheReadInputTokens":513,"cacheCreationInputTokens":783,"webSearchRequests":0,"costUSD":0.00051393,"contextWindow":1000000,"maxOutputTokens":128000,"thinkingTokens":0,"canonicalModel":"claude-haiku-5-5","provider":"firstParty","costBasis":"list"}`
	opus := `{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":30,"cacheCreationInputTokens":40,"thinkingTokens":5,"costUSD":0.01}`
	preset := &model.TaskUsage{InputTokens: 1, TotalTokens: 1, Model: "adapter"}
	cases := []struct {
		name   string
		usage  string
		preset *model.TaskUsage
		want   *model.TaskUsage
	}{
		{"single model", `{"claude-haiku-5-5":` + haiku + `}`, nil,
			&model.TaskUsage{InputTokens: 2, OutputTokens: 704, CacheReadTokens: 513, CacheWriteTokens: 783, TotalTokens: 2002, Model: "m"}},
		{"two models summed", `{"claude-haiku-5-5":` + haiku + `,"claude-opus-5-5":` + opus + `}`, nil,
			&model.TaskUsage{InputTokens: 12, OutputTokens: 724, ThinkingTokens: 5, CacheReadTokens: 543, CacheWriteTokens: 823, TotalTokens: 2102, Model: "m"}},
		{"malformed entry", `{"claude-haiku-5-5":"not an object"}`, nil, nil},
		{"snake_case top level keys are not per-model entries", `{"input_tokens":5,"output_tokens":6}`, nil, nil},
		{"empty", `{}`, nil, nil},
		{"overflow across models rejects all usage", `{"a":{"inputTokens":9223372036854775807},"b":{"inputTokens":2}}`, nil, nil},
		{"overflow in total rejects all usage", `{"a":{"inputTokens":9223372036854775807,"outputTokens":1}}`, nil, nil},
		{"overflow in thinking rejects all usage", `{"a":{"inputTokens":1,"thinkingTokens":9223372036854775807},"b":{"thinkingTokens":1}}`, nil, nil},
		{"negative count rejects all usage", `{"a":{"inputTokens":-5,"outputTokens":10},"b":` + opus + `}`, nil, nil},
		{"zero tokens", `{"m":{"inputTokens":0,"outputTokens":0,"thinkingTokens":3}}`, nil, nil},
		{"adapter usage kept", `{"claude-haiku-5-5":` + haiku + `}`, preset, preset},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var usage map[string]json.RawMessage
			if err := json.Unmarshal([]byte(c.usage), &usage); err != nil {
				t.Fatal(err)
			}
			response := model.WorkResponse{Usage: c.preset}
			applyClaudeModelUsage(&response, usage, "m")
			if !reflect.DeepEqual(response.Usage, c.want) {
				t.Fatalf("usage = %+v, want %+v", response.Usage, c.want)
			}
			if c.preset != nil && response.Usage != c.preset {
				t.Fatal("adapter-reported usage was replaced")
			}
		})
	}
}
