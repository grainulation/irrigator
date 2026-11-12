package terminalview

import (
	"net/url"
	"strings"
	"unicode"
)

// dropControls removes control characters (C0, C1 and DEL) outright, so a
// value cannot end the sequence or start another one.
func dropControls(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

// Hyperlink wraps text in an OSC 8 hyperlink (iTerm2, Ghostty, kitty, WezTerm,
// VTE, Windows Terminal and others). The params field is empty, so no id is
// set, and the sequence ends with ST. Control characters are removed from both
// parts. Only http, https and file URLs link; anything else returns the plain
// text, so a caller can never produce a link to javascript: or similar.
func Hyperlink(rawURL, text string) string {
	rawURL, text = dropControls(rawURL), dropControls(text)
	lower := strings.ToLower(rawURL)
	if rawURL == "" || text == "" || !(strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "file://")) {
		return text
	}
	return "\x1b]8;;" + rawURL + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// fileURL returns the file:// URL for an absolute path. OSC 8 expects the
// hostname so terminals can tell a local file from one on a remote host.
func fileURL(host, path string) string {
	return (&url.URL{Scheme: "file", Host: host, Path: path}).String()
}
