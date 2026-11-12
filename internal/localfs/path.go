package localfs

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// RelativePath accepts clean relative paths without control characters.
func RelativePath(p string) bool {
	if p == "" || filepath.IsAbs(p) || filepath.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || !utf8.ValidString(p) {
		return false
	}
	for _, c := range p {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}
