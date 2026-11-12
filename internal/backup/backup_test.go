package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestBackupRoundTripUncertainty(t *testing.T) {
	root := runfixture.BackupFixture(t)
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "uncertain"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "receipt.txt"), []byte("unresolved external receipt"), 0600); err != nil {
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
	if m.RunID != runfixture.Read(t, root).RunID {
		t.Fatal("identity")
	}
	dest := filepath.Join(runfixture.BackupTemp(t), "restored")
	if err = BackupRestore(bundle, dest); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, filepath.Join(dest, "evidence"))
	if s.Tasks["a"].Status != "uncertain" {
		t.Fatal("uncertainty lost")
	}
	for _, p := range []string{dest, filepath.Join(dest, "evidence"), filepath.Join(dest, "evidence", "cells", "a")} {
		if checkpoint.RejectQuarantinedRun(p) == nil {
			t.Fatal("quarantine bypass", p)
		}
	}
	alias := filepath.Join(runfixture.BackupTemp(t), "alias")
	if err = os.Symlink(filepath.Join(dest, "evidence"), alias); err != nil {
		t.Fatal(err)
	}
	if checkpoint.RejectQuarantinedRun(alias) == nil {
		t.Fatal("symlink bypass")
	}
	for _, p := range []string{dest, filepath.Join(dest, "evidence")} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0700 {
			t.Fatal("directory privacy", err)
		}
	}
	if BackupRestore(bundle, dest) == nil || BackupCreate(root, bundle) == nil {
		t.Fatal("overwrite permitted")
	}
}
func TestBackupRejectTamperAndTruncation(t *testing.T) {
	root := runfixture.BackupFixture(t)
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if err := BackupCreate(root, bundle); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(bundle)
	var m backupManifest
	json.Unmarshal(b, &m)
	cases := map[string][]byte{"truncated": b[:len(b)/2], "trailing": append(append([]byte{}, b...), []byte("{}")...), "duplicate": []byte(strings.Replace(string(b), `"schema":`, `"schema":"fake","schema":`, 1))}
	m.Files[0].Data = append(m.Files[0].Data, 'x')
	bad, _ := json.Marshal(m)
	cases["tamper"] = bad
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(runfixture.BackupTemp(t), "bad")
			os.WriteFile(p, data, 0600)
			dest := filepath.Join(runfixture.BackupTemp(t), "out")
			if BackupRestore(p, dest) == nil {
				t.Fatal("accepted corrupt bundle")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatal("created destination before validation")
			}
		})
	}
}
func TestBackupRefusesActiveAndHostileFiles(t *testing.T) {
	for _, kind := range []string{"controller", "state", "delivery", "worker", "symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := runfixture.BackupFixture(t)
			switch kind {
			case "controller", "state", "delivery":
				f, e := checkpoint.FileLock(filepath.Join(root, kind+".lock"), true)
				if e != nil {
					t.Fatal(e)
				}
				defer checkpoint.Unlock(f)
			case "worker":
				os.MkdirAll(filepath.Join(root, "cells", "a"), 0700)
				f, e := checkpoint.FileLock(filepath.Join(root, "cells", "a", "lease"), true)
				if e != nil {
					t.Fatal(e)
				}
				defer checkpoint.Unlock(f)
			case "symlink":
				os.Symlink("state.json", filepath.Join(root, "evil"))
			case "hardlink":
				os.Link(filepath.Join(root, "state.json"), filepath.Join(root, "evil"))
			case "fifo":
				if e := syscall.Mkfifo(filepath.Join(root, "fifo"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			if BackupCreate(root, filepath.Join(runfixture.BackupTemp(t), "out")) == nil {
				t.Fatal("accepted unsafe source")
			}
		})
	}
}
func TestBackupInterruptedRestoreNeverReused(t *testing.T) {
	root := runfixture.BackupFixture(t)
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if err := BackupCreate(root, bundle); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(runfixture.BackupTemp(t), "partial")
	os.Mkdir(dest, 0700)
	os.WriteFile(filepath.Join(dest, checkpoint.BackupQuarantine), []byte("incomplete"), 0600)
	os.Mkdir(filepath.Join(dest, ".restore-staging"), 0700)
	if BackupRestore(bundle, dest) == nil {
		t.Fatal("reused interrupted destination")
	}
	if checkpoint.RejectQuarantinedRun(dest) == nil {
		t.Fatal("incomplete marker did not fence")
	}
}

func TestBackupCannotMutateQuarantine(t *testing.T) {
	root := runfixture.BackupFixture(t)
	bundle := filepath.Join(runfixture.BackupTemp(t), "bundle")
	if err := BackupCreate(root, bundle); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(runfixture.BackupTemp(t), "restore")
	if err := BackupRestore(bundle, dest); err != nil {
		t.Fatal(err)
	}
	if err := BackupCreate(filepath.Join(dest, "evidence"), filepath.Join(runfixture.BackupTemp(t), "second")); err == nil {
		t.Fatal("backup created locks in restored evidence")
	}
	if err := BackupCreate(root, filepath.Join(dest, "new-bundle")); err == nil {
		t.Fatal("backup wrote into quarantine")
	}
	if err := BackupRestore(bundle, filepath.Join(dest, "nested")); err == nil {
		t.Fatal("restore wrote into quarantine")
	}
}
