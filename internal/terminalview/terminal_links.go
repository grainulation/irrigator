package terminalview

import (
	"os"
	"path/filepath"
	"strings"
)

// linksEnabled reports whether OSC 8 hyperlinks may be written to f: only a
// terminal, never a pipe or file, and not when IRRIGATOR_LINKS=off. Links are
// not color, so NO_COLOR does not apply.
func LinksEnabled(f *os.File) bool {
	switch strings.ToLower(os.Getenv("IRRIGATOR_LINKS")) {
	case "off", "0", "false":
		return false
	}
	if f == nil || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// pathLink returns text linked to the file:// URL of path (with this host's
// name, as OSC 8 expects), or text unchanged when links are off.
func PathLink(enabled bool, path, text string) string {
	if !enabled {
		return text
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return text
	}
	host, _ := os.Hostname()
	return Hyperlink(fileURL(host, filepath.ToSlash(abs)), text)
}
