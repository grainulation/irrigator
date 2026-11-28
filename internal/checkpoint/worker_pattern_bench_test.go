package checkpoint

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

// BenchmarkJournalWorkerPattern measures a one-task commit the way the real
// workload sees it. Workers are short-lived processes that make three commits
// per task while other workers commit in between, so their in-process cache is
// empty (cold) at the first commit and behind the journal head (stale) at the
// later ones. A same-process warm sequence, as in BenchmarkScaleStore, is the
// best case and does not occur under load.
func BenchmarkJournalWorkerPattern(b *testing.B) {
	for _, backend := range []string{"mock", "managed"} {
		for _, tasks := range []int{1000, 2000, 5000, 10000} {
			for _, mode := range []string{"warm", "stale", "cold"} {
				benchmarkWorkerPattern(b, backend, tasks, mode)
			}
		}
	}
}

func benchmarkWorkerPattern(b *testing.B, backend string, tasks int, mode string) {
	{
		{
			b.Run(fmt.Sprintf("%s/tasks=%d/%s", backend, tasks, mode), func(b *testing.B) {
				root := workerPatternRun(b, backend, tasks)
				key, err := filepath.Abs(root)
				if err != nil {
					b.Fatal(err)
				}
				store := FileCheckpointStore{Root: root}
				commit := func(i int) {
					if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
						s.Tasks[workerPatternTask(s)].Attempt = i
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
					switch mode {
					case "cold":
						committedCache.Lock()
						delete(committedCache.entries, key)
						committedCache.Unlock()
					case "stale":
						committedCache.Lock()
						stale := committedCache.entries[key]
						committedCache.Unlock()
						commit(1000 + 2*i) // two peer commits this process never saw
						commit(1001 + 2*i)
						committedCache.Lock()
						committedCache.entries[key] = stale
						committedCache.Unlock()
					}
					b.StartTimer()
					commit(100000 + i)
				}
			})
		}
	}
}

func workerPatternRun(b testing.TB, backend string, tasks int) string {
	b.Helper()
	if backend == "managed" {
		p := sharedProfileState(b, tasks, false).Plan
		root := filepath.Join(b.TempDir(), "run")
		if err := Initialize(p, root, "managed", model.Options{Workers: 6, Timeout: 30 * time.Second, Budget: .01}); err != nil {
			b.Fatal(err)
		}
		if err := EnableJournal(root); err != nil {
			b.Fatal(err)
		}
		return root
	}
	p := model.Plan{APIVersion: "swarm/v1", Name: "worker pattern", MaxTasks: tasks, Tasks: make([]model.Task, tasks)}
	for i := range p.Tasks {
		p.Tasks[i] = model.Task{ID: fmt.Sprintf("scale_%05d", i), Prompt: "x", Requires: []string{"answer"}, Equals: map[string]string{"answer": "ok"}, MockResult: map[string]string{"answer": "ok"}}
	}
	root := filepath.Join(b.TempDir(), "run")
	if err := Initialize(p, root, "mock", model.Options{Workers: 6, Timeout: 30 * time.Second, Budget: .01}); err != nil {
		b.Fatal(err)
	}
	if err := EnableJournal(root); err != nil {
		b.Fatal(err)
	}
	return root
}

// workerPatternTask returns a stable task id to touch: scale_00001 on the mock
// plan, task_00001 on the managed one.
func workerPatternTask(s *model.State) string {
	if _, ok := s.Tasks["scale_00001"]; ok {
		return "scale_00001"
	}
	return "task_00001"
}
