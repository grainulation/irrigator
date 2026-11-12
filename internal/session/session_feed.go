package session

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/textsafe"
)

func recordSessionEvent(host string, in io.Reader) error { return record(host, in) }

type sessionView = view

// attachRuns links Irrigator runs registered in this workspace to the turn
// during which they started, so swarms appear inside the conversation.
func attachRuns(ws string, v *sessionView) {
	reg, err := ReadRegistryAny(filepath.Join(ws, ".irrigator", "current-session.json"))
	if err != nil {
		return
	}
	for _, entry := range reg.Entries {
		started := entry.StartedAt.UTC().Format(time.RFC3339Nano)
		for i := len(v.Turns) - 1; i >= 0; i-- {
			end := v.Turns[i].Ended
			if v.Turns[i].At <= started && (end == "" || started <= end) {
				v.Turns[i].Runs = append(v.Turns[i].Runs, entry.RunID)
				if v.RunPaths == nil {
					v.RunPaths = map[string]string{}
				}
				v.RunPaths[entry.RunID] = entry.RunPath
				break
			}
		}
	}
}

func listSessions(ws string) ([]*sessionView, error) { return list(ws) }
func sessionEnabledAt(ws string) bool                { return enabledAt(ws) }
func removeSessionEntry(ws, name string) error       { return removeEntry(ws, name) }

func SessionCLI(args []string) (int, error) {
	action := "list"
	if len(args) > 0 && args[0] != commandflags.MetadataArgument && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("session "+action, flag.ContinueOnError)
	workspace := fs.String("workspace", ".", "workspace whose sessions to read")
	asJSON := fs.Bool("json", false, "print structured output")
	host := ""
	switch action {
	case "record":
		fs.StringVar(&host, "host", "", "hook host: claude, codex, gemini, or agy")
	case "list", "show", "enable", "disable", "delete":
	default:
		return 2, errors.New("usage: irrigator session [list|show [ID]|delete ID|enable|disable|record --host claude|codex|gemini|agy]")
	}
	if err := commandflags.ParseCommandArguments(fs, args); err != nil {
		return 2, err
	}
	if action == "record" {
		if !validHost(host) {
			return 2, errors.New("--host must be claude, codex, gemini, or agy")
		}
		// Never disturb the agent: recording problems are swallowed.
		_ = recordSessionEvent(host, os.Stdin)
		return 0, nil
	}
	ws, err := filepath.Abs(*workspace)
	if err != nil {
		return 1, err
	}
	if top, err := ChangeWorkspace(ws); err == nil {
		ws = top
	}
	switch action {
	case "enable":
		if err := enable(ws); err != nil {
			return 1, err
		}
		runtime, _ := os.Executable()
		settings, _ := json.MarshalIndent(json.RawMessage(sessionHookSettings(runtime)), "", "  ")
		fmt.Printf("Session recording enabled for %s.\nAgent prompts, one-line tool activity and replies are kept in .irrigator/sessions (git-ignored); tool output is not stored. irrigator session disable turns it off.\n\nFor Claude Code sessions, add these hooks to .claude/settings.local.json. Other hosts require a supported recorder integration:\n%s\n", ReviewTerminalText(ws), settings)
		return 0, nil
	case "disable":
		err := removeSessionEntry(ws, ".enabled")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 1, err
		}
		fmt.Println("Session recording disabled. Existing sessions remain until irrigator review prune removes them.")
		return 0, nil
	case "delete":
		// Removes Irrigator's feed for one session; the agent's own transcript is untouched.
		if fs.NArg() != 1 {
			return 2, errors.New("usage: irrigator session delete ID")
		}
		id := fs.Arg(0)
		host, sid, ok := strings.Cut(id, "-")
		if !ok || !validHost(host) || !validID(sid) {
			return 2, errors.New("invalid session id; see irrigator session list")
		}
		if err := removeSessionEntry(ws, id+".jsonl"); err != nil {
			return 1, err
		}
		fmt.Printf("Deleted session %s from Irrigator. The agent's own history is unchanged.\n", id)
		return 0, nil
	case "list":
		sessions, err := listSessions(ws)
		if err != nil {
			return 1, err
		}
		if *asJSON {
			return 0, jsonio.PrintJSON(map[string]any{"workspace": ws, "enabled": sessionEnabledAt(ws) || os.Getenv("IRRIGATOR_LAUNCH_CONTEXT") != "", "sessions": sessions})
		}
		if len(sessions) == 0 {
			fmt.Println("No recorded agent sessions here.")
			if !sessionEnabledAt(ws) {
				fmt.Println("Run irrigator session enable to record Claude Code sessions in this workspace (sessions launched through a supported host wrapper record automatically).")
			}
			return 0, nil
		}
		for _, s := range sessions {
			// View fields are raw data; each is sanitized exactly once here.
			fmt.Printf("%-8s %-58s %d turn(s) · %s\n", ReviewTerminalText(s.State), ReviewTerminalText(s.Title), len(s.Turns), ReviewTerminalText(s.Updated))
		}
		return 0, nil
	case "show":
		sessions, err := listSessions(ws)
		if err != nil {
			return 1, err
		}
		var v *sessionView
		for _, s := range sessions {
			if fs.NArg() == 0 || s.ID == fs.Arg(0) {
				v = s
				break
			}
		}
		if v == nil {
			return 1, errors.New("no such session; see irrigator session list")
		}
		attachRuns(ws, v)
		if *asJSON {
			return 0, jsonio.PrintJSON(v)
		}
		printSession(v)
		return 0, nil
	}
	return 2, errors.New("unknown session action")
}

func printSession(v *sessionView) {
	// View fields are raw data; each is sanitized exactly once at print time.
	// sessionClip is itself a sanitizing display helper, so its input is raw.
	fmt.Printf("%s · %s · %d turn(s)\n", ReviewTerminalText(v.Title), ReviewTerminalText(v.State), len(v.Turns))
	if v.Waiting != "" {
		fmt.Printf("Waiting: %s — answer in the agent's terminal.\n", ReviewTerminalText(v.Waiting))
	}
	for _, t := range v.Turns {
		if t.Prompt != "" {
			fmt.Printf("\n› %s\n", textsafe.Clip(t.Prompt, 300))
		}
		if n := len(t.Tools); n > 0 {
			failed := 0
			for _, tool := range t.Tools {
				if tool.Failed {
					failed++
				}
			}
			line := fmt.Sprintf("  %d tool call(s)", n)
			if failed > 0 {
				line += fmt.Sprintf(", %d failed", failed)
			}
			fmt.Println(line)
		}
		for _, r := range t.Runs {
			fmt.Printf("  Irrigator run %s · irrigator status <run>\n", ReviewTerminalText(r))
		}
		if t.Reply != "" {
			fmt.Printf("  %s\n", textsafe.Clip(t.Reply, 600))
		}
	}
}

// sessionHookEvents are the Claude Code events that feed the Session view.
var sessionHookEvents = []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "PostToolUseFailure", "Notification", "PermissionRequest", "Stop", "SessionEnd"}

// sessionHookSettings returns Claude Code settings that record this session.
// Hooks are async, so they never delay the agent, and they swallow failures.
func sessionHookSettings(runtime string) string {
	command := "'" + strings.ReplaceAll(runtime, "'", `'"'"'`) + "' session record --host claude 2>/dev/null || true"
	hooks := map[string]any{}
	for _, event := range sessionHookEvents {
		hooks[event] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "async": true, "timeout": 10}}}}
	}
	data, _ := json.Marshal(map[string]any{"hooks": hooks})
	return string(data)
}
