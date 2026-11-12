package cli

import (
	"testing"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func unbudgetedCountEvents(s *model.State, kind string) int {
	n := 0
	for _, e := range s.Events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// unbudgetedLegacyFixture builds a legacy checkpoint with no schema_version, no saved
// call limits and no unbudgeted decision. The only task is already verified, so
// resuming it can never dispatch a provider call, whatever the backend.
func unbudgetedLegacyFixture(t *testing.T, backend string) string {
	t.Helper()
	root := runfixture.New(t, runfixture.Task("a", 0.01))
	runfixture.MutateStateJSON(t, root, func(raw map[string]any) {
		raw["backend"] = backend
		delete(raw, "schema_version")
		delete(raw, "call_limits")
		delete(raw, "unbudgeted_decision")
		row := raw["tasks"].(map[string]any)["a"].(map[string]any)
		row["status"] = "verified"
		row["attempt"] = 1
		row["result"] = map[string]any{"answer": "accepted"}
	})
	return root
}

func worker(root, id string, opts model.Options) error {
	return executor.WorkerOperation(root, id, opts, "")
}
