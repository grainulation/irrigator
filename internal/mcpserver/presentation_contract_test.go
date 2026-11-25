package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPCredentialContainersAndPublicTokenAccounting(t *testing.T) {
	for _, secret := range []any{
		"fixture-secret", 12345, true, []any{"fixture-secret"},
		map[string]any{"value": "fixture-secret"},
		map[string]any{"nested": []any{"fixture-secret"}},
	} {
		t.Run(fmt.Sprintf("%T", secret), func(t *testing.T) {
			clean, _ := mcpSanitize(mcpOut{"credentials": secret, "token": secret, "password": secret, "ordinary": "keep"})
			got := clean.(map[string]any)
			for _, key := range []string{"credentials", "token", "password"} {
				if got[key] != "[redacted]" {
					t.Fatalf("%s retained a credential value: %#v", key, got[key])
				}
			}
			if got["ordinary"] != "keep" {
				t.Fatalf("ordinary field lost: %#v", got)
			}
		})
	}
	usage := &model.TaskUsage{InputTokens: 10, OutputTokens: 20, ThinkingTokens: 3, CacheReadTokens: 5, CacheWriteTokens: 7, TotalTokens: 45, Model: "fixture"}
	clean, _ := mcpSanitize(mcpOut{
		"cost": mcpOut{"tokens": usage}, "usage": usage,
		"max_output_tokens":             512,
		"provider_reported_token_usage": mcpOut{"input_tokens": 10, "cached_input_tokens": 2, "cache_write_input_tokens": 3, "output_tokens": 20, "reasoning_output_tokens": 4},
		"provider_reported_model_usage": mcpOut{"fixture-model": mcpOut{"inputTokens": 2, "outputTokens": 704, "cacheReadInputTokens": 513, "cacheCreationInputTokens": 783, "thinkingTokens": 0, "maxOutputTokens": 128000, "costUSD": 0.00051393, "canonicalModel": "fixture-model", "provider": "firstParty", "costBasis": "list"}},
		"invalid_metric":                mcpOut{"input_tokens": []string{"fixture-secret"}},
		"invalid_container":             mcpOut{"tokens": mcpOut{"value": "fixture-secret"}},
		"invalid_model_container":       mcpOut{"tokens": mcpOut{"model": "fixture-secret"}},
	})
	got := clean.(map[string]any)
	for _, accounting := range []map[string]any{got["usage"].(map[string]any), got["cost"].(map[string]any)["tokens"].(map[string]any)} {
		if accounting["input_tokens"] != json.Number("10") || accounting["total_tokens"] != json.Number("45") || accounting["model"] != "fixture" {
			t.Fatalf("public usage accounting changed: %#v", accounting)
		}
	}
	if got["max_output_tokens"] != json.Number("512") {
		t.Fatalf("token limit changed: %#v", got)
	}
	if got["provider_reported_token_usage"] != "[redacted]" {
		t.Fatal("opaque credential-looking receipt container was not redacted")
	}
	modelUsage := got["provider_reported_model_usage"].(map[string]any)["fixture-model"].(map[string]any)
	for field, want := range map[string]float64{"inputTokens": 2, "outputTokens": 704, "cacheReadInputTokens": 513, "cacheCreationInputTokens": 783, "thinkingTokens": 0, "maxOutputTokens": 128000} {
		if modelUsage[field] != json.Number(fmt.Sprintf("%.0f", want)) {
			t.Fatalf("provider receipt metric %s changed type or value: %#v", field, modelUsage[field])
		}
	}
	for _, container := range []string{"invalid_metric", "invalid_container", "invalid_model_container"} {
		encoded, _ := json.Marshal(got[container])
		if strings.Contains(string(encoded), "fixture-secret") {
			t.Fatalf("credential survived in %s: %s", container, encoded)
		}
	}
	clean, _ = mcpSanitize(mcpOut{"cost": mcpOut{"tokens": &model.TaskUsage{Model: "model-only-accounting"}}})
	if got := clean.(map[string]any)["cost"].(map[string]any)["tokens"].(map[string]any)["model"]; got != "model-only-accounting" {
		t.Fatalf("typed model-only cost accounting changed: %#v", clean)
	}
}

func TestMCPTruncationIncludesNoticeWithinByteLimit(t *testing.T) {
	for _, limit := range []int{0, 1, 2, 3, 5, 48, McpErrorCap, McpTextCap} {
		for _, unit := range []string{"x", "é", "🌾"} {
			for _, delta := range []int{-1, 0, 1, 40} {
				size := max(0, limit+delta)
				value := strings.Repeat(unit, size/len(unit))
				value += strings.Repeat("x", size-len(value))
				t.Run(fmt.Sprintf("limit=%d/unit=%q/delta=%d", limit, unit, delta), func(t *testing.T) {
					got, cut := mcpTruncate(value, limit)
					if len(got) > limit || !utf8.ValidString(got) {
						t.Fatalf("invalid bounded string: %d bytes, limit %d, valid=%v", len(got), limit, utf8.ValidString(got))
					}
					if cut != (len(value) > limit) {
						t.Fatalf("cut=%v for %d bytes at %d", cut, len(value), limit)
					}
					if !cut && got != value {
						t.Fatal("short value changed")
					}
					if cut && strings.Contains(got, "bytes shown]") {
						marker := strings.Index(got, "… [truncated:")
						want := fmt.Sprintf("… [truncated: %d of %d bytes shown]", marker, len(value))
						if !strings.HasSuffix(got, want) {
							t.Fatalf("inaccurate notice: %q", got)
						}
					}
				})
			}
		}
	}
}

func TestMCPResultCapsSummaryAndRetainsStructuredDisclosure(t *testing.T) {
	for _, value := range []string{"short", strings.Repeat("é", McpTextCap)} {
		result, out, err := mcpResultWithTruncation(strings.Repeat("s", McpTextCap+1), mcpOut{"value": value}, []string{"$.early.error"})
		if err != nil {
			t.Fatal(err)
		}
		text := result.Content[0].(*mcp.TextContent).Text
		if len(text) > McpTextCap || !utf8.ValidString(text) || !strings.Contains(text, `see "truncated"`) || !strings.Contains(text, "[truncated:") {
			t.Fatalf("summary cap or disclosure lost: length=%d suffix=%q", len(text), text[max(0, len(text)-160):])
		}
		paths := out["truncated"].([]string)
		if len(paths) == 0 || paths[0] != "$.early.error" {
			t.Fatalf("early truncation path lost: %#v", paths)
		}
		if !strings.Contains(text, "inline byte counts") || strings.Contains(text, "truncated to 8192") {
			t.Fatalf("incorrect uniform cap disclosure: %s", text)
		}
	}
	result, _, err := mcpResult(strings.Repeat("x", McpTextCap+1), mcpOut{"ok": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if len(text) > McpTextCap || !strings.Contains(text, "[truncated:") {
		t.Fatal("summary without structured truncation was not bounded")
	}
}

func TestMCPRunStatusReportsEarlyErrorTruncationAndKeepsUsage(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Status = "failed"
		s.Tasks["a"].Error = strings.Repeat("é", McpErrorCap)
		s.Tasks["a"].Usage = &model.TaskUsage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, canonical := range []bool{false, true} {
		cfg := McpConfig{Workspace: t.TempDir(), canonicalNames: canonical}
		cs := connectDefinitionTest(t, cfg)
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpToolName(cfg, "run_status"), Arguments: map[string]any{"run": root}})
		if err != nil || result.IsError {
			t.Fatalf("run status: %v %+v", err, result)
		}
		out := result.StructuredContent.(map[string]any)
		paths := out["truncated"].([]any)
		if len(paths) != 1 || paths[0] != "$.cells[0].error" {
			t.Fatalf("wrong shortened error paths: %#v", paths)
		}
		cell := out["cells"].([]any)[0].(map[string]any)
		if text := cell["error"].(string); len(text) > McpErrorCap || !strings.Contains(text, "bytes shown]") {
			t.Fatalf("error cap or notice lost: %q", text)
		}
		if cell["usage"].(map[string]any)["total_tokens"] != float64(18) || out["cost"].(map[string]any)["tokens"].(map[string]any)["total_tokens"] != float64(18) {
			t.Fatalf("numeric usage contract changed: %#v", out)
		}
		if !strings.Contains(result.Content[0].(*mcp.TextContent).Text, `see "truncated"`) {
			t.Fatal("summary omitted structured truncation disclosure")
		}
	}
}

func TestMCPListRunsReportsCheckpointStatFailure(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"no-checkpoint", "stat-error"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink loop causes a deterministic non-absence Stat error without
	// depending on privilege-sensitive directory permission checks.
	if err := os.Symlink("state.json", filepath.Join(root, "stat-error", "state.json")); err != nil {
		t.Fatal(err)
	}
	result, out, err := (&mcpTools{cfg: McpConfig{RunRoots: []string{root}}}).listRuns(context.Background(), nil, mcpNoInput{})
	if err != nil {
		t.Fatal(err)
	}
	rows := out["runs"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["name"] != "stat-error" || rows[0].(map[string]any)["error"] == "" {
		t.Fatalf("expected exactly one stat-error row, got %#v", rows)
	}
	if got := result.Content[0].(*mcp.TextContent).Text; got != "0 run(s); 1 diagnostic row(s)" {
		t.Fatalf("diagnostic was counted as a discovered run: %q", got)
	}
}

func TestMCPOpaqueAccountingLookalikesAreRedacted(t *testing.T) {
	counts := mcpOut{"input_tokens": 42, "output_tokens": 7}
	clean, _ := mcpSanitize(mcpOut{"cost": mcpOut{"tokens": counts}, "event": mcpOut{"tokens": counts, "observed_tokens": counts, "provider_reported_token_usage": counts}})
	out := clean.(map[string]any)
	if out["cost"].(map[string]any)["tokens"].(map[string]any)["input_tokens"] != json.Number("42") {
		t.Fatal("published accounting projection changed")
	}
	for field, value := range out["event"].(map[string]any) {
		if value != "[redacted]" {
			t.Fatalf("opaque %s container copied the accounting exception: %#v", field, value)
		}
	}
}

func TestMCPResultPreservesLargeIntegerAccounting(t *testing.T) {
	const tokens int64 = 9007199254740993
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Usage = &model.TaskUsage{InputTokens: tokens, TotalTokens: tokens}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, out, err := (&mcpTools{}).runStatus(context.Background(), nil, mcpRunInput{Run: root})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil || !strings.Contains(string(raw), `"input_tokens":9007199254740993`) || strings.Contains(string(raw), "9007199254740992") {
		t.Fatalf("accounting precision lost: %s %v", raw, err)
	}
	if !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "9007199254740993") {
		t.Fatal("summary and structured accounting disagree")
	}
	diagnostic, err := mcpDiagnosticObject(model.TaskUsage{InputTokens: tokens, TotalTokens: tokens})
	if err != nil || diagnostic["input_tokens"] != json.Number("9007199254740993") {
		t.Fatalf("diagnostic conversion rounded accounting: %#v %v", diagnostic, err)
	}
}

func TestMCPRunStatusPreservesLargeIntegersOnRawWire(t *testing.T) {
	const tokens int64 = 9007199254740993
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Usage = &model.TaskUsage{InputTokens: tokens, TotalTokens: tokens}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		server, client := net.Pipe()
		if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		ss, err := NewMCPServer(cfg).Connect(ctx, &mcp.IOTransport{Reader: server, Writer: server}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = client.Close(); _ = ss.Close() }()
		enc, dec := json.NewEncoder(client), json.NewDecoder(client)
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "accounting-wire-test", "version": "1"}}}); err != nil {
			t.Fatal(err)
		}
		var initialized map[string]json.RawMessage
		if err := dec.Decode(&initialized); err != nil || initialized["error"] != nil {
			t.Fatalf("initialize: %s %v", initialized["error"], err)
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": mcpToolName(cfg, "run_status"), "arguments": map[string]any{"run": root}}}); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError           bool `json:"isError"`
				StructuredContent struct {
					Cells []struct {
						Usage map[string]json.RawMessage `json:"usage"`
					} `json:"cells"`
					Cost struct {
						Tokens map[string]json.RawMessage `json:"tokens"`
					} `json:"cost"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if err := dec.Decode(&response); err != nil || response.Error != nil || response.Result.IsError {
			t.Fatalf("raw tools/call failed: %+v %v", response, err)
		}
		out := response.Result.StructuredContent
		if len(out.Cells) != 1 {
			t.Fatalf("missing cell usage: %+v", out)
		}
		for _, usage := range []map[string]json.RawMessage{out.Cells[0].Usage, out.Cost.Tokens} {
			for _, field := range []string{"input_tokens", "total_tokens"} {
				if string(usage[field]) != "9007199254740993" {
					t.Fatalf("wire %s lost integer precision: %s", field, usage[field])
				}
			}
		}
	})
}

func TestMCPDiagnosticCapMeasuresValidUTF8(t *testing.T) {
	part := strings.Repeat(string([]byte{0xff}), 100)
	missing := filepath.Join(t.TempDir(), part, part, part, part)
	_, out, err := (&mcpTools{cfg: McpConfig{RunRoots: []string{missing}}}).listRuns(context.Background(), nil, mcpNoInput{})
	if err != nil {
		t.Fatal(err)
	}
	message := out["runs"].([]any)[0].(map[string]any)["error"].(string)
	if len(message) > McpErrorCap || !utf8.ValidString(message) {
		t.Fatalf("encoded diagnostic exceeded its cap: %d bytes valid=%v", len(message), utf8.ValidString(message))
	}
}

func TestMCPListRunsRejectsUnrepresentableSelectors(t *testing.T) {
	root := t.TempDir()
	plan := model.Plan{Tasks: []model.Task{runfixture.Task("a", 0)}}
	for _, name := range []string{"\uFFFD", string([]byte{0xff})} {
		if err := checkpoint.Initialize(plan, filepath.Join(root, name), "mock"); err != nil {
			if !utf8.ValidString(name) && errors.Is(err, syscall.EILSEQ) {
				t.Skipf("filesystem rejects invalid UTF-8 filenames: %v", err)
			}
			t.Fatal(err)
		}
	}
	_, out, err := (&mcpTools{cfg: McpConfig{RunRoots: []string{root}}}).listRuns(context.Background(), nil, mcpNoInput{})
	if err != nil {
		t.Fatal(err)
	}
	rows := out["runs"].([]any)
	if len(rows) != 2 {
		t.Fatalf("expected one successful run and one diagnostic: %#v", rows)
	}
	var successful, diagnostics int
	for _, value := range rows {
		row := value.(map[string]any)
		if row["error"] != nil {
			diagnostics++
			if row["path"] != nil || row["name"] != nil || !strings.Contains(row["error"].(string), `\xff`) {
				t.Fatalf("unrepresentable bytes produced a misleading selector: %#v", row)
			}
		} else {
			successful++
			if row["path"] != filepath.Join(root, "\uFFFD") {
				t.Fatalf("wrong successful selector: %#v", row)
			}
		}
	}
	if successful != 1 || diagnostics != 1 {
		t.Fatalf("unexpected discovery counts: %d/%d", successful, diagnostics)
	}
}

func TestMCPSanitizeCapsEnvironmentNames(t *testing.T) {
	clean, paths := mcpSanitize(mcpOut{"env": []string{strings.Repeat("A", McpTextCap+1) + "=fixture-secret"}})
	name := clean.(map[string]any)["env"].([]any)[0].(string)
	if len(name) > McpTextCap || strings.Contains(name, "fixture-secret") || !reflect.DeepEqual(paths, []string{"$.env[0]"}) {
		t.Fatalf("environment presentation escaped sanitization: length=%d paths=%#v", len(name), paths)
	}
}

func TestMCPListRunsReportsEarlyDiagnosticTruncation(t *testing.T) {
	missing := filepath.Join(t.TempDir(), strings.Repeat("x", McpErrorCap+100))
	result, out, err := (&mcpTools{cfg: McpConfig{RunRoots: []string{missing}}}).listRuns(context.Background(), nil, mcpNoInput{})
	if err != nil {
		t.Fatal(err)
	}
	paths := out["truncated"].([]string)
	if !reflect.DeepEqual(paths, []string{"$.runs[0].error"}) {
		t.Fatalf("early diagnostic truncation undisclosed: %#v", paths)
	}
	row := out["runs"].([]any)[0].(map[string]any)
	if message := row["error"].(string); len(message) > McpErrorCap || !strings.Contains(message, "bytes shown]") {
		t.Fatalf("diagnostic cap or inline count lost: %q", message)
	}
	if !strings.Contains(result.Content[0].(*mcp.TextContent).Text, `see "truncated"`) {
		t.Fatal("summary omitted diagnostic truncation")
	}
}

func TestMCPSanitizePathsDistinguishLiteralPropertyNames(t *testing.T) {
	value := strings.Repeat("x", McpTextCap+1)
	result, out, err := mcpResult("fixture", mcpOut{
		"a.b": value, "a": mcpOut{"b": value},
		`quote"key`: value, "array[0]": value, "line\nbreak": value,
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := out["truncated"].([]string)
	want := []string{"$.a.b", `$["a.b"]`, `$["array[0]"]`, `$["line\nbreak"]`, `$["quote\"key"]`}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("ambiguous truncation paths: got %#v, want %#v", paths, want)
	}
	if !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "5 structured value(s)") {
		t.Fatal("distinct property paths were merged")
	}
}
