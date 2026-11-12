package reviewtui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type fakeBackend struct {
	items    []Item
	diffs    map[string]string
	diffErr  error
	decideFn func(kind, branch, head, note string) error
	calls    []string
}

func (f *fakeBackend) Queue() (string, []Item, error) { return "main", f.items, nil }
func (f *fakeBackend) Diff(branch, head string) (string, error) {
	return f.diffs[branch], f.diffErr
}
func (f *fakeBackend) decide(kind, branch, head, note string) error {
	f.calls = append(f.calls, kind+" "+branch+"@"+head+" "+note)
	if f.decideFn != nil {
		return f.decideFn(kind, branch, head, note)
	}
	return nil
}
func (f *fakeBackend) Approve(branch, head string) error {
	return f.decide("approve", branch, head, "")
}
func (f *fakeBackend) RequestChanges(branch, head, note string) error {
	return f.decide("request", branch, head, note)
}

const diffA = "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@\n-old\n+new\n@@ -10,2 +10,2 @@\n ctx\n-a\n+b\ndiff --git a/y.go b/y.go\n@@ -1 +1 @@\n-1\n+2\n"

func newFake() *fakeBackend {
	return &fakeBackend{
		items: []Item{
			{Branch: "feat/a", Head: "aaa", Ahead: 2, Files: 2, Status: "pending", Overlap: true},
			{Branch: "feat/b", Head: "bbb", Ahead: 1, Files: 1, Status: "approved"},
		},
		diffs: map[string]string{"feat/a": diffA, "feat/b": "diff --git a/z b/z\n@@ -1 +1 @@\n-q\n+r\n"},
	}
}

// settle runs a command and feeds resulting messages back, skipping quit.
func settle(m tea.Model, cmd tea.Cmd) tea.Model {
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			return m
		}
		if _, ok := msg.(tea.QuitMsg); ok {
			return m
		}
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				m = settle(m, c)
			}
			return m
		}
		m, cmd = m.Update(msg)
	}
	return m
}

func press(m tea.Model, s string) (tea.Model, tea.Cmd) {
	var k tea.KeyPressMsg
	switch s {
	case "enter":
		k = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		k = tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		k = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		k = tea.KeyPressMsg{Code: tea.KeyDown}
	default:
		r := []rune(s)
		k = tea.KeyPressMsg{Code: r[0], Text: s}
	}
	return m.Update(k)
}

func send(m tea.Model, keys ...string) tea.Model {
	for _, k := range keys {
		var cmd tea.Cmd
		m, cmd = press(m, k)
		m = settle(m, cmd)
	}
	return m
}

func start(t *testing.T, f *fakeBackend, opts options) tea.Model {
	t.Helper()
	var m tea.Model = newModel(f, opts)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	return settle(m, m.Init())
}

func offset(m tea.Model) int {
	mm := m.(model)
	return mm.diff.YOffset()
}

func text(m tea.Model) string { return m.View().Content }

func TestInitLoadsQueueAndFirstDiff(t *testing.T) {
	m := start(t, newFake(), options{noColor: true})
	v := text(m)
	for _, want := range []string{"feat/a", "feat/b", "pending", "approved", "* feat/a", "changed on more than one branch", "+new", "-old", "base main"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	if !m.View().AltScreen {
		t.Error("view must use the alternate screen")
	}
}

func TestMovementSelectsAndLoadsDiff(t *testing.T) {
	m := start(t, newFake(), options{noColor: true})
	m = send(m, "j")
	v := text(m)
	if !strings.Contains(v, "Diff: feat/b") || !strings.Contains(v, "+r") || strings.Contains(v, "+new") {
		t.Errorf("expected feat/b diff:\n%s", v)
	}
	m = send(m, "k")
	if !strings.Contains(text(m), "Diff: feat/a") {
		t.Error("k should return to feat/a")
	}
	m = send(m, "down")
	if !strings.Contains(text(m), "Diff: feat/b") {
		t.Error("down arrow should move")
	}
}

func TestPaneSwitchAndHunkJump(t *testing.T) {
	m := start(t, newFake(), options{noColor: true})
	mm := m.(model)
	if mm.focus != paneList {
		t.Fatal("list should start focused")
	}
	m = send(m, "tab")
	if m.(model).focus != paneDiff {
		t.Fatal("tab should focus diff")
	}
	m = send(m, "enter")
	if m.(model).focus != paneList {
		t.Fatal("enter should switch back")
	}
	// A short window makes the 15-line diff scrollable (5 visible lines).
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 8})
	// marks: 0 (file), 3 (hunk), 6 (hunk), 10 (file), 11 (hunk); offsets clamp at 9.
	for i, w := range []int{0, 3, 6, 9, 9, 9} {
		m = send(m, "n")
		if got := offset(m); got != w {
			t.Fatalf("n #%d: offset %d, want %d", i, got, w)
		}
	}
	m = send(m, "p")
	if got := offset(m); got != 9 {
		t.Fatalf("p to file y.go: offset %d, want 9 (clamped)", got)
	}
	m = send(m, "p")
	if got := offset(m); got != 6 {
		t.Fatalf("p: offset %d, want 6", got)
	}
}

func TestApprove(t *testing.T) {
	f := newFake()
	m := start(t, f, options{noColor: true})
	m = send(m, "a")
	if len(f.calls) != 1 || f.calls[0] != "approve feat/a@aaa " {
		t.Fatalf("calls = %q", f.calls)
	}
	if !strings.Contains(text(m), "approved feat/a") {
		t.Errorf("status missing:\n%s", text(m))
	}
}

func TestRequestChangesPrompt(t *testing.T) {
	f := newFake()
	m := start(t, f, options{noColor: true})
	m = send(m, "r")
	if !strings.Contains(text(m), "note>") {
		t.Fatalf("prompt not shown:\n%s", text(m))
	}
	// An empty note is refused and the prompt stays open.
	m = send(m, "enter")
	if len(f.calls) != 0 || !m.(model).prompting || !strings.Contains(text(m), "note is required") {
		t.Fatalf("empty note should be refused: calls=%q", f.calls)
	}
	// q is text inside the prompt, not quit.
	m = send(m, "f", "i", "x", " ", "q")
	m = send(m, "enter")
	if len(f.calls) != 1 || f.calls[0] != "request feat/a@aaa fix q" {
		t.Fatalf("calls = %q", f.calls)
	}
	if m.(model).prompting {
		t.Error("prompt should close")
	}
}

func TestRequestChangesEscCancels(t *testing.T) {
	f := newFake()
	m := start(t, f, options{noColor: true})
	m = send(m, "r", "x", "esc")
	if len(f.calls) != 0 || m.(model).prompting {
		t.Fatalf("esc should cancel without a decision: %q", f.calls)
	}
}

func TestDecisionErrorShown(t *testing.T) {
	f := newFake()
	f.decideFn = func(string, string, string, string) error { return errors.New("feat/a moved since it was listed") }
	m := start(t, f, options{noColor: true})
	m = send(m, "a")
	if !strings.Contains(text(m), "error: feat/a moved") {
		t.Errorf("error not surfaced:\n%s", text(m))
	}
}

func TestDiffErrorShown(t *testing.T) {
	f := newFake()
	f.diffErr = errors.New("boom")
	m := start(t, f, options{noColor: true})
	if !strings.Contains(text(m), "error: boom") {
		t.Errorf("diff error not shown:\n%s", text(m))
	}
}

func TestHelpToggleAndQuit(t *testing.T) {
	m := start(t, newFake(), options{noColor: true})
	m = send(m, "?")
	if !strings.Contains(text(m), "request changes") {
		t.Errorf("full help missing:\n%s", text(m))
	}
	_, cmd := press(m, "q")
	if cmd == nil {
		t.Fatal("q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q should produce QuitMsg")
	}
}

func TestEmptyQueue(t *testing.T) {
	f := newFake()
	f.items = nil
	m := start(t, f, options{noColor: true})
	if !strings.Contains(text(m), "No branches are ahead") {
		t.Errorf("empty state missing:\n%s", text(m))
	}
	send(m, "a", "r", "n", "j")
	if len(f.calls) != 0 {
		t.Error("no decision without a selection")
	}
}

func TestNoColorEmitsNoEscapes(t *testing.T) {
	plain := text(start(t, newFake(), options{noColor: true}))
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("NO_COLOR view contains escapes: %q", plain)
	}
	colored := text(start(t, newFake(), options{}))
	if !strings.Contains(colored, "\x1b[") {
		t.Error("color view should be styled")
	}
}
