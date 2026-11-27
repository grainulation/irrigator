package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

// BenchmarkScaleStore tracks journal-mode checkpoint cost against graph size
// (tasks) and uncompacted journal tail length (tail). Read is a warm read of the
// current checkpoint; CommitOneTask changes one field of one task. A one-task
// commit should not scale with the number of tasks; these numbers show how far
// it does. Run with -benchmem and a small -benchtime (for example 8x): the
// journal rotates its snapshot every 128 transactions, which would otherwise land
// inside the timed loop.
func BenchmarkScaleStore(b *testing.B) {
	for _, tasks := range []int{1000, 2000, 5000, 10000} {
		for _, tail := range []int{0, 64} {
			name := fmt.Sprintf("tasks=%d/tail=%d", tasks, tail)
			b.Run(name+"/Read", func(b *testing.B) {
				store := scaleStoreRun(b, tasks, tail)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := store.Read(context.Background()); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(name+"/CommitOneTask", func(b *testing.B) {
				store := scaleStoreRun(b, tasks, tail)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := scaleStoreCommit(store, i+1000); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func scaleStoreCommit(store checkpoint.FileCheckpointStore, attempt int) error {
	return store.Update(context.Background(), checkpoint.CheckpointUpdateOptions{}, func(s *model.State) error {
		s.Tasks["scale_00001"].Attempt = attempt
		return nil
	})
}

func scaleStoreRun(b *testing.B, tasks, tail int) checkpoint.FileCheckpointStore {
	b.Helper()
	plan, err := scaleBenchmarkFixture(scaleBenchmarkConfig{Mode: "mock", Tasks: tasks, Workers: 6, Timeout: 30 * time.Second, OutputTokens: 32})
	if err != nil {
		b.Fatal(err)
	}
	root := filepath.Join(b.TempDir(), "run")
	if err = checkpoint.Initialize(plan, root, "mock", model.Options{Workers: 6, Timeout: 30 * time.Second, Budget: .01}); err != nil {
		b.Fatal(err)
	}
	if err = checkpoint.EnableJournal(root); err != nil {
		b.Fatal(err)
	}
	store := checkpoint.FileCheckpointStore{Root: root}
	for i := 0; i < tail; i++ {
		if err = scaleStoreCommit(store, i+1); err != nil {
			b.Fatal(err)
		}
	}
	return store
}
