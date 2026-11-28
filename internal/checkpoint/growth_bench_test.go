package checkpoint

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

// BenchmarkJournalGrowth measures a stale-cache worker commit on a 10,000-task
// managed run whose event history has grown, the way a real run's does (about
// four events per finished task). BenchmarkScaleStore starts from an empty
// history and so cannot see this cost.
func BenchmarkJournalGrowth(b *testing.B) {
	for _, events := range []int{0, 10000, 20000, 40000} {
		b.Run(fmt.Sprintf("managed/tasks=10000/events=%d", events), func(b *testing.B) {
			root := workerPatternRun(b, "managed", 10000)
			store := FileCheckpointStore{Root: root}
			if events > 0 {
				if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
					for i := 0; i < events; i++ {
						s.Event("turn_verified", "task_00001", map[string]any{"agent_id": "task_00001", "turn_index": 1, "helpers": nil, "admitted": 0, "operation_id": "0123456789abcdef0123456789abcdef"})
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
			}
			key, err := filepath.Abs(root)
			if err != nil {
				b.Fatal(err)
			}
			commit := func(i int) {
				if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
					s.Tasks["task_00001"].Attempt = i
					s.Event("started", "task_00001", map[string]any{"i": i})
					return nil
				}); err != nil {
					b.Fatal(err)
				}
			}
			commit(1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				committedCache.Lock()
				stale := committedCache.entries[key]
				committedCache.Unlock()
				commit(1000 + 2*i)
				commit(1001 + 2*i)
				committedCache.Lock()
				committedCache.entries[key] = stale
				committedCache.Unlock()
				b.StartTimer()
				commit(100000 + i)
			}
		})
	}
}
