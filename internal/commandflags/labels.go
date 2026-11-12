package commandflags

import (
	"context"
	"runtime/pprof"
)

// goLabeled runs fn in a new goroutine carrying pprof labels (key/value pairs)
// so goroutine dumps and profiles can be attributed to a run, task or worker.
// It adds labels only; scheduling and behavior are unchanged.
func GoLabeled(fn func(), kv ...string) {
	go pprof.Do(context.Background(), pprof.Labels(kv...), func(context.Context) { fn() })
}
