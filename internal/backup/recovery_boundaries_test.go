package backup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Aliases must not hide an ancestor's retirement marker.
func TestRecoveryNestedAliasCannotAcquireLease(t *testing.T) {
	source := runfixture.BackupFixture(t)
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if err := BackupCreate(source, bundle); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(runfixture.BackupTemp(t), "target")
	if err := RecoveryTransfer(source, bundle, target, 0, nil); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(runfixture.BackupTemp(t), "cell-alias")
	if err := os.Symlink(filepath.Join(source, "cells", "a"), alias); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.RejectQuarantinedRun(alias); err == nil {
		t.Error("nested symlink bypassed source retirement mutation gate")
	}
	lock, err := checkpoint.FileLock(filepath.Join(alias, "lease"), true)
	if err == nil {
		checkpoint.Unlock(lock)
		t.Error("nested symlink acquired retired source cell lease")
	}
}

func TestRecoveryTargetReplacementAndArchiveTampering(t *testing.T) {
	for _, mode := range []string{"replace_inode", "archive", "completion"} {
		t.Run(mode, func(t *testing.T) {
			source := runfixture.BackupFixture(t)
			bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
			if err := BackupCreate(source, bundle); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(runfixture.BackupTemp(t), "target")
			if err := RecoveryTransfer(source, bundle, target, 0, nil); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "replace_inode":
				inventory, err := BackupInventory(target)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(target, target+"-old"); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				for _, entry := range inventory {
					path := filepath.Join(target, entry.Path)
					if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err = os.WriteFile(path, entry.Data, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "archive":
				if err := os.WriteFile(filepath.Join(target, "RECOVERY_ORIGINAL_STATE.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "completion":
				if err := os.Remove(filepath.Join(target, checkpoint.RecoveryComplete)); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err := checkpoint.Update(target, func(s *model.State) error { called = true; s.Control = "run"; return nil })
			if err == nil || called {
				t.Fatalf("target corruption admitted mutation (%s): %v", mode, err)
			}
			if checkpoint.RejectQuarantinedRun(source) == nil {
				t.Fatal("source retirement relaxed")
			}
		})
	}
}

func TestRecoveryRetryRejectsAlteredBundleAndTarget(t *testing.T) {
	source := runfixture.BackupFixture(t)
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if err := BackupCreate(source, bundle); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(runfixture.BackupTemp(t), "target")
	interrupted := errors.New("interrupt")
	err := RecoveryTransfer(source, bundle, target, 0, func(stage string) error {
		if stage == "copied" {
			return interrupted
		}
		return nil
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(target, "state.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if RecoveryTransfer(source, bundle, target, 0, nil) == nil {
		t.Fatal("retry overwrote conflicting checkpoint")
	}
	if checkpoint.RejectQuarantinedRun(source) == nil || checkpoint.RejectQuarantinedRun(target) == nil {
		t.Fatal("incomplete conflicting transfer retained authority")
	}
}
