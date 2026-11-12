package executor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestRecoveryCrashResumeAndClone(t *testing.T) {
	for _, stage := range []string{"retired", "copied", "campaign", "complete"} {
		t.Run(stage, func(t *testing.T) {
			source := runfixture.BackupFixture(t)
			if e := checkpoint.Update(source, func(s *model.State) error { s.Tasks["a"].Status = "uncertain"; return nil }); e != nil {
				t.Fatal(e)
			}
			bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
			if e := backup.BackupCreate(source, bundle); e != nil {
				t.Fatal(e)
			}
			target := filepath.Join(runfixture.BackupTemp(t), "target")
			crash := errors.New("injected interruption")
			if e := backup.RecoveryTransfer(source, bundle, target, 0, func(s string) error {
				if s == stage {
					return crash
				}
				return nil
			}); !errors.Is(e, crash) {
				t.Fatal(e)
			}
			if checkpoint.RejectRecoveryRun(source) == nil {
				t.Fatal("source not retired")
			}
			if stage != "complete" && checkpoint.RejectRecoveryRun(target) == nil && stage != "retired" {
				t.Fatal("incomplete target authorized")
			}
			if e := backup.RecoveryTransfer(source, bundle, target, 0, nil); e != nil {
				t.Fatal(e)
			}
			if e := checkpoint.RejectRecoveryRun(target); e != nil {
				t.Fatal(e)
			}
			if e := checkpoint.Update(target, func(s *model.State) error { s.Control = "pause"; return nil }); e != nil {
				t.Fatal(e)
			}
			if e := backup.RecoveryTransfer(source, bundle, target, 0, nil); e != nil {
				t.Fatal("completed transfer not idempotent after progress", e)
			}
			if runfixture.Read(t, target).Tasks["a"].Status != "uncertain" {
				t.Fatal("uncertainty lost")
			}
			clone := runfixture.BackupTemp(t)
			inventory, e := backup.BackupInventory(target)
			if e != nil {
				t.Fatal(e)
			}
			for _, entry := range inventory {
				p := filepath.Join(clone, entry.Path)
				os.MkdirAll(filepath.Dir(p), 0700)
				if e = os.WriteFile(p, entry.Data, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if checkpoint.RejectRecoveryRun(clone) == nil {
				t.Fatal("clone authorized")
			}
			if e := backup.RecoveryTransfer(source, bundle, filepath.Join(runfixture.BackupTemp(t), "second"), 0, nil); e == nil {
				t.Fatal("second identity permitted")
			}
			if e = os.Rename(source, source+"-unreachable"); e != nil {
				t.Fatal(e)
			}
			if checkpoint.RejectRecoveryRun(target) == nil {
				t.Fatal("unreachable retirement accepted")
			}
		})
	}
}
func TestRecoveryLeaseMismatchAndDestination(t *testing.T) {
	for _, kind := range []string{"controller", "cell", "changed", "existing"} {
		t.Run(kind, func(t *testing.T) {
			source := runfixture.BackupFixture(t)
			bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
			if e := backup.BackupCreate(source, bundle); e != nil {
				t.Fatal(e)
			}
			target := filepath.Join(runfixture.BackupTemp(t), "target")
			switch kind {
			case "controller", "cell":
				p := filepath.Join(source, "controller.lock")
				if kind == "cell" {
					p = filepath.Join(source, "cells", "a", "lease")
				}
				f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
				if e != nil {
					t.Fatal(e)
				}
				defer f.Close()
				if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
					t.Fatal(e)
				}
			case "changed":
				os.WriteFile(filepath.Join(source, "extra"), []byte("changed"), 0600)
			case "existing":
				os.Mkdir(target, 0700)
			}
			if e := backup.RecoveryTransfer(source, bundle, target, 0, nil); e == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if _, e := os.Stat(filepath.Join(source, checkpoint.RecoverySource)); !os.IsNotExist(e) {
				t.Fatal("failed preflight retired source", e)
			}
		})
	}
}

func TestRecoveryCampaignMigrationAndOriginalArchive(t *testing.T) {
	source, campaign := backupCampaignFixture(t)
	cells := CheckpointCells{Root: source}
	if e := checkpoint.SetControl(source, "run"); e != nil {
		t.Fatal(e)
	}
	attempt, e := cells.BeginAttempt("a", "", 123)
	if e != nil {
		t.Fatal(e)
	}
	before := runfixture.Read(t, source)
	original, e := os.ReadFile(filepath.Join(source, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if e = backup.BackupCreate(source, bundle); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(runfixture.BackupTemp(t), "target")
	crash := errors.New("after grant transfer")
	if e = backup.RecoveryTransfer(source, bundle, target, 5, func(stage string) error {
		if stage == "campaign" {
			return crash
		}
		return nil
	}); !errors.Is(e, crash) {
		t.Fatal(e)
	}
	if e = backup.RecoveryTransfer(source, bundle, target, 5, nil); e != nil {
		t.Fatal(e)
	}
	after := runfixture.Read(t, target)
	if after.Version != 5 || after.PlanHash != before.PlanHash || after.RunID != before.RunID {
		t.Fatal("migration provenance changed")
	}
	if after.Tasks["a"].Status != "uncertain" || after.Execution.Reservations[attempt.Fence.OperationID].Status != "uncertain" {
		t.Fatal("running operation replay or exposure lost")
	}
	archived, e := os.ReadFile(filepath.Join(target, "RECOVERY_ORIGINAL_STATE.json"))
	if e != nil || string(archived) != string(original) {
		t.Fatal("original archive altered", e)
	}
	c, e := checkpoint.OpenCampaign(campaign)
	if e != nil {
		t.Fatal(e)
	}
	l, e := c.Read()
	c.Close()
	if e != nil {
		t.Fatal(e)
	}
	if len(l.Grants) != 1 {
		t.Fatal("created fresh spending grant")
	}
	g := l.Grants[before.Plan.Campaign.GrantID]
	if g.RunDirectory != target || g.ReservedMicros != 250000 || g.ReportedMicros != 0 {
		t.Fatal("grant not transferred exactly")
	}
	if e = checkpoint.ValidateCampaignAuthority(target, after.Plan); e != nil {
		t.Fatal(e)
	}
	if checkpoint.ValidateCampaignAuthority(source, before.Plan) == nil {
		t.Fatal("original grant still authorized")
	}
	migratedBundle := filepath.Join(runfixture.BackupTemp(t), "migrated-bundle")
	if e = backup.BackupCreate(target, migratedBundle); e != nil {
		t.Fatal(e)
	}
	evidence := filepath.Join(runfixture.BackupTemp(t), "evidence")
	if e = backup.BackupRestore(migratedBundle, evidence); e != nil {
		t.Fatal(e)
	}
	if restored := runfixture.Read(t, filepath.Join(evidence, "evidence")); restored.Version != 5 || restored.PlanHash != before.PlanHash {
		t.Fatal("migrated backup not inspectable")
	}
	if e = checkpoint.Update(filepath.Join(evidence, "evidence"), func(s *model.State) error { return nil }); e == nil {
		t.Fatal("migrated evidence permitted mutation")
	}
}

func TestRecoveryPreservesVerifiedHistoryAndUnknownCost(t *testing.T) {
	source, _ := backupCampaignFixture(t)
	if e := checkpoint.SetControl(source, "run"); e != nil {
		t.Fatal(e)
	}
	cells := CheckpointCells{Root: source}
	a, e := cells.BeginAttempt("a", "", 123)
	if e != nil {
		t.Fatal(e)
	}
	if e = cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "verified output"}}); e != nil {
		t.Fatal(e)
	}
	if e = cells.RecordValidation(CellValidation{Fence: a.Fence, Level: "validated"}); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(source, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if e = backup.BackupCreate(source, bundle); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(runfixture.BackupTemp(t), "target")
	if e = backup.RecoveryTransfer(source, bundle, target, 0, nil); e != nil {
		t.Fatal(e)
	}
	after, e := os.ReadFile(filepath.Join(target, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	if string(before) != string(after) {
		t.Fatal("verified output/history/unknown cost checkpoint changed")
	}
	if e = checkpoint.RejectRecoveryRun(target); e != nil {
		t.Fatal(e)
	}
}

func TestRecoveryRejectsUnsupportedMigrationAndSymlinks(t *testing.T) {
	source := runfixture.BackupFixture(t)
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if e := backup.BackupCreate(source, bundle); e != nil {
		t.Fatal(e)
	}
	for _, version := range []int{-1, 4, 5, 6} {
		if e := backup.RecoveryTransfer(source, bundle, filepath.Join(runfixture.BackupTemp(t), "target"), version, nil); e == nil {
			t.Fatal("unsupported migration accepted", version)
		}
	}
	alias := filepath.Join(runfixture.BackupTemp(t), "alias")
	if e := os.Symlink(source, alias); e != nil {
		t.Fatal(e)
	}
	if e := backup.RecoveryTransfer(alias, bundle, filepath.Join(runfixture.BackupTemp(t), "target"), 0, nil); e == nil {
		t.Fatal("source symlink accepted")
	}
	target := filepath.Join(runfixture.BackupTemp(t), "target")
	if e := os.Symlink(runfixture.BackupTemp(t), target); e != nil {
		t.Fatal(e)
	}
	if e := backup.RecoveryTransfer(source, bundle, target, 0, nil); e == nil {
		t.Fatal("destination symlink accepted")
	}
}

// Subprocess termination deliberately skips deferred unlock/cleanup, matching
// controller loss rather than merely returning an error from the transfer.
func TestRecoveryHardCrashHelper(t *testing.T) {
	if os.Getenv("IRRIGATOR_TEST_RECOVERY_CRASH") != "1" {
		return
	}
	err := backup.RecoveryTransfer(os.Getenv("IRRIGATOR_TEST_SOURCE"), os.Getenv("IRRIGATOR_TEST_BUNDLE"), os.Getenv("IRRIGATOR_TEST_TARGET"), 0, func(stage string) error {
		if stage == os.Getenv("IRRIGATOR_TEST_STAGE") {
			os.Exit(73)
		}
		return nil
	})
	t.Fatal("crash stage was not reached", err)
}
func TestRecoveryHardCrashStages(t *testing.T) {
	for _, stage := range []string{"retired", "copied", "campaign", "complete"} {
		t.Run(stage, func(t *testing.T) {
			source, _ := backupCampaignFixture(t)
			bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
			if e := backup.BackupCreate(source, bundle); e != nil {
				t.Fatal(e)
			}
			target := filepath.Join(runfixture.BackupTemp(t), "target")
			cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryHardCrashHelper$")
			cmd.Env = append(os.Environ(), "IRRIGATOR_TEST_RECOVERY_CRASH=1", "IRRIGATOR_TEST_SOURCE="+source, "IRRIGATOR_TEST_BUNDLE="+bundle, "IRRIGATOR_TEST_TARGET="+target, "IRRIGATOR_TEST_STAGE="+stage)
			output, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("unexpected child result %v: %s", e, output)
			}
			if e = backup.RecoveryTransfer(source, bundle, target, 0, nil); e != nil {
				t.Fatal(e)
			}
			if e = checkpoint.RejectRecoveryRun(target); e != nil {
				t.Fatal(e)
			}
			if checkpoint.RejectRecoveryRun(source) == nil {
				t.Fatal("source authorized after crash recovery")
			}
		})
	}
}
