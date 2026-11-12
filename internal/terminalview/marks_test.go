package terminalview

import "testing"

func TestHyperlinkSemanticMarks(t *testing.T) {
	cases := map[string]string{
		MarkPromptStart:   "\x1b]133;A\x1b\\",
		MarkPromptEnd:     "\x1b]133;B\x1b\\",
		MarkOutputStart:   "\x1b]133;C\x1b\\",
		MarkCommandEnd(0): "\x1b]133;D;0\x1b\\",
		MarkCommandEnd(1): "\x1b]133;D;1\x1b\\",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
