package admission

import (
	"sync"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

// classifier says whether a task's already-bound execution is local or remote.
// It reads the binding and never writes it. The answer is memoized per binding,
// because a large run has thousands of tasks but only a few distinct bindings.
type classifier struct {
	mu   sync.Mutex
	memo map[string]locality
}

// newClassifier returns an empty classifier.
func newClassifier() *classifier { return &classifier{memo: map[string]locality{}} }

// locality classifies task id. mock and ollama_http bindings run on this
// machine; every other managed adapter (the hosted CLIs) is remote, and so is a
// binding that cannot be resolved. A legacy run is remote only on the paid haiku
// backend.
func (c *classifier) locality(s *model.State, id string) locality {
	row := s.Tasks[id]
	if row == nil {
		return remote
	}
	if !checkpoint.ManagedRun(s) {
		if s.Backend == "haiku" {
			return remote
		}
		return local
	}
	key := model.ManagedProfile(s.Plan, row.Spec) + "\x00" + row.BindingDigest
	c.mu.Lock()
	defer c.mu.Unlock()
	if loc, ok := c.memo[key]; ok {
		return loc
	}
	loc := remote
	if b, err := checkpoint.ManagedBinding(s, row); err == nil {
		switch b.Adapter.Kind {
		case "mock", "ollama_http":
			loc = local
		}
	}
	c.memo[key] = loc
	return loc
}
