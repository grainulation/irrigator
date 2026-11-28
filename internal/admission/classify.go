package admission

import (
	"sync"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

// Classifier says whether a task's already-bound execution is local or remote.
// It reads the binding and never writes it. The answer is memoized per binding,
// because a large run has thousands of tasks but only a few distinct bindings.
type Classifier struct {
	mu   sync.Mutex
	memo map[string]Locality
}

// NewClassifier returns an empty classifier.
func NewClassifier() *Classifier { return &Classifier{memo: map[string]Locality{}} }

// Locality classifies task id. mock and ollama_http bindings run on this
// machine; every other managed adapter (the hosted CLIs) is remote, and so is a
// binding that cannot be resolved. A legacy run is remote only on the paid haiku
// backend.
func (c *Classifier) Locality(s *model.State, id string) Locality {
	row := s.Tasks[id]
	if row == nil {
		return Remote
	}
	if !checkpoint.ManagedRun(s) {
		if s.Backend == "haiku" {
			return Remote
		}
		return Local
	}
	key := model.ManagedProfile(s.Plan, row.Spec) + "\x00" + row.BindingDigest
	c.mu.Lock()
	defer c.mu.Unlock()
	if loc, ok := c.memo[key]; ok {
		return loc
	}
	loc := Remote
	if b, err := checkpoint.ManagedBinding(s, row); err == nil {
		switch b.Adapter.Kind {
		case "mock", "ollama_http":
			loc = Local
		}
	}
	c.memo[key] = loc
	return loc
}
