package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/grainulation/irrigator/internal/model"
)

const RecoverySource = "RECOVERY_SOURCE.json"
const RecoveryTarget = "RECOVERY_TARGET.json"
const RecoveryComplete = "RECOVERY_COMPLETE.json"

type RecoveryReceipt struct {
	Schema              string `json:"schema"`
	Source              string `json:"source"`
	Target              string `json:"target"`
	RunID               string `json:"run_id"`
	BundleSHA256        string `json:"bundle_sha256"`
	OriginalStateSHA256 string `json:"original_state_sha256"`
	TargetVersion       int    `json:"target_version"`
	Device              uint64 `json:"device"`
	Inode               uint64 `json:"inode"`
}

func RecoveryDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func RecoveryRead(path string) (RecoveryReceipt, error) {
	var r RecoveryReceipt
	b, e := BackupReadFile(path, BackupFileLimit)
	if e != nil {
		return r, e
	}
	e = CampaignDecode(b, &r)
	if e == nil && (r.Schema != "irrigator.recovery/v1" || !filepath.IsAbs(r.Source) || !filepath.IsAbs(r.Target) || r.Source == r.Target || len(r.BundleSHA256) != 64) {
		e = errors.New("invalid recovery receipt")
	}
	return r, e
}

// Called at every mutation/dispatch gate, including after acquiring its lease.
// A copied checkpoint has no authority at a different filesystem identity/path.
func RejectRecoveryRun(path string) error {
	if err := rejectRecoveryPath(path); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return rejectRecoveryPath(resolved)
}
func rejectRecoveryPath(path string) error {
	p, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	for {
		if _, e = os.Lstat(filepath.Join(p, RecoverySource)); e == nil {
			return errors.New("source permanently retired by recovery")
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if _, e = os.Lstat(filepath.Join(p, RecoveryTarget)); e == nil {
			r, e := RecoveryRead(filepath.Join(p, RecoveryTarget))
			if e != nil {
				return e
			}
			if p != r.Target {
				return errors.New("recovery target path mismatch: clone has no execution authority")
			}
			root, e := BackupOpenRoot(p)
			if e != nil {
				return e
			}
			st, e := root.Stat(".")
			root.Close()
			if e != nil {
				return e
			}
			ss := st.Sys().(*syscall.Stat_t)
			if uint64(ss.Dev) != r.Device || ss.Ino != r.Inode {
				return errors.New("recovery target filesystem identity mismatch")
			}
			source, e := RecoveryRead(filepath.Join(r.Source, RecoverySource))
			if e != nil {
				return fmt.Errorf("recovery source retirement cannot be verified: %w", e)
			}
			complete, e := RecoveryRead(filepath.Join(p, RecoveryComplete))
			if e != nil {
				return errors.New("recovery transfer incomplete")
			}
			archive, e := BackupReadFile(filepath.Join(p, "RECOVERY_ORIGINAL_STATE.json"), BackupFileLimit)
			if e != nil || RecoveryDigest(archive) != r.OriginalStateSHA256 {
				return errors.New("recovery original checkpoint archive changed")
			}
			current, e := BackupReadFile(filepath.Join(p, "state.json"), BackupFileLimit)
			if e != nil {
				return e
			}
			var originalState, currentState model.State
			if json.Unmarshal(archive, &originalState) != nil || json.Unmarshal(current, &currentState) != nil || currentState.RunID != r.RunID || currentState.Version != r.TargetVersion || currentState.PlanHash != originalState.PlanHash {
				return errors.New("recovery checkpoint provenance changed")
			}
			if source != r || complete != r {
				return errors.New("recovery receipts disagree")
			}
			return nil
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		next := filepath.Dir(p)
		if next == p {
			return nil
		}
		p = next
	}
}

func validateRecoveryCheckpoint(root string, s *model.State) error {
	if s.CheckpointMigration == nil {
		return nil
	}
	if !model.IsRecoveryMigratedCheckpoint(s) {
		return errors.New("unsupported checkpoint migration descriptor")
	}
	// Quarantined backups remain inspectable evidence. Their mutation gate
	// rejects before this read-only schema validation can grant authority.
	quarantined := false
	for p := root; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(filepath.Join(p, BackupQuarantine)); err == nil {
			quarantined = true
			break
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if !quarantined {
		if err := RejectRecoveryRun(root); err != nil {
			return err
		}
	}
	r, err := RecoveryRead(filepath.Join(root, RecoveryTarget))
	if err != nil {
		return err
	}
	archive, err := BackupReadFile(filepath.Join(root, "RECOVERY_ORIGINAL_STATE.json"), BackupFileLimit)
	if err != nil || RecoveryDigest(archive) != r.OriginalStateSHA256 {
		return errors.New("migration original archive digest mismatch")
	}
	if r.OriginalStateSHA256 != s.CheckpointMigration.OriginalStateSHA256 || r.TargetVersion != s.Version {
		return errors.New("checkpoint migration receipt mismatch")
	}
	return nil
}
