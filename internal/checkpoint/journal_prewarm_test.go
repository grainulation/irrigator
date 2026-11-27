package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func cachedHead(t *testing.T, root string) (journalHead, *model.State, bool) {
	t.Helper()
	key, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	committedCache.Lock()
	defer committedCache.Unlock()
	entry, ok := committedCache.entries[key]
	return entry.head, entry.state, ok
}

func TestPrewarmFillsAColdCacheOnce(t *testing.T) {
	root := journalFixture(t)
	dropJournalCache(t, root)
	prewarmJournal(root)
	head, state, ok := cachedHead(t, root)
	if !ok {
		t.Fatal("pre-warm left the cache cold")
	}
	marker, _ := journalMarker(t, root)
	if head.Storage.Snapshot != marker.Storage.Snapshot || head.Storage.Sequence != marker.Storage.Sequence {
		t.Fatalf("pre-warm cached a different head: %+v vs %+v", head.Storage, marker.Storage)
	}
	prewarmJournal(root)
	if _, again, _ := cachedHead(t, root); again != state {
		t.Fatal("pre-warm re-read a cache that already held the current generation")
	}
}

func TestPrewarmRefreshesAfterSnapshotRotation(t *testing.T) {
	root := journalFixture(t)
	if _, err := ReadState(root); err != nil {
		t.Fatal(err)
	}
	staleHead, staleState, _ := cachedHead(t, root)
	for i := 0; i < journalTransactionLimit+5; i++ {
		if err := Update(root, func(s *model.State) error { s.Event("tick", "a", nil); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	marker, _ := journalMarker(t, root)
	if marker.Storage.Snapshot == staleHead.Storage.Snapshot {
		t.Fatal("expected the journal to have rotated to a new snapshot")
	}
	key, _ := filepath.Abs(root)
	committedCache.Lock()
	committedCache.entries[key] = journalCacheEntry{head: staleHead, state: staleState}
	committedCache.Unlock()
	prewarmJournal(root)
	if head, _, _ := cachedHead(t, root); head.Storage.Snapshot != marker.Storage.Snapshot {
		t.Fatal("pre-warm kept a cache from a rotated-away snapshot generation")
	}
}

func TestPrewarmIgnoresPlainCheckpointsAndDisabledMode(t *testing.T) {
	plain := fixture(t, task("a", 0))
	dropJournalCache(t, plain)
	prewarmJournal(plain)
	if _, _, ok := cachedHead(t, plain); ok {
		t.Fatal("pre-warm cached a plain checkpoint")
	}
	root := journalFixture(t)
	dropJournalCache(t, root)
	old := journalPrewarmEnabled
	journalPrewarmEnabled = false
	t.Cleanup(func() { journalPrewarmEnabled = old })
	prewarmJournal(root)
	if _, _, ok := cachedHead(t, root); ok {
		t.Fatal("disabled pre-warm still filled the cache")
	}
}

// A failed pre-warm must stay invisible: the locked read reports the same
// corruption error it always did.
func TestPrewarmFailureIsReportedByTheLockedRead(t *testing.T) {
	root := journalFixture(t)
	marker, _ := journalMarker(t, root)
	snapshot := filepath.Join(root, filepath.FromSlash(marker.Storage.Snapshot))
	raw, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0x01
	if err = os.WriteFile(snapshot, raw, 0600); err != nil {
		t.Fatal(err)
	}
	dropJournalCache(t, root)
	prewarmJournal(root)
	if _, _, ok := cachedHead(t, root); ok {
		t.Fatal("pre-warm cached a corrupt snapshot")
	}
	called := false
	if err = Update(root, func(*model.State) error { called = true; return nil }); err == nil || called {
		t.Fatalf("corrupt snapshot reached the callback (err=%v)", err)
	}
}
