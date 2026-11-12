package textsafe

import (
	"strings"
	"unicode/utf8"
)

// Clip sanitizes s for a terminal and shortens it to at most n bytes without
// splitting an escaped token, appending an ellipsis when it cut.
func Clip(s string, n int) string {
	s = strings.TrimSpace(Terminal(s))
	if len(s) <= n {
		return s
	}
	cut := 0
	for cut < len(s) {
		next := cut + clipTokenLen(s[cut:])
		if next > n {
			break
		}
		cut = next
	}
	return s[:cut] + "…"
}

func clipTokenLen(s string) int {
	if s[0] == '\\' && len(s) > 1 {
		n := 2
		switch s[1] {
		case 'x':
			n = 4
		case 'u':
			n = 6
		case 'U':
			n = 10
		}
		return min(n, len(s))
	}
	_, size := utf8.DecodeRuneInString(s)
	return size
}
