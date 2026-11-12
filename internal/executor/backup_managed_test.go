package executor

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestBackupRetainsManagedUnknownCost(t *testing.T) {
	root, err := filepath.EvalSymlinks(runfixture.UnknownCostFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	c := CheckpointCells{Root: root}
	a, err := c.BeginAttempt("a", "", 123)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.RecordOutcome(managed.CellOutcome{Fence: a.Fence, CostUnknown: true, Result: map[string]string{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
	if err = c.RecordValidation(CellValidation{Fence: a.Fence, Level: "validated"}); err != nil {
		t.Fatal(err)
	}
	before := runfixture.Read(t, root)
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if err = backup.BackupCreate(root, bundle); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(runfixture.BackupTemp(t), "restore")
	if err = backup.BackupRestore(bundle, dest); err != nil {
		t.Fatal(err)
	}
	after := runfixture.Read(t, filepath.Join(dest, "evidence"))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("restore changed state, costs or receipts")
	}
	if !after.Tasks["a"].CostUnknown {
		t.Fatal("unknown cost lost")
	}
	used, err := checkpoint.ManagedBudgetUsed(after.Execution)
	if err != nil || used != 250000 {
		t.Fatal("budget exposure lost", used, err)
	}
	// Offline restore remains possible after the original directory is renamed.
	moved := root + "-moved"
	if err = os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err = backup.BackupRestore(bundle, filepath.Join(runfixture.BackupTemp(t), "offline")); err != nil {
		t.Fatal(err)
	}
}
