package checkpoint

import (
	"sort"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func TestProfileCheckpointEventUpdates(t *testing.T) {
	root := fixture(t, task("a", 0), task("b", 0), task("c", 0), task("d", 0), task("e", 0))

	const iterations = 50
	latencies := make([]time.Duration, 0, iterations)

	for i := 0; i < iterations; i++ {
		start := time.Now()
		err := Update(root, func(s *model.State) error {
			s.Event("profile_tick", "a", map[string]int{"iteration": i})
			return nil
		})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("update failed at iteration %d: %v", i, err)
		}
		latencies = append(latencies, elapsed)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[len(latencies)*50/100]
	p90 := latencies[len(latencies)*90/100]
	p99 := latencies[len(latencies)*99/100]

	t.Logf("writeState (atomicJSON + 2x fsync) profile over %d writes on APFS/SSD:", iterations)
	t.Logf("  p50: %v", p50)
	t.Logf("  p90: %v", p90)
	t.Logf("  p99: %v", p99)

	// This small-fixture diagnostic does not establish large-plan performance.
	// Deterministic no-op assertions and the bounded scale harness cover that
	// separate scope; elapsed-time budgets belong to declared benchmark hardware.
}
