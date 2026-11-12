package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// maxBackupBytes bounds every checkpoint or backup read. Checkpoints are
// ordinary JSON documents; anything larger is treated as suspect, never loaded.
const maxBackupBytes = 256 << 20

// errSuspectBackup is wrapped by every rejection of an existing backup file.
// The offending file is never modified, replaced or removed: it is evidence.
var errSuspectBackup = errors.New("existing checkpoint backup is not usable")

// EnsureBackup creates an idempotent, non-overwriting backup (path.bak) of the
// checkpoint at path. A pre-migration alias (state.v1.json.bak, beside path)
// satisfies the same purpose.
//
// A backup is trusted only after it is validated, never because it exists. An
// existing candidate must be a bounded regular (non-symlink) file holding one
// complete checkpoint JSON document whose run and plan identity, where both
// sides carry them, agree with the live checkpoint. The live checkpoint may be
// newer than the backup (the backup is intentionally older), so contents are not
// compared. A suspect candidate is preserved untouched and reported as an error;
// the caller must not proceed to overwrite or migrate past it.
//
// A new backup is copied completely to a temporary file, fsynced, and published
// with an atomic no-replace hard link, then the directory is fsynced. The final
// name therefore either does not exist or is a complete copy; an interrupted
// attempt leaves at most an ignorable ".state-backup-*.tmp" file. Concurrent
// callers are safe: the loser of the link race validates the winner's file.
func EnsureBackup(path string) error {
	dir := filepath.Dir(path)
	bakPath := path + ".bak"
	candidates := []string{filepath.Join(dir, "state.v1.json.bak"), bakPath}

	data, srcErr := readLiveCheckpoint(path)
	if srcErr != nil && !errors.Is(srcErr, os.ErrNotExist) {
		return srcErr
	}
	var live *checkpointIdentity
	var liveParseErr error
	if srcErr == nil {
		live, liveParseErr = parseCheckpointIdentity(data)
	}

	found := false
	for _, candidate := range candidates {
		if _, err := os.Lstat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%w: %s: %v", errSuspectBackup, candidate, err)
		}
		if err := validateBackupFile(candidate, live); err != nil {
			return fmt.Errorf("%w: %s (preserved for inspection): %v", errSuspectBackup, candidate, err)
		}
		found = true
	}
	if found {
		return nil
	}
	if errors.Is(srcErr, os.ErrNotExist) {
		return nil // nothing to back up
	}
	if liveParseErr != nil {
		return fmt.Errorf("refusing to back up a checkpoint that is not complete checkpoint JSON: %w", liveParseErr)
	}

	published, err := publishBackup(dir, bakPath, data)
	if err != nil {
		return err
	}
	if !published {
		// Another process published between our check and our link. Never trust
		// it merely because it exists.
		if err := validateBackupFile(bakPath, live); err != nil {
			return fmt.Errorf("%w: %s (preserved for inspection): %v", errSuspectBackup, bakPath, err)
		}
	}
	return nil
}

// publishBackup writes data to a synced temporary file in dir and links it to
// bakPath without replacing anything. It reports false when bakPath already
// exists. The temporary name is always removed; the link keeps the content.
func publishBackup(dir, bakPath string, data []byte) (bool, error) {
	tmp, err := os.CreateTemp(dir, ".state-backup-*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	written, err := os.ReadFile(tmpName)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(written, data) {
		return false, errors.New("backup temporary copy is incomplete")
	}
	if err := os.Link(tmpName, bakPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("publish backup without replacement: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return true, fmt.Errorf("backup published but directory sync failed: %w", err)
	}
	return true, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// checkpointIdentity is the minimal, version-tolerant view of a checkpoint.
// It deliberately knows nothing about the application's State type.
type checkpointIdentity struct {
	Version  *int            `json:"version"`
	RunID    string          `json:"run_id"`
	PlanHash string          `json:"plan_hash"`
	Plan     json.RawMessage `json:"plan"`
	Tasks    json.RawMessage `json:"tasks"`
}

// parseCheckpointIdentity requires exactly one complete JSON object shaped like
// a checkpoint of any supported version (1..5). Older shapes
// may lack run_id, schema_version, call limits and all later additions.
func parseCheckpointIdentity(data []byte) (*checkpointIdentity, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("empty checkpoint")
	}
	var id checkpointIdentity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("not a complete checkpoint JSON object: %w", err)
	}
	if id.Version == nil || *id.Version < 1 || *id.Version > 5 {
		return nil, errors.New("missing or unsupported checkpoint version")
	}
	if len(id.PlanHash) != 64 {
		return nil, errors.New("missing plan_hash")
	}
	var plan struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if len(id.Plan) == 0 || json.Unmarshal(id.Plan, &plan) != nil || len(plan.Tasks) == 0 {
		return nil, errors.New("missing plan tasks")
	}
	var tasks map[string]json.RawMessage
	if len(id.Tasks) == 0 || json.Unmarshal(id.Tasks, &tasks) != nil || len(tasks) == 0 {
		return nil, errors.New("missing task states")
	}
	return &id, nil
}

// readLiveCheckpoint reads the live checkpoint with the same bound as backups.
func readLiveCheckpoint(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("checkpoint %s is not a regular file", path)
	}
	return readBounded(f)
}

func readBounded(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBackupBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBackupBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxBackupBytes)
	}
	return data, nil
}

// validateBackupFile accepts only a bounded, non-empty, regular, non-symlink
// file containing a complete checkpoint whose identity agrees with live (when
// live is known and both sides record the field).
func validateBackupFile(path string, live *checkpointIdentity) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file (mode %s)", info.Mode().Type())
	}
	if info.Size() == 0 {
		return errors.New("empty file")
	}
	if info.Size() > maxBackupBytes {
		return fmt.Errorf("file exceeds %d bytes", maxBackupBytes)
	}
	// O_NOFOLLOW and O_NONBLOCK keep a swapped-in symlink or FIFO from being
	// followed or blocking; the post-open Stat must still be the same file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return errors.New("file changed while being validated")
	}
	data, err := readBounded(f)
	if err != nil {
		return err
	}
	id, err := parseCheckpointIdentity(data)
	if err != nil {
		return err
	}
	if live != nil {
		if live.PlanHash != "" && id.PlanHash != live.PlanHash {
			return errors.New("backup belongs to a different plan (plan_hash mismatch)")
		}
		if live.RunID != "" && id.RunID != "" && id.RunID != live.RunID {
			return errors.New("backup belongs to a different run (run_id mismatch)")
		}
	}
	return nil
}
