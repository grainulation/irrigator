package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Pin each directory before traversing the next component. Comparing the
// pre-open directory identity with the opened handle closes replacement races;
// all subsequent traversal is relative to that pinned handle, not the pathname.
func OpenContextFile(name string, flags int, perm os.FileMode) (*os.File, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("context path must be clean and absolute")
	}
	volume := filepath.VolumeName(name)
	base := volume + string(filepath.Separator)
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer func() { root.Close() }()
	parts := strings.Split(strings.TrimPrefix(name, base), string(filepath.Separator))
	if len(parts) == 0 {
		return nil, errors.New("context file name is missing")
	}
	for _, part := range parts[:len(parts)-1] {
		before, err := root.Lstat(part)
		if err != nil {
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("context ancestor must be a real directory")
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return nil, errors.New("context ancestor changed during open")
		}
		root.Close()
		root = next
	}
	return root.OpenFile(parts[len(parts)-1], flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
}
