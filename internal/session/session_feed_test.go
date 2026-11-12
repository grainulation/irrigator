package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sessionTestWorkspace(t *testing.T) string {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	t.Setenv("IRRIGATOR_WORKSPACE", "")
	return ws
}

func recordAll(t *testing.T, payloads ...string) {
	t.Helper()
	for _, p := range payloads {
		if err := recordSessionEvent("claude", strings.NewReader(p)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionRecordingIsOffUnlessEnabled(t *testing.T) {
	ws := sessionTestWorkspace(t)
	recordAll(t, `{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"`+ws+`","prompt":"secret plan"}`)
	if _, err := os.Stat(filepath.Join(ws, ".irrigator")); !os.IsNotExist(err) {
		t.Fatal("recorded without opt-in")
	}
}

func TestSessionFeedBuildsTurnsWithoutToolOutput(t *testing.T) {
	ws := sessionTestWorkspace(t)
	os.MkdirAll(filepath.Join(ws, ".irrigator", "sessions"), 0700)
	os.WriteFile(filepath.Join(ws, ".irrigator", "sessions", ".enabled"), nil, 0600)
	sub := filepath.Join(ws, "pkg")
	os.Mkdir(sub, 0700)
	recordAll(t,
		`{"hook_event_name":"SessionStart","session_id":"s1","cwd":"`+sub+`","source":"startup"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"`+sub+`","prompt":"Fix the login bug\nand add a test"}`,
		`{"hook_event_name":"PostToolUse","session_id":"s1","cwd":"`+sub+`","tool_name":"Edit","tool_input":{"file_path":"`+ws+`/auth/login.go"},"tool_output":"SECRET FILE CONTENTS"}`,
		`{"hook_event_name":"PostToolUseFailure","session_id":"s1","cwd":"`+sub+`","tool_name":"Bash","tool_input":{"command":"go test ./...\necho more"}}`,
		`{"hook_event_name":"Notification","session_id":"s1","cwd":"`+sub+`","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"}`,
	)
	sessions, err := listSessions(ws)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %v %v", sessions, err)
	}
	v := sessions[0]
	if v.State != "waiting" || v.Title != "Fix the login bug" || len(v.Turns) != 1 {
		t.Fatalf("view: %+v", v)
	}
	tools := v.Turns[0].Tools
	if len(tools) != 2 || tools[0].Detail != "auth/login.go" || tools[1].Detail != "go test ./..." || !tools[1].Failed {
		t.Fatalf("tools: %+v", tools)
	}
	recordAll(t, `{"hook_event_name":"Stop","session_id":"s1","cwd":"`+sub+`","last_assistant_message":"Fixed it."}`)
	sessions, _ = listSessions(ws)
	if sessions[0].State != "idle" || sessions[0].Turns[0].Reply != "Fixed it." {
		t.Fatalf("after stop: %+v", sessions[0])
	}
	data, _ := os.ReadFile(sessions[0].Path)
	if strings.Contains(string(data), "SECRET FILE CONTENTS") {
		t.Fatal("tool output stored")
	}
}

func TestSessionRecordRejectsUnsafeSessionIDs(t *testing.T) {
	ws := sessionTestWorkspace(t)
	os.MkdirAll(filepath.Join(ws, ".irrigator", "sessions"), 0700)
	os.WriteFile(filepath.Join(ws, ".irrigator", "sessions", ".enabled"), nil, 0600)
	recordAll(t, `{"hook_event_name":"UserPromptSubmit","session_id":"../../escape","cwd":"`+ws+`","prompt":"x"}`)
	entries, _ := os.ReadDir(filepath.Join(ws, ".irrigator", "sessions"))
	if len(entries) != 1 {
		t.Fatalf("unsafe session id recorded: %v", entries)
	}
}

func TestSessionHookArgsKeepUserSettings(t *testing.T) {
	args := []string{"--settings", "mine.json", "prompt"}
	if got := sessionHookArgs("/bin/irrigator", args); len(got) != 3 {
		t.Fatalf("user --settings replaced: %q", got)
	}
}

func TestSessionDeleteRemovesOnlyThatFeed(t *testing.T) {
	ws := sessionTestWorkspace(t)
	os.MkdirAll(filepath.Join(ws, ".irrigator", "sessions"), 0700)
	os.WriteFile(filepath.Join(ws, ".irrigator", "sessions", ".enabled"), nil, 0600)
	recordAll(t, `{"hook_event_name":"UserPromptSubmit","session_id":"keep","cwd":"`+ws+`","prompt":"a"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"drop","cwd":"`+ws+`","prompt":"b"}`)
	if code, err := SessionCLI([]string{"delete", "--workspace", ws, "claude-drop"}); code != 0 || err != nil {
		t.Fatalf("delete: %d %v", code, err)
	}
	if code, _ := SessionCLI([]string{"delete", "--workspace", ws, "claude-../x"}); code == 0 {
		t.Fatal("unsafe id accepted")
	}
	sessions, _ := listSessions(ws)
	if len(sessions) != 1 || sessions[0].SessionID != "keep" {
		t.Fatalf("remaining: %v", sessions)
	}
}

func TestSessionRemovalRefusesSymlinkedAncestors(t *testing.T) {
	for _, ancestor := range []string{".irrigator", "sessions"} {
		for _, action := range []string{"delete", "disable"} {
			t.Run(ancestor+"/"+action, func(t *testing.T) {
				ws := sessionTestWorkspace(t)
				outside := sessionTestWorkspace(t)
				name := ".enabled"
				args := []string{action, "--workspace", ws}
				if action == "delete" {
					name = "claude-keep.jsonl"
					args = append(args, "claude-keep")
				}
				target := outside
				link := filepath.Join(ws, ".irrigator")
				if ancestor == ".irrigator" {
					outside = filepath.Join(outside, "sessions")
				} else {
					if err := os.Mkdir(link, 0700); err != nil {
						t.Fatal(err)
					}
					link = filepath.Join(link, "sessions")
				}
				if err := os.MkdirAll(outside, 0700); err != nil {
					t.Fatal(err)
				}
				victim := filepath.Join(outside, name)
				if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				if code, err := SessionCLI(args); code == 0 || err == nil {
					t.Fatalf("removal through symlink accepted: %d %v", code, err)
				}
				if data, err := os.ReadFile(victim); err != nil || string(data) != "keep" {
					t.Fatalf("external file changed: %q %v", data, err)
				}
			})
		}
	}
}
