package textsafe

import (
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestTerminalControls(t *testing.T) {
	got := Terminal("hello\x1b[31m\tline\nnext\x00\x7f")
	want := "hello\\e[31m\tline\nnext\\0\\x7F"
	if got != want {
		t.Fatalf("Terminal() = %q, want %q", got, want)
	}
}

// decodeTerminal is the inverse of Terminal. It exists only to prove the
// escaping is reversible: every unescaped backslash must begin a known escape.
func decodeTerminal(t *testing.T, s string) string {
	t.Helper()
	hex := func(at, n int) uint64 {
		if at+n > len(s) {
			t.Fatalf("truncated escape in %q", s)
		}
		v, err := strconv.ParseUint(s[at:at+n], 16, 32)
		if err != nil || strings.ToUpper(s[at:at+n]) != s[at:at+n] {
			t.Fatalf("non-canonical hex %q in %q", s[at:at+n], s)
		}
		return v
	}
	var out []byte
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			out = append(out, s[i])
			i++
			continue
		}
		if i+1 >= len(s) {
			t.Fatalf("dangling backslash in %q", s)
		}
		switch s[i+1] {
		case '\\':
			out, i = append(out, '\\'), i+2
		case '0':
			out, i = append(out, 0), i+2
		case 'a':
			out, i = append(out, '\a'), i+2
		case 'b':
			out, i = append(out, '\b'), i+2
		case 'e':
			out, i = append(out, 0x1b), i+2
		case 'r':
			out, i = append(out, '\r'), i+2
		case 'x':
			out, i = append(out, byte(hex(i+2, 2))), i+4
		case 'u':
			out, i = utf8.AppendRune(out, rune(hex(i+2, 4))), i+6
		case 'U':
			out, i = utf8.AppendRune(out, rune(hex(i+2, 8))), i+10
		default:
			t.Fatalf("unknown escape %q in %q", s[i:i+2], s)
		}
	}
	return string(out)
}

func TestTerminalMarkerCollisionsAreDistinguishable(t *testing.T) {
	// Each pair is (text a reader could type, hostile text that is neutralized
	// to look similar). Before the escaping convention they displayed the same.
	pairs := []struct{ name, literal, hostile string }{
		{"ESC vs caret-bracket", "^[", "\x1b"},
		{"ESC vs escape name", `\e`, "\x1b"},
		{"ESC vs hex escape", `\x1B`, "\x1b"},
		{"CR vs backslash-r", `\r`, "\r"},
		{"NUL vs backslash-0", `\0`, "\x00"},
		{"BEL vs backslash-a", `\a`, "\a"},
		{"BS vs backslash-b", `\b`, "\b"},
		{"DEL vs caret-question", "^?", "\x7f"},
		{"ZWSP vs name", "<ZWSP>", "\u200b"},
		{"ZWSP vs unicode escape", `\u200B`, "\u200b"},
		{"BOM vs name", "<BOM>", "\ufeff"},
		{"RLO vs angle form", "<U+202E>", "\u202e"},
		{"RLO vs unicode escape", `\u202E`, "\u202e"},
		{"C1 CSI vs angle form", "<U+009B>", "\u009b"},
		{"invalid byte vs angle form", "<0xFF>", "\xff"},
		{"invalid byte vs hex escape", `\xFF`, "\xff"},
		{"C0 vs hex escape of C0", `\x03`, "\x03"},
		{"backslash-r pair vs CR", `\\r`, `\` + "\r"},
		{"plane-1 tag vs escape", `\U000E0001`, "\U000E0001"},
		{"LRM vs unicode escape", `\u200E`, "\u200e"},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			lit, host := Terminal(p.literal), Terminal(p.hostile)
			if lit == host {
				t.Fatalf("literal %q and hostile %q both display as %q", p.literal, p.hostile, lit)
			}
			if got := decodeTerminal(t, lit); got != p.literal {
				t.Fatalf("literal round trip = %q, want %q", got, p.literal)
			}
			if got := decodeTerminal(t, host); got != p.hostile {
				t.Fatalf("hostile round trip = %q, want %q", got, p.hostile)
			}
		})
	}
}

func TestTerminalPreservesOrdinaryReadableText(t *testing.T) {
	for _, in := range []string{
		"", "plain ascii", "tabs\tand\nnewlines\n", "a^[b <ZWSP> <0xFF> <U+202E>",
		"generic<T> and a<b && c>d", "héllo wörld 日本語 😀", "caret ^ and ^C and ^?",
		"valid replacement char \ufffd stays a real character",
	} {
		if got := Terminal(in); got != in {
			t.Errorf("Terminal(%q) = %q, want unchanged", in, got)
		}
	}
	// Backslashes are the only ordinary character that changes: they are the
	// escape introducer, so a literal one is doubled.
	if got, want := Terminal(`C:\dir\new`), `C:\\dir\\new`; got != want {
		t.Errorf("backslash path = %q, want %q", got, want)
	}
}

func TestTerminalNeutralizesAllFormatAndControlRunes(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // not encodable as UTF-8
		}
		in := string(r)
		out := Terminal(in)
		if !utf8.ValidString(out) {
			t.Fatalf("U+%04X: output %q is not valid UTF-8", r, out)
		}
		neutral := r < 0x20 && r != '\t' && r != '\n' || r >= 0x7f && r <= 0x9f ||
			r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r)
		switch {
		case neutral:
			if out == in || !strings.HasPrefix(out, `\`) || len(out) < 2 {
				t.Fatalf("U+%04X not neutralized: %q", r, out)
			}
			for i := 0; i < len(out); i++ {
				if out[i] >= 0x80 || (out[i] < 0x20 && out[i] != '\t' && out[i] != '\n') || out[i] == 0x7f {
					t.Fatalf("U+%04X: escape %q contains raw byte %#x", r, out, out[i])
				}
			}
		case r == '\\':
			if out != `\\` {
				t.Fatalf("backslash = %q", out)
			}
		default:
			if out != in {
				t.Fatalf("U+%04X unexpectedly changed to %q", r, out)
			}
		}
	}
}

func TestTerminalDirectionalMarksAndSeparators(t *testing.T) {
	in := "a\u200eb\u200fc\u061cd\u2028e\u2029f"
	want := `a\u200Eb\u200Fc\u061Cd\u2028e\u2029f`
	if got := Terminal(in); got != want {
		t.Fatalf("Terminal() = %q, want %q", got, want)
	}
}

func TestTerminalInvalidUTF8IsVisibleAndOutputValid(t *testing.T) {
	in := "ok\xff\xfe\xc2 \xed\xa0\x80 \xf0\x9f\x98 \xc0\xaf end"
	got := Terminal(in)
	if !utf8.ValidString(got) {
		t.Fatalf("output %q is invalid UTF-8", got)
	}
	want := `ok\xFF\xFE\xC2 \xED\xA0\x80 \xF0\x9F\x98 \xC0\xAF end`
	if got != want {
		t.Fatalf("Terminal() = %q, want %q", got, want)
	}
	if again := Terminal(in); again != got {
		t.Fatalf("not deterministic: %q vs %q", again, got)
	}
}

func TestTerminalDoesNotMutateInput(t *testing.T) {
	raw := []byte("secret\x1b[2J\r\n\xff")
	before := string(raw)
	_ = Terminal(string(raw))
	if string(raw) != before {
		t.Fatal("input mutated")
	}
}

func TestTerminalIsReversibleForHostileInput(t *testing.T) {
	fragments := []string{
		`\`, `\\`, `r`, `0`, `x1B`, `u200B`, "^[", "<ZWSP>", "<0xFF>", "<U+202E>", "\x1b", "\r", "\r\n",
		"\u200b", "\u200e", "\u200f", "\u061c", "\u202e", "\u2028", "\u2029", "\ufeff", "\U000E0001",
		"\u0085", "\x00", "\x7f", "\a", "\b", "\x03", "a", "Z", "é", "😀", "\n", "\t", " ",
		"\xff", "\xc2", "\xed\xa0\x80", "\xf0\x9f", "\ufffd",
	}
	rng := rand.New(rand.NewSource(20261110))
	seen := map[string]string{}
	for n := 0; n < 5000; n++ {
		var in strings.Builder
		for k := rng.Intn(10) + 1; k > 0; k-- {
			in.WriteString(fragments[rng.Intn(len(fragments))])
		}
		out := Terminal(in.String())
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 output %q for %q", out, in.String())
		}
		if got := decodeTerminal(t, out); got != in.String() {
			t.Fatalf("round trip of %q via %q = %q", in.String(), out, got)
		}
		if prior, ok := seen[out]; ok && prior != in.String() {
			t.Fatalf("collision: %q and %q both display as %q", prior, in.String(), out)
		}
		seen[out] = in.String()
	}
}
