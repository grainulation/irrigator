package checkpoint

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

// A reader must not keep the shared state lock while it decodes: a writer that
// wants the exclusive lock would otherwise wait out every decode.
func TestJournalReadReleasesSharedLockBeforeDecoding(t *testing.T) {
	root := journalFixture(t)
	dropJournalCache(t, root)
	var heldDuringDecode error
	ran := false
	journalReadHook = func() {
		ran = true
		lock, err := FileLock(filepath.Join(root, "state.lock"), true)
		if err != nil {
			heldDuringDecode = err
			return
		}
		Unlock(lock)
	}
	t.Cleanup(func() { journalReadHook = nil })
	if _, err := ReadState(root); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("the read did not reach the decode step")
	}
	if heldDuringDecode != nil {
		t.Fatalf("exclusive lock unavailable while a reader decoded: %v", heldDuringDecode)
	}
}

// Readers that decode after releasing the lock must still see only committed,
// consistent states, including across snapshot rotation (which deletes the
// previous generation's files).
func TestJournalReadersStayConsistentAcrossRotationWhileCommitting(t *testing.T) {
	root := journalFixture(t)
	const commits = journalTransactionLimit + 30
	done := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			lastAttempt, lastEvents := -1, -1
			for {
				select {
				case <-done:
					return
				default:
				}
				s, err := ReadState(root)
				if err != nil {
					errs <- fmt.Errorf("reader %d: %w", r, err)
					return
				}
				attempt, events := s.Tasks["a"].Attempt, len(s.Events)
				if attempt < lastAttempt || events < lastEvents {
					errs <- fmt.Errorf("reader %d went backwards: attempt %d after %d, events %d after %d", r, attempt, lastAttempt, events, lastEvents)
					return
				}
				lastAttempt, lastEvents = attempt, events
			}
		}(r)
	}
	for i := 1; i <= commits; i++ {
		i := i
		if err := Update(root, func(s *model.State) error {
			s.Tasks["a"].Attempt = i
			s.Event("tick", "a", map[string]any{"i": i})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	final, err := ReadState(root)
	if err != nil || final.Tasks["a"].Attempt != commits {
		t.Fatalf("final state wrong: %v", err)
	}
}
