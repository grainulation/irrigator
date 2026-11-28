package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func commit(t *testing.T, root string, change func(*model.State)) {
	t.Helper()
	if err := Update(root, func(s *model.State) error { change(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestTaskWatchSkipsOtherTasksProgressAndWakesForWhatMatters(t *testing.T) {
	root := journalFixture(t)
	now := time.Now()
	w := NewTaskWatch(root, "a", time.Hour, now)
	if w.Due(now) {
		t.Fatal("a fresh watch asked for a read with nothing committed")
	}

	// Other tasks' progress, events and accounting are not this task's business.
	commit(t, root, func(s *model.State) { s.Tasks["b"].Attempt = 3; s.Tasks["b"].Status = "running" })
	commit(t, root, func(s *model.State) { s.Tasks["b"].Notes = map[string]string{"k": "v"} })
	commit(t, root, func(s *model.State) { s.Event("tick", "b", map[string]any{"n": 1}) })
	if w.Due(now) {
		t.Fatal("progress on another task woke the watch")
	}
	if w.Due(now) {
		t.Fatal("the watch kept reporting a change it had already seen")
	}

	wakes := []struct {
		name   string
		change func(*model.State)
	}{
		{"own row edited", func(s *model.State) { s.Tasks["a"].Error = "boom" }},
		{"own cancel requested", func(s *model.State) { s.Tasks["a"].CancelRequested = true }},
		{"own operation superseded", func(s *model.State) { s.Tasks["a"].OperationID = "op-new" }},
		{"run taken down", func(s *model.State) { s.Control = "down" }},
		{"unfamiliar top-level field", func(s *model.State) { s.Backend = "other" }},
	}
	for _, wake := range wakes {
		commit(t, root, wake.change)
		if !w.Due(now) {
			t.Fatalf("%s did not wake the watch", wake.name)
		}
		if w.Due(now) {
			t.Fatalf("%s kept waking the watch after it was seen", wake.name)
		}
	}
}

func TestTaskWatchWakesWhenItsTaskRowIsRemoved(t *testing.T) {
	root := journalFixture(t)
	now := time.Now()
	w := NewTaskWatch(root, "a", time.Hour, now)
	commit(t, root, func(s *model.State) { delete(s.Tasks, "a") })
	if !w.Due(now) {
		t.Fatal("removing the watched task's row did not wake the watch")
	}
}

func TestTaskWatchRefreshBoundsTheTimeWithoutAFullRead(t *testing.T) {
	root := journalFixture(t)
	now := time.Now()
	w := NewTaskWatch(root, "a", time.Second, now)
	if w.Due(now.Add(500 * time.Millisecond)) {
		t.Fatal("woke before the refresh with nothing committed")
	}
	if !w.Due(now.Add(2 * time.Second)) {
		t.Fatal("did not ask for a full read once the refresh elapsed")
	}
	if w.Due(now.Add(2*time.Second + 100*time.Millisecond)) {
		t.Fatal("asked again straight after the refresh read")
	}
}

func TestTaskWatchWakesAcrossSnapshotRotation(t *testing.T) {
	root := journalFixture(t)
	now := time.Now()
	w := NewTaskWatch(root, "a", time.Hour, now)
	before, _ := journalMarker(t, root)
	for i := 0; i < journalTransactionLimit+5; i++ {
		commit(t, root, func(s *model.State) { s.Event("tick", "b", nil) })
	}
	after, _ := journalMarker(t, root)
	if after.Storage.Snapshot == before.Storage.Snapshot {
		t.Fatal("expected a snapshot rotation")
	}
	if !w.Due(now) {
		t.Fatal("a rotated journal did not wake the watch")
	}
	if w.Due(now) {
		t.Fatal("the watch kept waking after it re-seeded on the new generation")
	}
}

func TestTaskWatchTreatsAnUnreadableTailAsAChange(t *testing.T) {
	root := journalFixture(t)
	now := time.Now()
	w := NewTaskWatch(root, "a", time.Hour, now)
	start, _ := journalMarker(t, root)
	commit(t, root, func(s *model.State) { s.Tasks["b"].Attempt = 9 })
	end, _ := journalMarker(t, root)
	path := filepath.Join(root, filepath.FromSlash(end.Storage.Journal))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[start.Storage.Bytes] = 'x' // corrupt the first byte of the new line
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if !w.Due(now) {
		t.Fatal("an unparseable tail was treated as nothing to read")
	}
}

func TestTaskWatchOnAPlainCheckpointFallsBackToStateStamps(t *testing.T) {
	root := fixture(t, task("a", 0))
	now := time.Now()
	w := NewTaskWatch(root, "a", time.Hour, now)
	if !w.Due(now) {
		t.Fatal("a plain checkpoint must ask for its first read")
	}
	if w.Due(now) {
		t.Fatal("an unchanged plain checkpoint asked for another read")
	}
	commit(t, root, func(s *model.State) { s.Control = "pause" })
	if !w.Due(now) {
		t.Fatal("a republished plain checkpoint did not wake the watch")
	}
}
