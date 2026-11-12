package session

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/textsafe"
)

func pathInside(root, name string) bool {
	rel, err := filepath.Rel(root, name)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func validHost(host string) bool {
	return host == "claude" || host == "codex" || host == "gemini" || host == "agy"
}

func validID(id string) bool {
	if !sessionIDPattern.MatchString(id) || strings.HasPrefix(id, "-") || strings.EqualFold(id, "latest") {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return true
		}
	}
	return false
}
func readFile(path string) (*view, error) {
	f, err := localfs.OpenContextFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Durable and parsed data stay raw; presentation sanitizes at print time.
	return parse(f, path, rawClip)
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// rawClip bounds durable data. When longer than n bytes, it cuts at a UTF-8
// rune start and appends "…" (so the result may exceed
// n by the 3 ellipsis bytes). It never sanitizes: the bytes are the data.
func rawClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// clip is the DISPLAY helper: it sanitizes s for the terminal exactly once and
// bounds the sanitized text to n bytes without cutting inside an escape marker
// or a rune. Never feed its result back into storage or into another sanitizer.
func clip(s string, n int) string {
	s = strings.TrimSpace(textsafe.Terminal(s))
	if len(s) <= n {
		return s
	}
	cut := 0
	for cut < len(s) {
		next := cut + displayTokenLen(s[cut:])
		if next > n {
			break
		}
		cut = next
	}
	return s[:cut] + "…"
}

// displayTokenLen returns the length of the first textsafe token of sanitized
// text: a whole backslash escape marker or one UTF-8 rune. Sanitized text
// doubles every literal backslash, so markers parse unambiguously.
func displayTokenLen(s string) int {
	if s[0] == '\\' && len(s) > 1 {
		n := 2
		switch s[1] {
		case 'x':
			n = 4
		case 'u':
			n = 6
		case 'U':
			n = 10
		}
		return min(n, len(s))
	}
	_, size := utf8.DecodeRuneInString(s)
	return size
}
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// workspace finds where to record: the app's launch workspace when the
// hook runs inside it, otherwise the nearest ancestor that opted in with
// `irrigator session enable`. Anywhere else, recording is off.
func workspace(cwd string) string {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	cwd = filepath.Clean(cwd)
	if ws := os.Getenv("IRRIGATOR_WORKSPACE"); ws != "" && os.Getenv("IRRIGATOR_LAUNCH_CONTEXT") != "" && pathInside(filepath.Clean(ws), cwd) {
		return filepath.Clean(ws)
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if info, err := os.Lstat(filepath.Join(dir, ".irrigator", "sessions", ".enabled")); err == nil && info.Mode().IsRegular() {
			return dir
		}
		if dir == filepath.Dir(dir) {
			return ""
		}
	}
}

// toolSummary turns a tool call into one human line without storing its output.
func toolSummary(ws, tool string, input map[string]any) string {
	str := func(k string) string { v, _ := input[k].(string); return v }
	rel := func(p string) string {
		if r, err := filepath.Rel(ws, p); err == nil && !strings.HasPrefix(r, "..") {
			return r
		}
		return p
	}
	switch tool {
	case "Bash", "shell", "exec_command", "local_shell":
		cmd := str("command")
		if cmd == "" {
			if parts, ok := input["command"].([]any); ok {
				words := []string{}
				for _, p := range parts {
					if s, ok := p.(string); ok {
						words = append(words, s)
					}
				}
				cmd = strings.Join(words, " ")
			}
		}
		first, _, _ := strings.Cut(cmd, "\n")
		return rawClip(first, 160)
	case "Edit", "Write", "MultiEdit", "Read", "NotebookEdit", "apply_patch":
		if p := str("file_path"); p != "" {
			return rawClip(rel(p), 160)
		}
		return ""
	case "Grep", "Glob":
		return rawClip(str("pattern"), 120)
	case "WebFetch":
		if u, err := url.Parse(str("url")); err == nil {
			return rawClip(u.Host, 120)
		}
	case "WebSearch":
		return rawClip(str("query"), 120)
	case "Task", "Agent":
		return rawClip(str("description"), 120)
	}
	return ""
}

func toolName(tool string) string {
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		server, name, _ := strings.Cut(rest, "__")
		return rawClip(server+": "+name, 80)
	}
	return rawClip(tool, 80)
}

// relCwd records where a session ran, relative to the workspace, so it
// can be resumed from the same directory.
func relCwd(ws, cwd string) string {
	if rel, err := filepath.Rel(ws, filepath.Clean(cwd)); err == nil && !strings.HasPrefix(rel, "..") && localfs.RelativePath(rel) || rel == "." {
		return rel
	}
	return "."
}

// normalizeEvent maps one hook payload to a feed event, or nil to skip it.
func normalizeEvent(host, ws string, p map[string]any) *event {
	str := func(k string) string { v, _ := p[k].(string); return v }
	e := &event{At: time.Now().UTC().Format(time.RFC3339Nano), Host: host, Session: str("session_id")}
	if !validID(e.Session) {
		return nil
	}
	input, _ := p["tool_input"].(map[string]any)
	switch str("hook_event_name") {
	case "SessionStart":
		e.Kind, e.Detail, e.Cwd = "start", rawClip(str("source"), 32), relCwd(ws, str("cwd"))
	case "UserPromptSubmit":
		if str("prompt_type") == "auto" {
			return nil
		}
		e.Kind, e.Text, e.Cwd = "prompt", rawClip(str("prompt"), 4096), relCwd(ws, str("cwd"))
	case "PostToolUse":
		e.Kind, e.Tool, e.Detail = "tool", toolName(str("tool_name")), toolSummary(ws, str("tool_name"), input)
	case "PostToolUseFailure":
		e.Kind, e.Tool, e.Detail = "tool_failed", toolName(str("tool_name")), toolSummary(ws, str("tool_name"), input)
	case "Notification":
		switch str("notification_type") {
		case "permission_prompt", "elicitation_dialog", "agent_needs_input":
			e.Kind, e.Text, e.Detail = "waiting", rawClip(str("message"), 512), str("notification_type")
		default:
			return nil
		}
	case "PermissionRequest":
		e.Kind, e.Tool, e.Detail = "waiting", toolName(str("tool_name")), "permission_prompt"
	case "Stop":
		e.Kind, e.Text = "reply", rawClip(str("last_assistant_message"), 8192)
	case "SessionEnd":
		e.Kind, e.Detail = "end", rawClip(str("reason"), 32)
	default:
		return nil
	}
	return e
}

// record appends one event. Hooks run asynchronously; failures are
// silent so a recording problem never surfaces inside the agent session.
func record(host string, in io.Reader) error {
	if !validHost(host) {
		return errors.New("invalid session host")
	}
	data, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return err
	}
	var payload map[string]any
	if err = json.Unmarshal(data, &payload); err != nil {
		return err
	}
	cwd, _ := payload["cwd"].(string)
	ws := workspace(cwd)
	if ws == "" {
		return nil
	}
	e := normalizeEvent(host, ws, payload)
	if e == nil {
		return nil
	}
	dir, err := localfs.PrivateDir(ws, "sessions")
	if err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := localfs.OpenContextFile(filepath.Join(dir, host+"-"+e.Session+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func(fd int) { _ = syscall.Flock(fd, syscall.LOCK_UN) }(int(f.Fd()))
	st, serr := f.Stat()
	if serr != nil {
		return serr
	}
	if st.Size() > fileLimit {
		return nil // cap reached: further events are dropped silently by design
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

func list(ws string) ([]*view, error) {
	dir := filepath.Join(ws, ".irrigator", "sessions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []*view{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []*view{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if v, err := readFile(filepath.Join(dir, e.Name())); err == nil {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}

// enable creates the workspace opt-in marker through confined file operations.
func enable(ws string) error {
	dir, err := localfs.PrivateDir(ws, "sessions")
	if err != nil {
		return err
	}
	f, err := localfs.OpenContextFile(filepath.Join(dir, ".enabled"), os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	return f.Close()
}

func enabledAt(ws string) bool {
	info, err := os.Lstat(filepath.Join(ws, ".irrigator", "sessions", ".enabled"))
	return err == nil && info.Mode().IsRegular()
}

// Pin the feed directory before removing an entry. Pathname removal would
// follow a replaced .irrigator or sessions directory outside the workspace.
func removeEntry(ws, name string) error {
	root, err := localfs.Root(filepath.Join(ws, ".irrigator", "sessions"))
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(name)
}
