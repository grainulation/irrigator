package reviewtui

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// requireDisplaySafe fails if the rendered view could still reach a terminal
// with an active control, format, separator or invalid byte.
func requireDisplaySafe(t *testing.T, view string) {
	t.Helper()
	if !utf8.ValidString(view) {
		t.Fatalf("view is not valid UTF-8: %q", view)
	}
	for _, r := range view {
		bad := r < 0x20 && r != '\n' && r != '\t' || r >= 0x7f && r <= 0x9f ||
			r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r)
		if bad {
			t.Fatalf("unsafe rune U+%04X reached the view: %q", r, view)
		}
	}
}

// shownDiff loads one diff through the real Update/View path and returns the
// model's display lines together with the rendered view.
func shownDiff(t *testing.T, raw string) ([]string, string) {
	t.Helper()
	f := newFake()
	f.diffs["feat/a"] = raw
	m := start(t, f, options{noColor: true})
	return m.(model).diffLines, text(m)
}

func TestDiffDisplayLinesCRLF(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"true CRLF diff", "diff --git a/x b/x\r\n@@ -1 +1 @@\r\n-old\r\n+new\r\n", []string{"diff --git a/x b/x", "@@ -1 +1 @@", "-old", "+new"}},
		{"LF diff unchanged", "a\nb\n", []string{"a", "b"}},
		{"mixed endings", "a\r\nb\nc\r\n", []string{"a", "b", "c"}},
		{"trailing blank CRLF lines dropped", "a\r\n\r\n\r\n", []string{"a"}},
		{"interior blank CRLF line kept", "a\r\n\r\nb\r\n", []string{"a", "", "b"}},
		{"embedded CR stays visible", "+a\rb\r\n", []string{`+a\rb`}},
		{"double CR before LF keeps one visible", "+a\r\r\n", []string{`+a\r`}},
		{"lone final CR without LF stays visible", "+a\n+b\r", []string{"+a", `+b\r`}},
		{"CR-only line endings are not line endings", "+a\r+b\n", []string{`+a\r+b`}},
		{"literal backslash r is distinct from CR", "+a\\r\n+b\r\n", []string{`+a\\r`, "+b"}},
		{"no newline marker keeps meaning", "+x\n\\ No newline at end of file\n", []string{"+x", `\\ No newline at end of file`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := diffDisplayLines(tc.raw); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diffDisplayLines(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestRenderCRLFDiffHidesLineEndingsButKeepsMarks(t *testing.T) {
	lines, view := shownDiff(t, "diff --git a/x.go b/x.go\r\n--- a/x.go\r\n+++ b/x.go\r\n@@ -1 +1 @@\r\n-old\r\n+new\r\n@@ -9 +9 @@\r\n ctx\r\n")
	if strings.Contains(view, `\r`) {
		t.Errorf("CRLF line endings leaked into the rendered view:\n%s", view)
	}
	for _, want := range []string{"-old", "+new", "@@ -1 +1 @@", "ctx"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if len(lines) != 8 {
		t.Fatalf("display lines = %q", lines)
	}
	requireDisplaySafe(t, view)

	// File header and hunk detection must still work on CRLF text.
	f := newFake()
	f.diffs["feat/a"] = "diff --git a/x.go b/x.go\r\n@@ -1 +1 @@\r\n-a\r\n@@ -9 +9 @@\r\n+b\r\n"
	m := start(t, f, options{noColor: true})
	if got := m.(model).marks; !reflect.DeepEqual(got, []int{0, 1, 3}) {
		t.Errorf("marks = %v, want [0 1 3]", got)
	}
}

func TestRenderEmbeddedCRRemainsVisible(t *testing.T) {
	_, view := shownDiff(t, "+visible\rhidden-overwrite\r\n+tail\r\n")
	if !strings.Contains(view, `+visible\rhidden-overwrite`) {
		t.Errorf("embedded CR not shown as \\r:\n%s", view)
	}
	requireDisplaySafe(t, view)
}

func TestRenderLiteralMarkersDifferFromNeutralizedBytes(t *testing.T) {
	// The first diff types the marker text; the second contains the characters
	// those markers used to stand for. They must not render identically.
	typed := "+^[ \\r <ZWSP> <0xFF> <U+202E>\n"
	hostile := "+\x1b \r \u200b \xff \u202e\n"
	typedLines, typedView := shownDiff(t, typed)
	hostileLines, hostileView := shownDiff(t, hostile)
	if reflect.DeepEqual(typedLines, hostileLines) {
		t.Fatalf("typed markers and hostile bytes render the same: %q", typedLines)
	}
	if want := []string{`+^[ \\r <ZWSP> <0xFF> <U+202E>`}; !reflect.DeepEqual(typedLines, want) {
		t.Errorf("typed lines = %q, want %q", typedLines, want)
	}
	if want := []string{`+\e \r \u200B \xFF \u202E`}; !reflect.DeepEqual(hostileLines, want) {
		t.Errorf("hostile lines = %q, want %q", hostileLines, want)
	}
	requireDisplaySafe(t, typedView)
	requireDisplaySafe(t, hostileView)
}

func TestRenderNeutralizesBidiInvalidAndTerminalSequences(t *testing.T) {
	raw := "+a\u202eb\u200ec\u2028d\xff\n+osc \x1b]52;c;cGFzcw==\x07 end\n+c1 \u009b[2J\n"
	lines, view := shownDiff(t, raw)
	want := []string{
		`+a\u202Eb\u200Ec\u2028d\xFF`,
		`+osc \e]52;c;cGFzcw==\a end`,
		`+c1 \u009B[2J`,
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	requireDisplaySafe(t, view)
}

type baseBackend struct {
	*fakeBackend
	base string
}

func (b baseBackend) Queue() (string, []Item, error) {
	_, items, err := b.fakeBackend.Queue()
	return b.base, items, err
}

func TestRenderSanitizesBaseErrorsAndStatus(t *testing.T) {
	f := newFake()
	f.diffErr = errors.New("git: \x1b]0;x\x07 failed")
	f.decideFn = func(string, string, string, string) error { return errors.New("moved \x1b[2J") }
	m := start(t, f, options{noColor: true})
	v := text(m)
	requireDisplaySafe(t, v)
	if !strings.Contains(v, `error: git: \e]0;x\a failed`) {
		t.Errorf("diff error not neutralized visibly:\n%s", v)
	}

	f.diffErr = nil
	m = send(start(t, f, options{noColor: true}), "a")
	v = text(m)
	requireDisplaySafe(t, v)
	if !strings.Contains(v, `error: moved \e[2J`) {
		t.Errorf("status not neutralized visibly:\n%s", v)
	}

	m = newModel(baseBackend{fakeBackend: newFake(), base: "\u202em\x1b"}, options{noColor: true})
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	m = settle(m, m.Init())
	v = text(m)
	requireDisplaySafe(t, v)
	if !strings.Contains(v, "base") || !strings.Contains(v, `\u202Em\e`) {
		t.Errorf("base not neutralized visibly:\n%s", v)
	}
}

func TestRenderBranchNamesWithControlsAreNeutralizedAndCopyStaysProtected(t *testing.T) {
	f := newFake()
	f.items[0].Branch = "feat/\x1b]52;c;x\x07"
	m := start(t, f, options{noColor: true})
	v := text(m)
	requireDisplaySafe(t, v)
	if !strings.Contains(v, `feat/\e]52;c;x\a`) {
		t.Errorf("branch not neutralized in view:\n%s", v)
	}
	// Newline protection on copy is unchanged by the display changes.
	f.items[0].Branch = "feat/a\r\nrm -rf /"
	m = start(t, f, options{noColor: true})
	if _, cmd := press(m, "y"); cmd != nil {
		t.Error("branch with CRLF must not be copied")
	}
}
