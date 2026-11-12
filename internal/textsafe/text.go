package textsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const hexDigits = "0123456789ABCDEF"

// Terminal sanitizes untrusted text for terminal presentation, rendering control
// characters, format characters, and invalid UTF-8 sequences visibly.
func Terminal(text string) string {
	var b strings.Builder
	b.Grow(len(text))

	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && size == 1 {
			writeByteEscape(&b, text[i])
			i++
			continue
		}
		i += size

		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == '\\':
			b.WriteString(`\\`)
		case r == 0x00:
			b.WriteString(`\0`)
		case r == '\a':
			b.WriteString(`\a`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == 0x1b:
			b.WriteString(`\e`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			writeByteEscape(&b, byte(r))
		case r >= 0x80 && r <= 0x9f, neutralizedFormat(r):
			writeRuneEscape(&b, r)
		default:
			b.WriteRune(r)
		}
	}

	return b.String()
}

// neutralizedFormat reports whether r is hidden or reorders display: every
// Unicode format character (Cf: bidi marks and overrides, isolates, zero-width
// characters, word joiner, BOM, tags, soft hyphen, ...) plus the line and
// paragraph separators U+2028 and U+2029, which are not Cf but break lines.
func neutralizedFormat(r rune) bool {
	return r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r)
}

func writeByteEscape(b *strings.Builder, c byte) {
	b.WriteString(`\x`)
	b.WriteByte(hexDigits[c>>4])
	b.WriteByte(hexDigits[c&0xf])
}

func writeRuneEscape(b *strings.Builder, r rune) {
	digits := 4
	if r > 0xFFFF {
		digits = 8
		b.WriteString(`\U`)
	} else {
		b.WriteString(`\u`)
	}
	for shift := (digits - 1) * 4; shift >= 0; shift -= 4 {
		b.WriteByte(hexDigits[(r>>uint(shift))&0xf])
	}
}
