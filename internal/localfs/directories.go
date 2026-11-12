package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func Abs(path string) (string, error) {
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}

// reviewRoot pins every directory and refuses ancestor symlinks before a write.
func Root(path string) (*os.Root, error) {
	path, err := Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		before, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, errors.New("review ancestor must be a real directory")
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			root.Close()
			return nil, errors.New("review directory changed during open")
		}
		root.Close()
		root = next
	}
	return root, nil
}
func PrivateDir(workspace, name string) (string, error) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	defer func() { root.Close() }()
	for _, part := range []string{".irrigator", name} {
		if err := root.Mkdir(part, 0700); err != nil && !os.IsExist(err) {
			return "", err
		}
		before, err := root.Lstat(part)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("review directory must be a real directory")
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			return "", err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			return "", errors.New("review directory changed during open")
		}
		root.Close()
		root = next
	}
	return filepath.Join(workspace, ".irrigator", name), nil
}
