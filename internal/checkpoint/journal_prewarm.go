package checkpoint

import (
	"os"
	"path/filepath"
)

// journalPrewarmEnabled can be switched off with IRRIGATOR_JOURNAL_PREWARM=off
// to compare lock-hold time with and without the pre-warm.
var journalPrewarmEnabled = os.Getenv("IRRIGATOR_JOURNAL_PREWARM") != "off"

// maxJournalMarkerBytes bounds a journal commit marker. A marker holds only
// file names and digests, so a larger state.json is a plain checkpoint.
const maxJournalMarkerBytes = 64 << 10

// prewarmJournal decodes the current snapshot generation into this process's
// cache before the caller takes the exclusive state lock, so a short-lived worker
// pays its cold decode in parallel with other workers instead of inside the lock.
// It is best effort and takes no lock: snapshots are immutable, the journal only
// grows within a generation, and the locked read re-verifies the head against the
// cache before using it. Any failure is ignored here and reported again, with its
// usual error, by the locked read. It does nothing when the cache already holds
// the current generation, so later commits in the same process add no reads.
func prewarmJournal(root string) {
	if !journalPrewarmEnabled {
		return
	}
	info, err := os.Stat(filepath.Join(root, "state.json"))
	if err != nil || info.Size() > maxJournalMarkerBytes {
		return
	}
	raw, err := readBoundedCheckpoint(filepath.Join(root, "state.json"), maxJournalMarkerBytes)
	if err != nil || !isJournalCheckpoint(raw) {
		return
	}
	h, err := parseJournalHead(raw)
	if err != nil {
		return
	}
	key, err := filepath.Abs(root)
	if err != nil {
		return
	}
	committedCache.Lock()
	prior, ok := committedCache.entries[key]
	current := ok && prior.head.Storage.Snapshot == h.Storage.Snapshot && prior.head.Storage.Journal == h.Storage.Journal &&
		prior.head.Storage.SnapshotSHA256 == h.Storage.SnapshotSHA256
	committedCache.Unlock()
	if current {
		return
	}
	_, _, _ = readJournalModelWithBase(root, raw, true)
}
