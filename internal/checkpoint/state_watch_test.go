package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func TestStateWatchSkipsOnlyAnUnchangedCheckpoint(t *testing.T) {
	root := fixture(t, task("worker", 0))
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	w := StateWatch{Root: root, Refresh: time.Second}
	if !w.Due(now) {
		t.Fatal("first poll must read the checkpoint")
	}
	if w.Due(now.Add(50 * time.Millisecond)) {
		t.Fatal("unchanged checkpoint was read again before refresh")
	}
	if err := Update(root, func(s *model.State) error { s.Control = "pause"; return nil }); err != nil {
		t.Fatal(err)
	}
	if !w.Due(now.Add(100 * time.Millisecond)) {
		t.Fatal("published checkpoint was not read")
	}
	if w.Due(now.Add(150 * time.Millisecond)) {
		t.Fatal("published checkpoint was read twice")
	}
	if !w.Due(now.Add(1100 * time.Millisecond)) {
		t.Fatal("refresh elapsed without a full read")
	}
	// An in-place rewrite of the same length keeps the file identity; its
	// modification time still marks it as changed.
	path := filepath.Join(root, "state.json")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if !w.Due(now.Add(1150 * time.Millisecond)) {
		t.Fatal("touched checkpoint was not read")
	}
}

func TestStateWatchReadsWhenTheCheckpointCannotBeStamped(t *testing.T) {
	root := fixture(t, task("worker", 0))
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	w := StateWatch{Root: root, Refresh: time.Hour}
	w.Due(now)
	if err := os.Rename(filepath.Join(root, "state.json"), filepath.Join(root, "state.moved")); err != nil {
		t.Fatal(err)
	}
	// The caller's read then reports the missing checkpoint; skipping it would hide that.
	for i := range 2 {
		if !w.Due(now.Add(time.Duration(i+1) * time.Millisecond)) {
			t.Fatalf("poll %d skipped a checkpoint that cannot be stamped", i)
		}
	}
	var zero stateStamp
	if zero.same(zero) {
		t.Fatal("zero stamps compared equal")
	}
}
