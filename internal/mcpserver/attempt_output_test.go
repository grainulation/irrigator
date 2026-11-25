package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func outputFixture(t *testing.T, content string) (attemptOutputInput, string) {
	t.Helper()
	root := runfixture.New(t, runfixture.Task("worker", 0))
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["worker"].Attempt = 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(root, "cells", "worker", "1")
	if err := os.MkdirAll(filepath.Dir(prefix), 0700); err != nil {
		t.Fatal(err)
	}
	for _, attempt := range []string{"1", "2"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(prefix), attempt+".stdout.json"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return attemptOutputInput{Run: root, Task: "worker", Attempt: 1, MaxBytes: 5}, prefix
}

func TestAttemptOutputPagesExactSnapshotAndRedactsBeforeSlicing(t *testing.T) {
	t.Setenv("IRRIGATOR_TEST_API_KEY", "secret-from-environment")
	content := "Aé界🙂\x1b]52;c;bad\x07\npassword=hidden-password Bearer private-access-token secret-from-environment finished"
	in, prefix := outputFixture(t, content)
	var joined strings.Builder
	for i := 0; i < 200; i++ {
		page, err := readAttemptOutput(in)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(page.Output) || len(page.Output) > 5 || page.Offset >= page.NextOffset {
			t.Fatalf("invalid bounded page: %+v", page)
		}
		if page.SourceTruncated != nil || page.CaptureComplete != nil || page.CaptureMetadata != "unrecorded" {
			t.Fatal("legacy source guessed capture completeness or loss")
		}
		joined.WriteString(page.Output)
		if !page.HasMore {
			if page.NextCursor != "" || page.NextOffset != page.TotalBytes {
				t.Fatal("EOF carries an invalid continuation")
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatal("missing continuation")
		}
		in.Cursor = page.NextCursor
	}
	got := joined.String()
	for _, forbidden := range []string{"hidden-password", "private-access-token", "secret-from-environment", "\x1b", "\x07"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("unsafe output contains %q: %s", forbidden, got)
		}
	}
	if !strings.Contains(got, "Aé界🙂") || !strings.Contains(got, "finished") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("paging lost safe content: %q", got)
	}
	raw, _ := os.ReadFile(prefix + ".stdout.json")
	if string(raw) != content {
		t.Fatal("reader changed source evidence")
	}
}

func TestAttemptOutputRedactsKnownSecretsBeforeJSONEscaping(t *testing.T) {
	// A known secret in an ordinary result string must stay redacted even when
	// JSON serialization would escape quotes, backslashes or HTML characters.
	for _, secret := range []string{`private<key>&value`, `private"key\value`, "private\nkey"} {
		t.Run(secret, func(t *testing.T) {
			t.Setenv("IRRIGATOR_TEST_API_KEY", secret)
			raw, err := json.Marshal(map[string]any{"answer": "before " + secret + " after"})
			if err != nil {
				t.Fatal(err)
			}
			in, _ := outputFixture(t, string(raw))
			in.MaxBytes = 8192
			page, err := readAttemptOutput(in)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(page.Output), &got); err != nil {
				t.Fatalf("invalid redacted JSON: %s: %v", page.Output, err)
			}
			if got["answer"] != "before [redacted] after" {
				t.Fatalf("known secret escaped redaction: %q", got["answer"])
			}
			keyRaw, err := json.Marshal(map[string]string{secret: "safe"})
			if err != nil {
				t.Fatal(err)
			}
			if redacted := redactAttemptText(keyRaw); redacted != `{"[redacted]":"safe"}` {
				t.Fatalf("known secret in object key escaped redaction: %s", redacted)
			}
			jsonl := string(raw) + "\n" + string(raw) + "\n"
			if redacted := redactAttemptText([]byte(jsonl)); strings.Contains(redacted, "private") {
				t.Fatalf("known secret in JSON-lines escaped redaction: %s", redacted)
			}
		})
	}
}

func TestAttemptOutputRedactsJSONLineContainersBeforePaging(t *testing.T) {
	content := "provider log before JSON\n" +
		`{"credentials":["first-private-value","second-private-value"],"answer":"first-safe","env":{"PRIVATE_ENV":"hidden-environment-value"}}` + "\n" +
		`{"nested":{"api_keys":[{"value":"third-private-value"},{"value":"fourth-private-value"}]},"answer":"second-safe","count":9007199254740993}` + "\n"
	in, prefix := outputFixture(t, content)
	if err := os.WriteFile(prefix+".stderr.log", []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		cfg.Workspace = filepath.Dir(in.Run)
		cs := connectDefinitionTest(t, cfg)
		for _, stream := range []string{"stdout", "stderr"} {
			args := map[string]any{"run": in.Run, "task": in.Task, "attempt": in.Attempt, "stream": stream, "max_bytes": 17}
			var joined strings.Builder
			for pageNumber := 0; ; pageNumber++ {
				if pageNumber >= 256 {
					t.Fatal("pagination never finished")
				}
				page := callParameterFixture(t, cs, mcpToolName(cfg, "attempt_output"), args)
				joined.WriteString(page["output"].(string))
				if !page["has_more"].(bool) {
					break
				}
				args["cursor"] = page["next_cursor"]
			}
			got := joined.String()
			for _, forbidden := range []string{"first-private-value", "second-private-value", "third-private-value", "fourth-private-value", "hidden-environment-value"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("%s credential escaped JSON-lines redaction: %s", stream, got)
				}
			}
			for _, retained := range []string{"provider log before JSON", "first-safe", "second-safe", "9007199254740993", "[redacted]"} {
				if !strings.Contains(got, retained) {
					t.Fatalf("%s lost non-sensitive output %q: %s", stream, retained, got)
				}
			}
		}
	})
	for _, suffix := range []string{".stdout.json", ".stderr.log"} {
		raw, err := os.ReadFile(prefix + suffix)
		if err != nil || string(raw) != content {
			t.Fatalf("source capture was changed: %v", err)
		}
	}
}

func TestAttemptOutputCursorRejectsChangedSourceAndDifferentAttempt(t *testing.T) {
	in, prefix := outputFixture(t, "abcdefghijk")
	first, err := readAttemptOutput(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Cursor = first.NextCursor
	in.Attempt = 2
	if _, err := readAttemptOutput(in); err == nil {
		t.Fatal("cursor from a different attempt was accepted")
	}
	in.Attempt = 1
	if err := os.WriteFile(prefix+".stdout.json", []byte("abcdefghijk changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAttemptOutput(in); err == nil {
		t.Fatal("cursor from a changed source was accepted")
	}
}

func TestAttemptOutputRefusesUnsafePathsAndUnknownAttempts(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*attemptOutputInput, string)
	}{
		{"escape", func(in *attemptOutputInput, _ string) { in.Task = "../worker" }},
		{"unknown attempt", func(in *attemptOutputInput, _ string) { in.Attempt = 3 }},
		{"input stream", func(in *attemptOutputInput, _ string) { in.Stream = "input" }},
		{"too small", func(in *attemptOutputInput, _ string) { in.MaxBytes = 1 }},
		{"too large", func(in *attemptOutputInput, _ string) { in.MaxBytes = 8193 }},
		{"malformed cursor", func(in *attemptOutputInput, _ string) { in.Cursor = "invalid%cursor" }},
		{"source symlink", func(_ *attemptOutputInput, prefix string) {
			if err := os.Rename(prefix+".stdout.json", prefix+".outside"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(prefix+".outside", prefix+".stdout.json"); err != nil {
				t.Fatal(err)
			}
		}},
		{"ancestor symlink", func(in *attemptOutputInput, _ string) {
			link := filepath.Join(filepath.Dir(in.Run), "run-alias")
			if err := os.Symlink(in.Run, link); err != nil {
				t.Fatal(err)
			}
			in.Run = link
		}},
		{"checkpoint symlink", func(in *attemptOutputInput, _ string) {
			p := filepath.Join(in.Run, "state.json")
			if err := os.Rename(p, p+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(p+".saved", p); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			in, prefix := outputFixture(t, "hello output")
			test.edit(&in, prefix)
			if _, err := readAttemptOutput(in); err == nil {
				t.Fatal("unsafe request accepted")
			}
		})
	}
}

func TestAttemptOutputDistinguishesUnreadPageFromCaptureLoss(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		in, prefix := outputFixture(t, "more than one page")
		digest := sha256.Sum256([]byte("more than one page"))
		raw, _ := json.Marshal(map[string]any{
			"schema": "irrigator.process-capture/v1",
			"streams": map[string]any{"stdout": map[string]any{
				"sha256": hex.EncodeToString(digest[:]), "captured_bytes": len("more than one page"),
				"limit_bytes": model.MaxOutputBytes, "truncated": truncated, "complete": true,
			}},
		})
		if err := os.WriteFile(prefix+".capture.json", raw, 0600); err != nil {
			t.Fatal(err)
		}
		page, err := readAttemptOutput(in)
		if err != nil || !page.HasMore || page.SourceTruncated == nil || *page.SourceTruncated != truncated || page.CaptureComplete == nil || !*page.CaptureComplete {
			t.Fatalf("page/capture distinction lost: %+v %v", page, err)
		}
		in.MaxBytes = 8192
		page, err = readAttemptOutput(in)
		if err != nil || page.HasMore || *page.SourceTruncated != truncated {
			t.Fatalf("EOF changed capture loss: %+v %v", page, err)
		}
		if err := os.WriteFile(prefix+".stdout.json", []byte("different"), 0600); err != nil {
			t.Fatal(err)
		}
		page, err = readAttemptOutput(in)
		if err != nil || page.CaptureMetadata != "mismatch" || page.SourceTruncated != nil {
			t.Fatalf("mismatched capture metadata trusted: %+v %v", page, err)
		}
	}
}

func TestAttemptOutputRedactsNestedJSONAndReadOnlyMCP(t *testing.T) {
	in, _ := outputFixture(t, `{"answer":"safe","api_key":"never-return","nested":{"authorization":["sensitive"]},"env":{"SECRET":"another-secret"}}`)
	in.MaxBytes = 8192
	tools := &mcpTools{cfg: McpConfig{Workspace: filepath.Dir(in.Run)}}
	_, out, err := tools.attemptOutput(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "never-return") || strings.Contains(string(raw), "sensitive") || strings.Contains(string(raw), "another-secret") || !strings.Contains(string(raw), "safe") {
		t.Fatalf("MCP output was not redacted: %s", raw)
	}
	code, err := AttemptOutputCLI([]string{commandflags.MetadataArgument})
	var metadata *commandflags.CommandMetadata
	if code != 2 || !errors.As(err, &metadata) {
		t.Fatalf("CLI metadata probe did not exit before reading a run: %d %v", code, err)
	}
	if len(metadata.Flags) != 4 {
		t.Fatalf("CLI flags missing from discovery: %+v", metadata)
	}
	jsonl := []byte("{\"api_key\":\"escaped\\\"suffix\"}\n{\"password\":\"second-secret\"}\n")
	redacted := redactAttemptText(jsonl)
	if strings.Contains(redacted, "escaped") || strings.Contains(redacted, "suffix") || strings.Contains(redacted, "second-secret") {
		t.Fatalf("JSONL quoted credentials escaped redaction: %s", redacted)
	}
}

func TestAttemptOutputCLIReturnsBoundedJSONPage(t *testing.T) {
	in, _ := outputFixture(t, "hello saved output")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	code, runErr := AttemptOutputCLI([]string{in.Run, in.Task, "--attempt", "1", "--max-bytes", "4"})
	os.Stdout = stdout
	_ = w.Close()
	raw, readErr := io.ReadAll(r)
	_ = r.Close()
	if code != 0 || runErr != nil || readErr != nil {
		t.Fatalf("CLI failed: %d %v %v", code, runErr, readErr)
	}
	var page attemptOutputPage
	if err := json.Unmarshal(raw, &page); err != nil || page.Output != "hell" || !page.HasMore || page.NextCursor == "" || page.SourceTruncated != nil {
		t.Fatalf("unexpected CLI page: %s %v", raw, err)
	}
}

func TestAttemptOutputToolDiscoveryAndProtocolInvocation(t *testing.T) {
	in, _ := outputFixture(t, "hello saved output")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := NewMCPServer(McpConfig{Workspace: filepath.Dir(in.Run)})
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	client := mcp.NewClient(&mcp.Implementation{Name: "attempt-output-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	diagnostics := map[string]bool{"provider_limit": false, "delivery_summary": false}
	for _, tool := range listed.Tools {
		if tool.Name == "attempt_output" {
			found = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		}
		if tool.Name == "apply" {
			t.Fatal("read-only server unexpectedly advertised control tools")
		}
		if _, ok := diagnostics[tool.Name]; ok {
			diagnostics[tool.Name] = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		}
	}
	if !found {
		t.Fatal("attempt_output missing from read-only tool discovery")
	}
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "attempt_output", Arguments: map[string]any{
		"run": in.Run, "task": in.Task, "attempt": in.Attempt, "max_bytes": 4,
	}})
	if err != nil || result.IsError {
		t.Fatalf("MCP invocation failed: %+v %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var page attemptOutputPage
	if err := json.Unmarshal(raw, &page); err != nil || page.Output != "hell" || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("unexpected protocol page: %s %v", raw, err)
	}
	for name, readOnly := range diagnostics {
		if !readOnly {
			t.Fatalf("%s missing read-only discovery annotation", name)
		}
		args := map[string]any{"run": in.Run}
		if name == "provider_limit" {
			args["node"], args["attempt"] = in.Task, in.Attempt
		}
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("%s invocation failed: %+v %v", name, result, err)
		}
		encoded, _ := json.Marshal(result.StructuredContent)
		if name == "provider_limit" && !strings.Contains(string(encoded), `"automatic_retry":false`) {
			t.Fatalf("limit advisory lost no-retry boundary: %s", encoded)
		}
		if name == "delivery_summary" && !strings.Contains(string(encoded), `"dispatched":false`) {
			t.Fatalf("delivery summary started work: %s", encoded)
		}
	}
}
