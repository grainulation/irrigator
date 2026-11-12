package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// backupAbsolutePath accepts macOS's root-owned system aliases without relaxing
// the no-symlink rule for user-controlled ancestors or archive payloads. Resolve
// only the first component, then all opens still pin and inspect each directory.
func BackupAbsolutePath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil || runtime.GOOS != "darwin" {
		return path, err
	}
	for _, name := range []string{"tmp", "var", "etc"} {
		alias := "/" + name
		if path != alias && !strings.HasPrefix(path, alias+"/") {
			continue
		}
		st, err := os.Lstat(alias)
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		target, err := os.Readlink(alias)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join("/", target)
		}
		expected := "/private/" + name
		if !ok || sys.Uid != 0 || filepath.Clean(target) != expected {
			return "", errors.New("backup system alias is not trusted")
		}
		return expected + strings.TrimPrefix(path, alias), nil
	}
	return path, nil
}

func BackupOpenRoot(path string) (*os.Root, error) {
	path, err := BackupAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(string(os.PathSeparator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		st, e := r.Lstat(part)
		if e != nil {
			r.Close()
			return nil, e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			r.Close()
			return nil, errors.New("backup ancestor must be a real directory")
		}
		next, e := r.OpenRoot(part)
		if e != nil {
			r.Close()
			return nil, e
		}
		after, e := next.Stat(".")
		r.Close()
		if e != nil || !os.SameFile(st, after) {
			next.Close()
			return nil, errors.New("backup ancestor changed")
		}
		r = next
	}
	return r, nil
}
