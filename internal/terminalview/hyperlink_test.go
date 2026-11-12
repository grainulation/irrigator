package terminalview

import (
	"strings"
	"testing"
)

func TestHyperlinkEncoding(t *testing.T) {
	got := Hyperlink("https://example.com/a?b=1", "open")
	want := "\x1b]8;;https://example.com/a?b=1\x1b\\open\x1b]8;;\x1b\\"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, u := range []string{"http://localhost:8787/x", "HTTPS://example.com", "file://host/tmp/run"} {
		if got := Hyperlink(u, "t"); got == "t" {
			t.Errorf("%q should link", u)
		}
	}
}

func TestHyperlinkRejectsOtherSchemes(t *testing.T) {
	for _, u := range []string{"", "javascript:alert(1)", "ftp://x/y", "/approve/abc", "data:text/html,x", "mailto:a@b", "x-http://a"} {
		if got := Hyperlink(u, "text"); got != "text" {
			t.Errorf("Hyperlink(%q) = %q, want plain text", u, got)
		}
	}
	if got := Hyperlink("https://example.com", ""); got != "" {
		t.Errorf("empty text = %q", got)
	}
}

func TestHyperlinkStripsControls(t *testing.T) {
	got := Hyperlink("https://exa\x1b\\mple.com/\x07x\u009b", "te\x1b]8;;evil\x07xt\n\x00")
	want := "\x1b]8;;https://exa\\mple.com/x\x1b\\te]8;;evilxt\x1b]8;;\x1b\\"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Two introducers and two terminators, and nothing else.
	if n := strings.Count(got, "\x1b"); n != 4 {
		t.Errorf("escape count = %d in %q", n, got)
	}
	if strings.ContainsAny(got, "\x07\x00\n\u009b") {
		t.Errorf("control character left in %q", got)
	}
}

func TestHyperlinkFileURL(t *testing.T) {
	if got := fileURL("box", "/tmp/my run/x"); got != "file://box/tmp/my%20run/x" {
		t.Errorf("FileURL = %q", got)
	}
}
