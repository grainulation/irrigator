package reviewtui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// clipboardText runs the command a key returned and reports the text it sets.
func clipboardText(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command returned")
	}
	return fmt.Sprint(cmd())
}

func TestReviewTUICopyBranch(t *testing.T) {
	m := start(t, newFake(), options{noColor: true})
	m2, cmd := press(m, "y")
	if got := clipboardText(t, cmd); got != "feat/a" {
		t.Errorf("clipboard = %q", got)
	}
	if !strings.Contains(text(m2), "copied") {
		t.Errorf("no copied status:\n%s", text(m2))
	}
	m = send(m, "j")
	_, cmd = press(m, "y")
	if got := clipboardText(t, cmd); got != "feat/b" {
		t.Errorf("clipboard after move = %q", got)
	}
}

func TestReviewTUICopyDiffCommand(t *testing.T) {
	m := start(t, newFake(), options{noColor: true})
	_, cmd := press(m, "Y")
	if got := clipboardText(t, cmd); got != "git diff main...feat/a" {
		t.Errorf("clipboard = %q", got)
	}
}

func TestReviewTUICopyQuotesAndCaps(t *testing.T) {
	if got := diffCommand("main", "x; rm -rf ~"); got != "git diff main...'x; rm -rf ~'" {
		t.Errorf("DiffCommand = %q", got)
	}
	if got := diffCommand("main", "it's"); got != "git diff main...'it'\\''s'" {
		t.Errorf("DiffCommand = %q", got)
	}
	f := newFake()
	f.items[0].Branch = strings.Repeat("b", maxCopyBytes+1)
	m := start(t, f, options{noColor: true})
	m2, cmd := press(m, "y")
	if cmd != nil {
		t.Errorf("oversized copy returned a command")
	}
	if !strings.Contains(text(m2), "not copied") {
		t.Errorf("no refusal status:\n%s", text(m2))
	}
}

func TestReviewTUICopyEmptyQueueAndPrompt(t *testing.T) {
	f := newFake()
	f.items = nil
	m := start(t, f, options{noColor: true})
	if _, cmd := press(m, "y"); cmd != nil {
		t.Error("copy with nothing selected returned a command")
	}
	m = start(t, newFake(), options{noColor: true})
	m = send(m, "r")
	m, _ = press(m, "y")
	if v := text(m); strings.Contains(v, "copied") {
		t.Errorf("y while typing a note must not copy:\n%s", v)
	}
}

func TestReviewTUIHelpListsCopyKeys(t *testing.T) {
	m := start(t, newFake(), options{noColor: true})
	if v := text(m); !strings.Contains(v, "copy branch") {
		t.Errorf("footer missing copy key:\n%s", v)
	}
	m = send(m, "?")
	if v := text(m); !strings.Contains(v, "copy diff command") {
		t.Errorf("full help missing Y:\n%s", v)
	}
}

func TestReviewTUICopyRejectsPastejacking(t *testing.T) {
	for _, bad := range []string{"feat/malicious\ncurl http://evil", "feat/malicious\r\nrm -rf /", "feat/malicious\r"} {
		f := newFake()
		f.items[0].Branch = bad
		m := start(t, f, options{noColor: true})
		m2, cmd := press(m, "y")
		if cmd != nil {
			t.Errorf("copy with newline %q returned a command", bad)
		}
		if !strings.Contains(text(m2), "contains newline") {
			t.Errorf("expected 'contains newline' status for %q, got: %s", bad, text(m2))
		}
	}
}

func TestReviewTUICopyAllowsBackslash(t *testing.T) {
	f := newFake()
	f.items[0].Branch = `feat/legit\branch`
	m := start(t, f, options{noColor: true})
	m2, cmd := press(m, "y")
	if cmd == nil {
		t.Fatal("copy with backslash was refused")
	}
	if got := clipboardText(t, cmd); got != `feat/legit\branch` {
		t.Errorf("clipboard = %q, want %q", got, `feat/legit\branch`)
	}
	if !strings.Contains(text(m2), "copied") {
		t.Errorf("expected copied status, got: %s", text(m2))
	}
}
