package runops

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func leaseTasks(root string) []model.Task {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		panic(err)
	}
	return s.AllTasks()
}

// A lease held by a process the controller did not start (an adopted worker)
// still occupies a slot and keeps its task from being dispatched again.
func TestMarkHeldLeasesSeesForeignHolders(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0), runfixture.Task("c", 0))
	tasks := leaseTasks(root)

	busy := map[string]bool{}
	if err := markHeldLeases(root, tasks, busy); err != nil || len(busy) != 0 {
		t.Fatalf("no holders: busy=%v err=%v", busy, err)
	}

	foreign, err := checkpoint.FileLock(filepath.Join(root, "cells", "b", "lease"), true)
	if err != nil {
		t.Fatal(err)
	}
	busy = map[string]bool{}
	if err = markHeldLeases(root, tasks, busy); err != nil || len(busy) != 1 || !busy["b"] {
		t.Fatalf("one foreign holder: busy=%v err=%v", busy, err)
	}
	checkpoint.Unlock(foreign)

	busy = map[string]bool{"a": true} // a child the controller already knows
	if err = markHeldLeases(root, tasks, busy); err != nil || len(busy) != 1 || !busy["a"] {
		t.Fatalf("released lease must not stay busy and known children must be kept: busy=%v err=%v", busy, err)
	}
}

// One recovery-authority check per scan must still refuse a run that recovery
// retired, as every per-lease check did.
func TestMarkHeldLeasesRefusesARetiredRun(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	tasks := leaseTasks(root)
	if err := os.WriteFile(filepath.Join(root, checkpoint.RecoverySource), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := markHeldLeases(root, tasks, map[string]bool{}); err == nil {
		t.Fatal("a retired run was scanned as if it still had authority")
	}
}

// BenchmarkLeaseScan compares the busy scan with the per-lease FileLock probe it
// replaced, over a run of 10,000 task cells.
func BenchmarkLeaseScan(b *testing.B) {
	root := b.TempDir()
	tasks := make([]model.Task, 10000)
	for i := range tasks {
		id := fmt.Sprintf("scale_%05d", i)
		tasks[i] = model.Task{ID: id}
		if err := os.MkdirAll(filepath.Join(root, "cells", id), 0700); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("probe-per-lease-filelock", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, spec := range tasks {
				lease, err := checkpoint.FileLock(filepath.Join(root, "cells", spec.ID, "lease"), true)
				if err != nil {
					b.Fatal(err)
				}
				checkpoint.Unlock(lease)
			}
		}
	})
	b.Run("markHeldLeases", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if err := markHeldLeases(root, tasks, map[string]bool{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
