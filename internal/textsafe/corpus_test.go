package textsafe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Expectations use the backslash convention documented on the package:
// \e ESC, \a BEL, \r CR, \xHH C0/DEL/invalid byte, \uHHHH C1/Cf/U+2028/2029.
func TestHostileCorpus(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		want     string
		unwanted []string // sequences that must NEVER appear in sanitized output
	}{
		{
			name:     "OSC 8 hyperlink injection",
			input:    "Click \x1b]8;;http://attacker.invalid\x1b\\here\x1b]8;;\x1b\\ for details",
			want:     `Click \e]8;;http://attacker.invalid\e\\here\e]8;;\e\\ for details`,
			unwanted: []string{"\x1b", "\x1b]8;;"},
		},
		{
			name:     "OSC 52 clipboard exfiltration",
			input:    "safe \x1b]52;c;cGFzc3dvcmQ=\x07 normal",
			want:     `safe \e]52;c;cGFzc3dvcmQ=\a normal`,
			unwanted: []string{"\x1b", "\x07"},
		},
		{
			name:     "CSI window resize",
			input:    "resize\x1b[8;50;100tpayload",
			want:     `resize\e[8;50;100tpayload`,
			unwanted: []string{"\x1b["},
		},
		{
			name:     "C1 8-bit CSI control sequence",
			input:    "test\u009b[8;50;100tpayload",
			want:     `test\u009B[8;50;100tpayload`,
			unwanted: []string{"\u009b"},
		},
		{
			name:     "Bidi Right-to-Left Override (RLO Trojan Source)",
			input:    "isAdmin := false; \u202E/* \u202A*/ if (isAdmin)",
			want:     `isAdmin := false; \u202E/* \u202A*/ if (isAdmin)`,
			unwanted: []string{"\u202E", "\u202A"},
		},
		{
			name:     "Bidi Isolates and Embeddings",
			input:    "\u202B\u202C\u202D\u2066\u2067\u2068\u2069",
			want:     `\u202B\u202C\u202D\u2066\u2067\u2068\u2069`,
			unwanted: []string{"\u202B", "\u202C", "\u202D", "\u2066", "\u2067", "\u2068", "\u2069"},
		},
		{
			name:     "Directional marks LRM RLM ALM",
			input:    "a\u200Eb\u200Fc\u061Cd",
			want:     `a\u200Eb\u200Fc\u061Cd`,
			unwanted: []string{"\u200E", "\u200F", "\u061C"},
		},
		{
			name:     "Line and paragraph separators",
			input:    "one\u2028two\u2029three",
			want:     `one\u2028two\u2029three`,
			unwanted: []string{"\u2028", "\u2029"},
		},
		{
			name:     "Invisible Unicode tag characters",
			input:    "a\U000E0001\U000E0041b",
			want:     `a\U000E0001\U000E0041b`,
			unwanted: []string{"\U000E0001", "\U000E0041"},
		},
		{
			name:     "Zero-width characters evasion",
			input:    "git\u200B clone \u200C--upload\u200D-pack \u2060\uFEFF",
			want:     `git\u200B clone \u200C--upload\u200D-pack \u2060\uFEFF`,
			unwanted: []string{"\u200B", "\u200C", "\u200D", "\u2060", "\uFEFF"},
		},
		{
			name:     "Invalid UTF-8 sequence",
			input:    "hello \xff\xfe world",
			want:     `hello \xFF\xFE world`,
			unwanted: []string{"\xff", "\xfe"},
		},
		{
			name:     "Truncated multi-byte UTF-8",
			input:    "incomplete \xc2",
			want:     `incomplete \xC2`,
			unwanted: []string{"\xc2"},
		},
		{
			name:     "Carriage return and C0 controls",
			input:    "line1\r\nline2\a\b\x03\x1a",
			want:     "line1\\r\nline2\\a\\b\\x03\\x1A",
			unwanted: []string{"\r", "\a", "\b", "\x03", "\x1a"},
		},
		{
			name:     "Literal markers are not forgeable",
			input:    `^[ \r <ZWSP> <0xFF> \e \x1B`,
			want:     `^[ \\r <ZWSP> <0xFF> \\e \\x1B`,
			unwanted: []string{"\x1b", "\r"},
		},
		{
			name:     "Normal tabs and newlines preserved",
			input:    "func main() {\n\tfmt.Println(\"hi\")\n}",
			want:     "func main() {\n\tfmt.Println(\"hi\")\n}",
			unwanted: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Terminal(tc.input)
			if got != tc.want {
				t.Errorf("Terminal(%q) = %q, want %q", tc.input, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Terminal(%q) = %q is not valid UTF-8", tc.input, got)
			}
			for _, bad := range tc.unwanted {
				if strings.Contains(got, bad) {
					t.Errorf("Terminal(%q) output %q contains forbidden sequence %q", tc.input, got, bad)
				}
			}
		})
	}
}
