package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/textsafe"
)

func boundaryHook(t *testing.T, ws, id string, fields map[string]any) string {
	t.Helper()
	fields["session_id"], fields["cwd"] = id, ws
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func boundaryCLI(t *testing.T, args ...string) string {
	t.Helper()
	var code int
	var err error
	out := runfixture.CaptureStdout(t, func() { code, err = SessionCLI(args) })
	if code != 0 || err != nil {
		t.Fatalf("session %v: %d %v", args, code, err)
	}
	return out
}

func requirePlainSafe(t *testing.T, out string) {
	t.Helper()
	if !utf8.ValidString(out) {
		t.Fatalf("invalid UTF-8 output: %q", out)
	}
	for _, r := range out {
		if r == '\r' || r == 0x1b || r == '\a' || r == 0 || r == 0x202e || r == 0x200b || r == 0x2028 || r == 0x2029 {
			t.Fatalf("unsafe rune %U in plain output: %q", r, out)
		}
	}
}

func TestSessionBoundaryRecordReadAndPresentOnce(t *testing.T) {
	ws := sessionTestWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws, ".irrigator", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".irrigator", "sessions", ".enabled"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := "C:\\Users\\me \x1b[2J\r\u202e\u200b\u2028x\nsecond line"
	title := strings.SplitN(prompt, "\n", 2)[0]
	reply := "done D:\\tmp\\n \x1b]0;pwn\a\u2029z"
	waiting := "allow C:\\x\x1b[0m?"
	detail := "go test C:\\pkg\x1b[1m\u202e"
	recordAll(t,
		boundaryHook(t, ws, "s1", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": prompt}),
		boundaryHook(t, ws, "s1", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": detail}}),
		boundaryHook(t, ws, "s1", map[string]any{"hook_event_name": "Stop", "last_assistant_message": reply}),
		boundaryHook(t, ws, "s2", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "needs input"}),
		boundaryHook(t, ws, "s2", map[string]any{"hook_event_name": "Notification", "notification_type": "permission_prompt", "message": waiting}),
	)
	feed := filepath.Join(ws, ".irrigator", "sessions", "claude-s1.jsonl")
	rawBefore, err := os.ReadFile(feed)
	if err != nil {
		t.Fatal(err)
	}

	list := boundaryCLI(t, "list", "--workspace", ws)
	requirePlainSafe(t, list)
	if c := strings.Count(list, `\u202E`); c != 1 {
		t.Fatalf("title marker count %d: %q", c, list)
	}
	if c := strings.Count(list, `C:\\Users\\me \e[2J\r\u202E\u200B\u2028x`); c != 1 || strings.Contains(list, `\\\\`) {
		t.Fatalf("title not sanitized exactly once: %q", list)
	}

	show := boundaryCLI(t, "show", "--workspace", ws, "claude-s1")
	requirePlainSafe(t, show)
	for literal, want := range map[string]int{
		`\u202E`: 2, `\e[2J`: 2, `\\Users`: 2, `\u200B`: 2, `\u2028`: 2, `\r`: 2,
		`\\tmp\\n`: 1, `\e]0;pwn\a\u2029z`: 1, `1 tool call(s)`: 1,
	} {
		if got := strings.Count(show, literal); got != want {
			t.Errorf("%q count %d want %d in %q", literal, got, want, show)
		}
	}
	if strings.Contains(show, `\\\\`) || strings.Contains(show, `\\e`) {
		t.Fatalf("escape markers were multiplied: %q", show)
	}
	if !strings.Contains(show, "\n› "+textsafe.Terminal(title)+"\n") {
		t.Fatalf("prompt line: %q", show)
	}

	waitOut := boundaryCLI(t, "show", "--workspace", ws, "claude-s2")
	requirePlainSafe(t, waitOut)
	if !strings.Contains(waitOut, `Waiting: allow C:\\x\e[0m? —`) || strings.Count(waitOut, `\\x`) != 1 {
		t.Fatalf("waiting line: %q", waitOut)
	}

	// --json keeps structured raw data (JSON encoding handles controls).
	var v sessionView
	if err := json.Unmarshal([]byte(boundaryCLI(t, "show", "--workspace", ws, "--json", "claude-s1")), &v); err != nil {
		t.Fatal(err)
	}
	if v.Title != title || v.Turns[0].Prompt != prompt || v.Turns[0].Reply != reply || v.Turns[0].Tools[0].Detail != detail {
		t.Fatalf("json view is not raw: %#v", v)
	}
	if after, _ := os.ReadFile(feed); string(after) != string(rawBefore) {
		t.Fatal("presenting modified the durable feed")
	}
}

func TestSessionBoundaryLegacyStoredTextRendersOnceAndFileIsUnchanged(t *testing.T) {
	ws := sessionTestWorkspace(t)
	dir := filepath.Join(ws, ".irrigator", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"at":"2025-01-01T00:00:00Z","host":"claude","session":"legacy","kind":"prompt","text":"C:\\\\Users\\\\me \\\\e[31m"}` + "\n"
	path := filepath.Join(dir, "claude-legacy.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	stored := `C:\\Users\\me \\e[31m`
	for _, args := range [][]string{{"list", "--workspace", ws}, {"show", "--workspace", ws, "claude-legacy"}} {
		out := boundaryCLI(t, args...)
		requirePlainSafe(t, out)
		// Stored bytes are rendered once (their backslashes are literal), never decoded.
		if !strings.Contains(out, textsafe.Terminal(stored)) || strings.Contains(out, `\\\\\\\\`) {
			t.Fatalf("legacy render %v: %q", args, out)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != line {
		t.Fatal("legacy feed was rewritten")
	}
}
