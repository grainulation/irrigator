package dashboard

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestStatusWatchControllerProbeDoesNotCreateLease(t *testing.T) {
	root := t.TempDir()
	if err := StatusWatchController(root); err == nil {
		t.Fatal("missing controller reported active")
	}
	if _, err := os.Lstat(filepath.Join(root, "controller.lock")); !os.IsNotExist(err) {
		t.Fatal("read-only probe created lease")
	}
	held, err := checkpoint.FileLock(filepath.Join(root, "controller.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoint.Unlock(held)
	if err := StatusWatchController(root); err != nil {
		t.Fatal(err)
	}
}

func TestBackupMacOSSystemAliasRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS root aliases")
	}
	for _, base := range []string{"/tmp", os.TempDir()} {
		t.Run(filepath.Base(base), func(t *testing.T) {
			dir, err := os.MkdirTemp(base, "irrigator-backup-alias-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			root := filepath.Join(dir, "run")
			if err := checkpoint.Initialize(model.Plan{Name: "alias", Tasks: []model.Task{runfixture.Task("a", 0)}}, root, "mock"); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "snapshot.json")
			if err := backup.BackupCreate(root, out); err != nil {
				t.Fatal(err)
			}
			if _, err := backup.BackupRead(out); err != nil {
				t.Fatal(err)
			}
			if err := backup.BackupRestore(out, filepath.Join(dir, "restored")); err != nil {
				t.Fatal(err)
			}
			canonical, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := backup.BackupCreate(root, filepath.Join(canonical, "nested.json")); err == nil {
				t.Fatal("alias hid output inside source")
			}
		})
	}
}

func TestBackupRejectsUserSymlinkAncestors(t *testing.T) {
	dir := runfixture.BackupTemp(t)
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	root := runfixture.BackupFixture(t)
	if err := backup.BackupCreate(root, filepath.Join(alias, "snapshot.json")); err == nil {
		t.Fatal("untrusted output ancestor accepted")
	}
	if _, err := os.Lstat(filepath.Join(real, "snapshot.json")); !os.IsNotExist(err) {
		t.Fatal("wrote through untrusted alias")
	}
	if err := os.Symlink(root, filepath.Join(real, "source")); err != nil {
		t.Fatal(err)
	}
	if err := backup.BackupCreate(filepath.Join(real, "source"), filepath.Join(dir, "snapshot.json")); err == nil {
		t.Fatal("source symlink accepted")
	}
}
