package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreOptInAndFeedLifecycle(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRRIGATOR_WORKSPACE", "")
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	payload := `{"session_id":"sample","cwd":` + jsonString(ws) + `,"hook_event_name":"UserPromptSubmit","prompt":"hello"}`
	if err := record("claude", strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if feeds, err := list(ws); err != nil || len(feeds) != 0 {
		t.Fatalf("record without opt-in: %v %v", feeds, err)
	}
	if err := enable(ws); err != nil {
		t.Fatal(err)
	}
	if !enabledAt(ws) {
		t.Fatal("enable marker missing")
	}
	if err := record("claude", strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	feeds, err := list(ws)
	if err != nil || len(feeds) != 1 || len(feeds[0].Turns) != 1 || feeds[0].Turns[0].Prompt != "hello" {
		t.Fatalf("recorded feed: %#v %v", feeds, err)
	}
	if err := removeEntry(ws, "claude-sample.jsonl"); err != nil {
		t.Fatal(err)
	}
	if feeds, err = list(ws); err != nil || len(feeds) != 0 {
		t.Fatalf("deleted feed: %v %v", feeds, err)
	}
	for _, host := range []string{"gemini", "agy"} {
		if err := record(host, strings.NewReader(payload)); err != nil {
			t.Fatalf("record %s: %v", host, err)
		}
		if err := removeEntry(ws, host+"-sample.jsonl"); err != nil {
			t.Fatalf("remove %s: %v", host, err)
		}
	}
	for _, badID := range []string{"latest", "LATEST", "1", "42", "-flag"} {
		if validID(badID) {
			t.Fatalf("accepted relative or unsafe session ID %q", badID)
		}
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestRecordPastFileLimitDropsSilently(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRRIGATOR_WORKSPACE", "")
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	if err := enable(ws); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"capped","cwd":` + jsonString(ws) + `,"hook_event_name":"UserPromptSubmit","prompt":"hello"}`
	if err := record("claude", strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(ws, ".irrigator", "sessions", "claude-capped.jsonl")
	if err := os.Truncate(file, fileLimit+1); err != nil {
		t.Fatal(err)
	}
	if err := record("claude", strings.NewReader(payload)); err != nil {
		t.Fatalf("record past the cap: %v", err)
	}
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != fileLimit+1 {
		t.Fatalf("size = %d, want %d (event must be dropped)", st.Size(), fileLimit+1)
	}
}
