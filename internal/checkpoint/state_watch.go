package checkpoint

import (
	"os"
	"path/filepath"
	"time"
)

// stateStamp identifies one published state.json. Update publishes a new
// checkpoint by atomic rename, so every publication replaces the file identity
// and modification time; an equal stamp means the bytes ReadState would decode
// have not been replaced. A stamp is a change hint for pollers, never a
// substitute for ReadState validation.
type stateStamp struct{ info os.FileInfo }

func readStateStamp(root string) (stateStamp, error) {
	info, err := os.Stat(filepath.Join(root, "state.json"))
	if err != nil {
		return stateStamp{}, err
	}
	return stateStamp{info: info}, nil
}

// same reports whether both stamps were taken of the same, unreplaced file.
// A zero stamp (a failed stat) is never the same as any stamp.
func (a stateStamp) same(b stateStamp) bool {
	return a.info != nil && b.info != nil && os.SameFile(a.info, b.info) &&
		a.info.Size() == b.info.Size() && a.info.ModTime().Equal(b.info.ModTime())
}

// StateWatch lets a poller skip decoding a checkpoint that has not changed.
// Decoding a long-running run's state.json is the dominant cost of a poll, and
// it is the same work every time the file is unchanged. Refresh bounds how long
// a poller may go without a full read even when the stamp is unchanged, so the
// checkpoint's other inputs (recovery receipts beside state.json) and any stamp
// that cannot tell two files apart are still re-validated on a fixed cadence.
type StateWatch struct {
	Root    string
	Refresh time.Duration

	seen stateStamp
	read time.Time
}

// Due stats state.json and reports whether the caller should read it again:
// the file was replaced since the stamp Due last recorded, it cannot be
// stamped, or Refresh has passed since Due last returned true. A true result
// records the new stamp and now. Call Due before reading the checkpoint, so a
// replacement that races the read changes the stamp again and is read next time.
func (w *StateWatch) Due(now time.Time) bool {
	stamp, err := readStateStamp(w.Root)
	if err == nil && stamp.same(w.seen) && now.Sub(w.read) < w.Refresh {
		return false
	}
	w.seen, w.read = stamp, now
	return true
}
