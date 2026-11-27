package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestJournalBackupRestoresCommittedStateAndRejectsMissingLog(t *testing.T) {
	root := runfixture.BackupFixture(t)
	if err := checkpoint.EnableJournal(root); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "uncertain"; s.Event("review", "a", nil); return nil }); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle.json")
	if err := BackupCreate(root, bundle); err != nil {
		t.Fatal(err)
	}
	m, err := BackupRead(bundle)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(runfixture.BackupTemp(t), "restored")
	if err = BackupRestore(bundle, dest); err != nil {
		t.Fatal(err)
	}
	restored := runfixture.Read(t, filepath.Join(dest, "evidence"))
	if restored.Tasks["a"].Status != "uncertain" {
		t.Fatal("journal backup lost latest committed state")
	}
	for _, entry := range m.Files {
		if filepath.Ext(entry.Path) == ".jsonl" {
			if err = os.Remove(filepath.Join(dest, "evidence", entry.Path)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = checkpoint.ReadState(filepath.Join(dest, "evidence")); err == nil {
		t.Fatal("missing committed log accepted")
	}
}
