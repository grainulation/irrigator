package checkpoint

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// TaskWatch tells a running worker when to re-read the checkpoint to see whether
// its attempt should stop: the run was taken down, the task was cancelled, its
// operation was superseded, or its row is gone. A full decode of a large run
// costs hundreds of milliseconds and a busy run is republished several times a
// second, so decoding on every publication is wasteful when almost every
// publication is some other task's progress.
//
// For a journal checkpoint the watch instead reads only the patches committed
// since it last looked, and asks for a full read only when one of them could
// matter. It errs toward reading: any unfamiliar patch, a snapshot rotation, an
// unreadable or unparseable journal tail, or a plain checkpoint all count as a
// change, and Refresh still bounds how long it goes without a full read. It is a
// hint, never a substitute for the read that decides: the caller still validates
// whatever it acts on.
type TaskWatch struct {
	root    string
	task    string
	refresh time.Duration

	plain    StateWatch // used while the checkpoint is not a journal
	seeded   bool
	snapshot string
	journal  string
	offset   int64
	read     time.Time
}

// NewTaskWatch starts watching from the current commit marker, so patches
// committed after this call (not the state at this moment) are what it reports.
// The caller has just validated the state it is watching from.
func NewTaskWatch(root, task string, refresh time.Duration, now time.Time) *TaskWatch {
	w := &TaskWatch{root: root, task: task, refresh: refresh, plain: StateWatch{Root: root, Refresh: refresh}, read: now}
	w.seed()
	return w
}

func (w *TaskWatch) head() (journalHead, bool) {
	info, err := os.Stat(filepath.Join(w.root, "state.json"))
	if err != nil || info.Size() > maxJournalMarkerBytes {
		return journalHead{}, false
	}
	raw, err := readBoundedCheckpoint(filepath.Join(w.root, "state.json"), maxJournalMarkerBytes)
	if err != nil || !isJournalCheckpoint(raw) {
		return journalHead{}, false
	}
	h, err := parseJournalHead(raw)
	return h, err == nil
}

func (w *TaskWatch) seed() {
	h, ok := w.head()
	if !ok {
		w.seeded = false
		return
	}
	w.seeded, w.snapshot, w.journal, w.offset = true, h.Storage.Snapshot, h.Storage.Journal, h.Storage.Bytes
}

// Due reports whether the caller should now do a full read of the checkpoint.
func (w *TaskWatch) Due(now time.Time) bool {
	if !w.seeded {
		if w.plain.Due(now) {
			w.seed()
			w.read = now
			return true
		}
		return false
	}
	if now.Sub(w.read) >= w.refresh {
		w.read = now
		w.seed()
		return true
	}
	h, ok := w.head()
	if !ok {
		w.seeded = false
		w.read = now
		return true
	}
	if h.Storage.Snapshot != w.snapshot || h.Storage.Journal != w.journal || h.Storage.Bytes < w.offset {
		w.read = now
		w.seed()
		return true
	}
	if h.Storage.Bytes == w.offset {
		return false
	}
	changed := w.tailMatters(h)
	w.offset = h.Storage.Bytes
	if changed {
		w.read = now
	}
	return changed
}

// tailMatters reads the committed journal bytes between the last position and
// the marker, and reports whether any patch could change what a worker watches.
func (w *TaskWatch) tailMatters(h journalHead) bool {
	f, err := os.OpenFile(filepath.Join(w.root, filepath.FromSlash(h.Storage.Journal)), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, h.Storage.Bytes-w.offset)
	if _, err = f.ReadAt(buf, w.offset); err != nil && err != io.EOF {
		return true
	}
	if len(buf) == 0 || buf[len(buf)-1] != '\n' {
		return true
	}
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var rec journalRecord
		if json.Unmarshal(line, &rec) != nil || len(rec.Transaction.Patches) == 0 {
			return true
		}
		for _, p := range rec.Transaction.Patches {
			if patchMatters(p.Path, w.task) {
				return true
			}
		}
	}
	return false
}

// patchMatters reports whether a patch path could change the run's control, this
// task's row, or its existence. Only progress that provably belongs elsewhere is
// skipped: appended or replaced events, execution accounting, and other tasks.
func patchMatters(path []string, task string) bool {
	if len(path) == 0 {
		return true
	}
	switch path[0] {
	case "events", "execution":
		return false
	case "tasks":
		return len(path) < 2 || path[1] == task
	}
	return true
}
