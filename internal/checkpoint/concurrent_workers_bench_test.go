package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

const concurrentHelperEnv = "IRRIGATOR_TEST_CONCURRENT_WORKER"

// TestConcurrentWorkerHelper is the body of one simulated worker process; it
// does nothing unless BenchmarkJournalConcurrentWorkers started it. Each task
// starts with a cold cache, as a freshly launched worker does, then makes the
// three commits a real task makes.
func TestConcurrentWorkerHelper(t *testing.T) {
	root := os.Getenv(concurrentHelperEnv)
	if root == "" {
		t.Skip("helper process only")
	}
	proc, _ := strconv.Atoi(os.Getenv("IRRIGATOR_TEST_PROC"))
	tasks, _ := strconv.Atoi(os.Getenv("IRRIGATOR_TEST_TASKS"))
	prefix := os.Getenv("IRRIGATOR_TEST_PREFIX")
	store := FileCheckpointStore{Root: root}
	for i := 0; i < tasks; i++ {
		dropJournalCache(t, root)
		id := fmt.Sprintf("%s%05d", prefix, 100+proc*100+i)
		for c := 1; c <= 3; c++ {
			c := c
			if err := store.Update(context.Background(), CheckpointUpdateOptions{}, func(s *model.State) error {
				s.Tasks[id].Attempt = c
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	out, _ := json.Marshal(StorageMetrics())
	fmt.Printf("METRICS %s\n", out)
}

// BenchmarkJournalConcurrentWorkers runs six worker-like processes against one
// shared run. It reports commits per second and the exclusive-lock time each
// commit holds, which is what a cold pre-warm is meant to shrink.
func BenchmarkJournalConcurrentWorkers(b *testing.B) {
	const procs, tasksPerProc = 6, 4
	for _, backend := range []string{"mock", "managed"} {
		for _, tasks := range []int{2000, 10000} {
			for _, prewarm := range []string{"off", "on"} {
				b.Run(fmt.Sprintf("%s/tasks=%d/prewarm=%s", backend, tasks, prewarm), func(b *testing.B) {
					prefix := "scale_"
					if backend == "managed" {
						prefix = "task_"
					}
					var heldMS, waitMS, commits uint64
					var wall time.Duration
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						root := workerPatternRun(b, backend, tasks)
						b.StartTimer()
						start := time.Now()
						var wg sync.WaitGroup
						var mu sync.Mutex
						for p := 0; p < procs; p++ {
							wg.Add(1)
							go func(p int) {
								defer wg.Done()
								cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentWorkerHelper$", "-test.v")
								cmd.Env = append(os.Environ(), concurrentHelperEnv+"="+root, "IRRIGATOR_TEST_PROC="+strconv.Itoa(p),
									"IRRIGATOR_TEST_TASKS="+strconv.Itoa(tasksPerProc), "IRRIGATOR_TEST_PREFIX="+prefix, "IRRIGATOR_JOURNAL_PREWARM="+prewarm)
								out, err := cmd.CombinedOutput()
								if err != nil {
									b.Errorf("worker %d: %v\n%s", p, err, out)
									return
								}
								for _, line := range strings.Split(string(out), "\n") {
									if rest, ok := strings.CutPrefix(line, "METRICS "); ok {
										var m map[string]uint64
										if json.Unmarshal([]byte(rest), &m) == nil {
											mu.Lock()
											heldMS += m["lock_held_ms"]
											waitMS += m["lock_wait_ms"]
											commits += m["publications"] + m["unchanged_updates"]
											mu.Unlock()
										}
									}
								}
							}(p)
						}
						wg.Wait()
						wall += time.Since(start)
					}
					total := float64(procs * tasksPerProc * 3 * b.N)
					b.ReportMetric(total/wall.Seconds(), "commits/s")
					b.ReportMetric(float64(heldMS)/total, "lock-held-ms/commit")
					b.ReportMetric(float64(waitMS)/total, "lock-wait-ms/commit")
				})
			}
		}
	}
}
