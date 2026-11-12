package backup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"

	"github.com/grainulation/irrigator/internal/checkpoint"
)

func backupRootPublish(r *os.Root, name string, data []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".irrigator-backup-" + hex.EncodeToString(nonce[:])
	f, err := r.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer r.Remove(tmp)
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = r.Link(tmp, name); err != nil {
		return err
	}
	if err = r.Remove(tmp); err != nil {
		return err
	}
	d, err := r.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func backupPublish(out string, data []byte) error {
	out, err := checkpoint.BackupAbsolutePath(out)
	if err != nil {
		return err
	}
	r, err := checkpoint.BackupOpenRoot(filepath.Dir(out))
	if err != nil {
		return err
	}
	defer r.Close()
	return backupRootPublish(r, filepath.Base(out), data)
}
func BackupRestore(in, out string) error {
	if err := checkpoint.RejectQuarantinedRun(out); err != nil {
		return err
	}
	m, err := BackupRead(in)
	if err != nil {
		return err
	}
	out, err = checkpoint.BackupAbsolutePath(out)
	if err != nil {
		return err
	}
	parent, err := checkpoint.BackupOpenRoot(filepath.Dir(out))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(out)
	if err = parent.Mkdir(name, 0700); err != nil {
		return err
	}
	r, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer r.Close()
	marker, _ := json.Marshal(map[string]any{"schema": "irrigator.restore-quarantine/v1", "run_id": m.RunID, "execution_authorized": false, "source_manifest_schema": m.Schema})
	if err = backupRootPublish(r, checkpoint.BackupQuarantine, marker); err != nil {
		return err
	}
	parentDir, err := parent.Open(".")
	if err != nil {
		return err
	}
	err = parentDir.Sync()
	parentDir.Close()
	if err != nil {
		return err
	}
	if m.Campaign != nil {
		data, e := json.Marshal(m.Campaign)
		if e != nil {
			return e
		}
		if e = backupRootPublish(r, "CAMPAIGN_EVIDENCE.json", data); e != nil {
			return e
		}
	}
	const stage = ".restore-staging"
	if err = r.Mkdir(stage, 0700); err != nil {
		return err
	}
	sr, err := r.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer sr.Close()
	for _, e := range m.Files {
		p := filepath.FromSlash(e.Path)
		if err = sr.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return err
		}
		f, er := sr.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if er != nil {
			return er
		}
		_, er = f.Write(e.Data)
		if er == nil {
			er = f.Sync()
		}
		ce := f.Close()
		if er != nil {
			return er
		}
		if ce != nil {
			return ce
		}
	}
	// Sync every directory bottom-up before exposing the evidence tree.
	var dirs []string
	seen := map[string]bool{".": true}
	dirs = append(dirs, ".")
	for _, e := range m.Files {
		for d := filepath.Dir(e.Path); d != "."; d = filepath.Dir(d) {
			if !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		d, er := sr.Open(dirs[i])
		if er != nil {
			return er
		}
		er = d.Sync()
		d.Close()
		if er != nil {
			return er
		}
	}
	if err = r.Rename(stage, "evidence"); err != nil {
		return err
	}
	return backupRootPublish(r, "RESTORE_COMPLETE.json", []byte("{\"schema\":\"irrigator.restore-complete/v1\",\"execution_authorized\":false}\n"))
}
