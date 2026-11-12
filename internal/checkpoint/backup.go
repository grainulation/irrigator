package checkpoint

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/grainulation/irrigator/internal/launch"
)

const BackupFileLimit = 16 << 20
const BackupQuarantine = "RESTORE_QUARANTINE.json"

func BackupReadFile(path string, limit int64) ([]byte, error) {
	path, err := BackupAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	f, err := launch.OpenContextFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || !ok || sys.Nlink != 1 || st.Size() > limit {
		return nil, errors.New("backup requires bounded regular single-link files")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("backup file exceeds limit")
	}
	return b, err
}

// Quarantine is an execution boundary, not a new run identity. Read-only tools
// may inspect the preserved checkpoint; all mutation/admission paths call this.
func RejectQuarantinedRun(root string) error {
	if err := RejectRecoveryRun(root); err != nil {
		return err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	paths := []string{root}
	resolved, e := filepath.EvalSymlinks(root)
	if e == nil && resolved != root {
		paths = append(paths, resolved)
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	for _, p := range paths {
		for {
			if _, e := os.Lstat(filepath.Join(p, BackupQuarantine)); e == nil {
				return errors.New("restored evidence is quarantined: execution and mutation are forbidden")
			} else if !errors.Is(e, os.ErrNotExist) {
				return e
			}
			parent := filepath.Dir(p)
			if parent == p {
				break
			}
			p = parent
		}
	}
	return nil
}
