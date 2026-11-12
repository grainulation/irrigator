package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/textsafe"
)

func boundaryWorkspace(t *testing.T) string {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRRIGATOR_WORKSPACE", "")
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	if err := enable(ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

func hookPayload(t *testing.T, ws string, fields map[string]any) string {
	t.Helper()
	fields["session_id"], fields["cwd"] = "s1", ws
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRecordKeepsRawDataAndReadFileDoesNotMultiplyText(t *testing.T) {
	ws := boundaryWorkspace(t)
	prompt := "C:\\Users\\me \x1b[2J\r\u202e\u200b\u2028x\nsecond"
	title := "C:\\Users\\me \x1b[2J\r\u202e\u200b\u2028x"
	reply := "done D:\\tmp\\n \x1b]0;pwn\a\u2029z"
	detail := "go test C:\\pkg\x1b[1m\u202e"
	toolFile := "a\u2028b\\c.go"
	for _, p := range []map[string]any{
		{"hook_event_name": "UserPromptSubmit", "prompt": prompt},
		{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": detail + "\nsecond"}},
		{"hook_event_name": "PostToolUseFailure", "tool_name": "Edit", "tool_input": map[string]any{"file_path": filepath.Join(ws, toolFile)}},
		{"hook_event_name": "PostToolUse", "tool_name": "mcp__srv\x1b__nm\\x"},
		{"hook_event_name": "Stop", "last_assistant_message": reply},
	} {
		if err := record("claude", strings.NewReader(hookPayload(t, ws, p))); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(ws, ".irrigator", "sessions", "claude-s1.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []event
	for _, line := range bytes.Split(bytes.TrimSpace(before), []byte("\n")) {
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if len(events) != 5 || events[0].Text != prompt || events[1].Detail != detail || events[2].Detail != toolFile ||
		events[3].Tool != "srv\x1b: nm\\x" || events[4].Text != reply {
		t.Fatalf("durable events are not raw: %#v", events)
	}
	first, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeat ReadFile changed the view")
	}
	turn := first.Turns[0]
	if first.Title != title || turn.Prompt != prompt || turn.Reply != reply || turn.Tools[0].Detail != detail || turn.Tools[1].Detail != toolFile {
		t.Fatalf("parsed view is not raw: %#v", first)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("reading modified the durable feed")
	}
}

func TestLegacyStoredTextReadsUnchanged(t *testing.T) {
	ws := boundaryWorkspace(t)
	line := `{"at":"2025-01-01T00:00:00Z","host":"claude","session":"legacy","kind":"prompt","text":"C:\\\\Users\\\\me \\\\e[31m \\\\u202E"}` + "\n"
	path := filepath.Join(ws, ".irrigator", "sessions", "claude-legacy.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	stored := `C:\\Users\\me \\e[31m \\u202E`
	for i := 0; i < 2; i++ {
		v, err := readFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if v.Turns[0].Prompt != stored || v.Title != stored {
			t.Fatalf("legacy text reinterpreted: %q %q", v.Turns[0].Prompt, v.Title)
		}
		// Presentation renders the stored bytes once; it does not decode them.
		if got := clip(v.Turns[0].Prompt, 300); got != textsafe.Terminal(stored) {
			t.Fatalf("legacy display: %q", got)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != line {
		t.Fatal("legacy feed was rewritten")
	}
}

func TestRawClipIsNotSanitizedAndKeepsRunes(t *testing.T) {
	in := "a\\b\x1b\u202ec"
	if got := rawClip("  "+in+"  ", 100); got != "  "+in+"  " {
		t.Fatalf("raw clip changed data: %q", got)
	}
	got := rawClip("aé", 2) // byte 2 is inside é
	if got != "a…" || !utf8.ValidString(got) {
		t.Fatalf("raw rune cut: %q", got)
	}
}

func TestRecordKeepsBoundaryWhitespaceAndFormatSeparators(t *testing.T) {
	ws := boundaryWorkspace(t)
	prompt := " \r\u0085\u2028boundary\u2029\r "
	if err := record("claude", strings.NewReader(hookPayload(t, ws, map[string]any{
		"hook_event_name": "UserPromptSubmit", "prompt": prompt,
	}))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, ".irrigator", "sessions", "claude-s1.jsonl")
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var event event
	if err := json.Unmarshal(bytes.TrimSpace(stored), &event); err != nil {
		t.Fatal(err)
	}
	view, err := readFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if event.Text != prompt || view.Turns[0].Prompt != prompt {
		t.Fatalf("boundary data was normalized: stored=%q parsed=%q want=%q", event.Text, view.Turns[0].Prompt, prompt)
	}
}

func TestClipDoesNotCutEscapeMarkers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"aaa\x1bbbb", 4, "aaa…"},
		{"aaa\x1bbbb", 5, `aaa\e…`},
		{"a\u202eb", 3, "a…"},
		{"a\u202eb", 7, `a\u202E…`},
		{"a\U000e0001b", 9, "a…"},
		{"a\xffb", 3, "a…"},
		{`a\b`, 2, "a…"},
		{`a\b`, 3, `a\\…`},
		{"aéb", 2, "a…"},
	} {
		if got := clip(tc.in, tc.n); got != tc.want {
			t.Errorf("Clip(%q,%d)=%q want %q", tc.in, tc.n, got, tc.want)
		}
	}
	in := "x\\y\x00\x1b\r\u202e\u200b\u2028\U000e0001\xff\xc3é\\\\z" + strings.Repeat("q\\\x1b", 20)
	full := textsafe.Terminal(in)
	for n := 0; n <= len(full)+2; n++ {
		got := clip(in, n)
		body := strings.TrimSuffix(got, "…")
		if !utf8.ValidString(got) || !strings.HasPrefix(full, body) || len(body) > n {
			t.Fatalf("Clip n=%d: %q", n, got)
		}
		// The body must end exactly on a token boundary of the full text.
		off := 0
		for off < len(body) {
			off += displayTokenLen(full[off:])
		}
		if off != len(body) {
			t.Fatalf("Clip n=%d cut inside marker: %q", n, got)
		}
	}
}
