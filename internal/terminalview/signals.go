package terminalview

import (
	"fmt"
	"strings"
	"unicode"
)

// textSafe keeps printable text, including non-ASCII, and replaces control
// characters (C0, C1 and DEL), so a value cannot end or inject a sequence.
func textSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
}

// Widely supported terminal signals that sit beside OSC 7501. Terminals that
// do not understand a sequence ignore it.

// TitleOSC sets the window or tab title (OSC 2). herdr-core records it as the
// terminal's title.
func TitleOSC(title string) string {
	return "\x1b]2;" + textSafe(title) + "\x1b\\"
}

// Title stack (XTWINOPS): save the title before changing it, restore on exit.
const (
	TitlePush = "\x1b[22;0t"
	TitlePop  = "\x1b[23;0t"
)

// ProgressOSC maps a run report to the ConEmu progress bar (OSC 9;4), shown by
// Ghostty, Windows Terminal and others in the tab or taskbar. Working shows
// the percentage, blocked shows it paused, error shows it failed, and done or
// idle removes the bar.
func ProgressOSC(st ProgramStatus) string {
	p := clamp(st.Progress, 0, 100)
	switch st.State {
	case "working":
		return fmt.Sprintf("\x1b]9;4;1;%d\x1b\\", p)
	case "blocked":
		return fmt.Sprintf("\x1b]9;4;4;%d\x1b\\", p)
	case "error":
		return fmt.Sprintf("\x1b]9;4;2;%d\x1b\\", p)
	}
	return ProgressClear
}

// ProgressClear removes the progress bar.
const ProgressClear = "\x1b]9;4;0\x1b\\"

// Notification flavors. OSC 9 is the iTerm2 form (also Ghostty, kitty,
// WezTerm); OSC 777 is the rxvt/VTE form (also Ghostty, Konsole, foot).
const (
	NotifyOSC9   = "osc9"
	NotifyOSC777 = "osc777"
)

// NotifyFlavor picks the notification sequence for the environment. setting
// is IRRIGATOR_NOTIFY: "off", "osc9" or "osc777"; anything else is automatic.
// Automatic sends one flavor only, so terminals that read both do not show
// the notification twice. ok is false when notifications are off.
func NotifyFlavor(setting string, getenv func(string) string) (string, bool) {
	switch setting {
	case "off", "0", "false":
		return "", false
	case NotifyOSC9, NotifyOSC777:
		return setting, true
	}
	if getenv("VTE_VERSION") != "" || getenv("KONSOLE_VERSION") != "" || strings.HasPrefix(getenv("TERM"), "foot") {
		return NotifyOSC777, true
	}
	return NotifyOSC9, true
}

// NotifyOSC encodes a desktop notification. Text is sanitized; the OSC 777
// fields cannot contain ';'. The OSC 9 body always starts with the title, so
// it is never mistaken for an OSC 9;N subcommand such as 9;4.
func NotifyOSC(flavor, title, body string) string {
	title, body = textSafe(title), textSafe(body)
	if flavor == NotifyOSC777 {
		r := strings.NewReplacer(";", ",")
		return "\x1b]777;notify;" + r.Replace(title) + ";" + r.Replace(body) + "\x1b\\"
	}
	return "\x1b]9;" + title + ": " + body + "\x1b\\"
}

// Notice returns the notification for a run moving from prev to next, or
// false when the change needs no one's attention. A first report (prev empty)
// never notifies, so watching an already-finished run stays quiet.
func Notice(prev, next ProgramStatus) (title, body string, ok bool) {
	if prev.State == "" || prev.State == next.State {
		return "", "", false
	}
	switch next.State {
	case "done":
		return next.Title, "run complete · " + next.Message, true
	case "blocked":
		return next.Title, "needs you · " + next.Message, true
	}
	return "", "", false
}

// CellNotice returns the notification for a cell that newly needs a person.
func CellNotice(runTitle string, prevState string, cell ProgramStatus) (title, body string, ok bool) {
	if cell.State != "blocked" || prevState == "blocked" {
		return "", "", false
	}
	return runTitle, cell.ID + ": " + cell.Message, true
}

// OSC 133 semantic prompt marks (FinalTerm; kitty, WezTerm, iTerm2, Ghostty,
// VTE, Windows Terminal). A worker terminal wraps each attempt like a shell
// command so terminals can jump between attempts and select one attempt's
// output: A starts the "prompt", B ends it and the command text begins, C
// starts the output, D ends the command with its exit status.
const (
	MarkPromptStart = "\x1b]133;A\x1b\\"
	MarkPromptEnd   = "\x1b]133;B\x1b\\"
	MarkOutputStart = "\x1b]133;C\x1b\\"
)

// MarkCommandEnd ends the marked command; code 0 is success.
func MarkCommandEnd(code int) string {
	return fmt.Sprintf("\x1b]133;D;%d\x1b\\", code)
}
