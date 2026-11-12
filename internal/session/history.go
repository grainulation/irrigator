package session

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
)

const fileLimit = 8 << 20

type event struct {
	At      string `json:"at"`
	Host    string `json:"host"`
	Session string `json:"session"`
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
}

type tool struct {
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
	Failed bool   `json:"failed,omitempty"`
}

type historyTurn struct {
	Prompt  string   `json:"prompt"`
	At      string   `json:"at"`
	Tools   []tool   `json:"tools"`
	Reply   string   `json:"reply,omitempty"`
	Waiting string   `json:"waiting,omitempty"`
	Runs    []string `json:"runs"`
	Ended   string   `json:"ended,omitempty"`
}

type view struct {
	ID       string        `json:"id"`
	Host     string        `json:"host"`
	Title    string        `json:"title"`
	State    string        `json:"state"`
	Started  string        `json:"started"`
	Updated  string        `json:"updated"`
	Turns    []historyTurn `json:"turns"`
	Path     string        `json:"path"`
	Waiting  string        `json:"waiting,omitempty"`
	ToolUses int           `json:"tool_uses"`
	// SessionID is the host's own id (for claude --resume); Cwd is relative to the workspace.
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	// RunPaths maps runs started during this session to their directories.
	RunPaths map[string]string `json:"run_paths,omitempty"`
}

// parse builds the presentation history from an already securely opened feed.
func parse(r io.Reader, path string, clip func(string, int) string) (*view, error) {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	host, sid, _ := strings.Cut(base, "-")
	v := &view{ID: base, Host: host, Path: path, Turns: []historyTurn{}, State: "idle", SessionID: sid, Cwd: "."}
	sc := bufio.NewScanner(io.LimitReader(r, fileLimit+64<<10))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	var cur *historyTurn
	turn := func(at string) *historyTurn {
		if cur == nil {
			v.Turns = append(v.Turns, historyTurn{At: at, Tools: []tool{}, Runs: []string{}})
			cur = &v.Turns[len(v.Turns)-1]
		}
		return cur
	}
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind == "" {
			continue
		}
		if v.Started == "" {
			v.Started = e.At
		}
		if e.Cwd != "" {
			v.Cwd = e.Cwd
		}
		v.Updated = e.At
		switch e.Kind {
		case "start":
			v.State = "idle"
		case "prompt":
			v.Turns = append(v.Turns, historyTurn{Prompt: e.Text, At: e.At, Tools: []tool{}, Runs: []string{}})
			cur = &v.Turns[len(v.Turns)-1]
			if v.Title == "" {
				v.Title = clip(strings.SplitN(e.Text, "\n", 2)[0], 80)
			}
			v.State, v.Waiting = "working", ""
		case "tool", "tool_failed":
			t := turn(e.At)
			t.Tools = append(t.Tools, tool{Name: e.Tool, Detail: e.Detail, Failed: e.Kind == "tool_failed"})
			t.Waiting = ""
			v.ToolUses++
			v.State, v.Waiting = "working", ""
		case "waiting":
			msg := e.Text
			if msg == "" {
				msg = "Waiting for your approval" + map[bool]string{true: ": " + e.Tool, false: ""}[e.Tool != ""]
			}
			turn(e.At).Waiting = msg
			v.State, v.Waiting = "waiting", msg
		case "reply":
			t := turn(e.At)
			t.Reply, t.Waiting, t.Ended = e.Text, "", e.At
			v.State, v.Waiting = "idle", ""
			cur = nil
		case "end":
			v.State, v.Waiting = "ended", ""
		}
	}
	if v.Title == "" {
		v.Title = "New session"
	}
	return v, nil
}
