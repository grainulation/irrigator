package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUsageParsing(t *testing.T) {
	// Valid usage with all fields
	raw := map[string]any{
		"input_tokens":       float64(100),
		"output_tokens":      float64(50),
		"thinking_tokens":    float64(25),
		"cache_read_tokens":  float64(10),
		"cache_write_tokens": float64(5),
		"total_tokens":       float64(190),
		"model":              "claude-3-7-sonnet",
		"ignored_extra_key":  "something",
	}
	u, err := ParseStrictUsage(raw)
	if err != nil {
		t.Fatalf("unexpected error parsing valid usage: %v", err)
	}
	if u.InputTokens != 100 || u.OutputTokens != 50 || u.ThinkingTokens != 25 ||
		u.CacheReadTokens != 10 || u.CacheWriteTokens != 5 || u.TotalTokens != 190 || u.Model != "claude-3-7-sonnet" {
		t.Fatalf("parsed usage mismatch: %+v", u)
	}

	// Derive total_tokens when omitted
	rawNoTotal := map[string]any{
		"input_tokens":  float64(10),
		"output_tokens": float64(20),
	}
	uNoTotal, err := ParseStrictUsage(rawNoTotal)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uNoTotal.TotalTokens != 30 {
		t.Fatalf("expected total_tokens 30, got %d", uNoTotal.TotalTokens)
	}

	// json.Number parsing
	jsonBlob := `{"input_tokens": 1234, "output_tokens": 56, "total_tokens": 1290}`
	var jsonMap map[string]any
	dec := json.NewDecoder(strings.NewReader(jsonBlob))
	dec.UseNumber()
	if err := dec.Decode(&jsonMap); err != nil {
		t.Fatal(err)
	}
	uNum, err := ParseStrictUsage(jsonMap)
	if err != nil {
		t.Fatalf("unexpected error with json.Number: %v", err)
	}
	if uNum.InputTokens != 1234 || uNum.OutputTokens != 56 || uNum.TotalTokens != 1290 {
		t.Fatalf("mismatch with json.Number: %+v", uNum)
	}

	// Negative token count rejected
	badNegative := map[string]any{"input_tokens": float64(-1)}
	if _, err := ParseStrictUsage(badNegative); err == nil {
		t.Fatal("expected error for negative input_tokens")
	}

	// Float token count rejected
	badFloat := map[string]any{"input_tokens": 1.5}
	if _, err := ParseStrictUsage(badFloat); err == nil {
		t.Fatal("expected error for non-integer input_tokens")
	}

	// Bad model string (control characters) rejected
	badModel := map[string]any{"model": "bad\x00model"}
	if _, err := ParseStrictUsage(badModel); err == nil {
		t.Fatal("expected error for control characters in model")
	}

	// Non-object usage rejected
	if _, err := ParseStrictUsage("not an object"); err == nil {
		t.Fatal("expected error for non-object usage")
	}

	// Nil usage returns nil, nil
	uNil, err := ParseStrictUsage(nil)
	if err != nil || uNil != nil {
		t.Fatalf("expected nil, nil for nil raw usage, got %+v, %v", uNil, err)
	}
}

func TestUsageAggregation(t *testing.T) {
	u1 := &TaskUsage{
		InputTokens:      10,
		OutputTokens:     5,
		ThinkingTokens:   2,
		CacheReadTokens:  1,
		CacheWriteTokens: 1,
		TotalTokens:      19,
		Model:            "model-a",
	}
	u2 := &TaskUsage{
		InputTokens:      20,
		OutputTokens:     10,
		ThinkingTokens:   0,
		CacheReadTokens:  5,
		CacheWriteTokens: 0,
		TotalTokens:      35,
	}

	sum := AddUsage(nil, u1)
	if sum.TotalTokens != 19 || sum.Model != "model-a" {
		t.Fatalf("expected 19 total, model-a, got %+v", sum)
	}

	sum = AddUsage(sum, u2)
	if sum.InputTokens != 30 || sum.OutputTokens != 15 || sum.ThinkingTokens != 2 ||
		sum.CacheReadTokens != 6 || sum.CacheWriteTokens != 1 || sum.TotalTokens != 54 {
		t.Fatalf("aggregation mismatch: %+v", sum)
	}
	// Model should be preserved
	if sum.Model != "model-a" {
		t.Fatalf("expected model preserved as model-a, got %q", sum.Model)
	}

	// Adding nil does nothing
	sum = AddUsage(sum, nil)
	if sum.TotalTokens != 54 {
		t.Fatalf("expected 54 total after adding nil, got %d", sum.TotalTokens)
	}
}

func TestUsageFormatSummary(t *testing.T) {
	if s := FormatTokenSummary(nil); s != "" {
		t.Fatalf("expected empty for nil, got %q", s)
	}
	if s := FormatTokenSummary(&TaskUsage{}); s != "" {
		t.Fatalf("expected empty for zero tokens, got %q", s)
	}

	u := &TaskUsage{
		InputTokens:      1000,
		OutputTokens:     200,
		ThinkingTokens:   50,
		CacheReadTokens:  100,
		CacheWriteTokens: 25,
		TotalTokens:      1375,
	}
	want := "1375 tokens (1000 in, 200 out, 50 thinking, 100 cached, 25 cache-write)"
	if got := FormatTokenSummary(u); got != want {
		t.Fatalf("formatTokenSummary mismatch:\nwant: %s\ngot:  %s", want, got)
	}

	uSimple := &TaskUsage{
		InputTokens:  10,
		OutputTokens: 5,
		TotalTokens:  15,
	}
	wantSimple := "15 tokens (10 in, 5 out)"
	if got := FormatTokenSummary(uSimple); got != wantSimple {
		t.Fatalf("formatTokenSummary mismatch:\nwant: %s\ngot:  %s", wantSimple, got)
	}
}
