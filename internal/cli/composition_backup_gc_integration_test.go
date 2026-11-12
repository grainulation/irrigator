//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestBackupCloneCannotExecute(t *testing.T) {
	root := runfixture.CompositionRun(t)
	dir := runfixture.CompositionTempDir(t)
	bundle := filepath.Join(dir, "snapshot.json")
	clone := filepath.Join(dir, "clone")
	if e := backup.BackupCreate(root, bundle); e != nil {
		t.Fatal(e)
	}
	if e := backup.BackupRestore(bundle, clone); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(dir, "alias")
	if e := os.Symlink(filepath.Join(clone, "evidence"), alias); e != nil {
		t.Fatal(e)
	}
	for _, candidate := range []string{clone, filepath.Join(clone, "evidence"), alias} {
		if _, e := runCommand([]string{"resume", candidate}); e == nil {
			t.Fatalf("restored evidence executable at %s", candidate)
		}
		if e := checkpoint.Update(candidate, func(s *model.State) error {
			s.Plan.Execution = &model.ManagedExecutionPlan{RunBudgetUSD: "1000"}
			return nil
		}); e == nil {
			t.Fatalf("restored evidence budget mutation permitted at %s", candidate)
		}
		if e := retryNode(candidate, "a", false, true); e == nil {
			t.Fatalf("restored evidence retry permitted at %s", candidate)
		}
	}
	source := runfixture.Read(t, root)
	if source.Tasks["a"].Attempt != 0 {
		t.Fatal("backup or restore dispatched source")
	}
}
