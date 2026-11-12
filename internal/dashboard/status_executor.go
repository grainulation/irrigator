package dashboard

import (
	"fmt"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/model"
)

// runExecutor names the executor a run uses: the recorded one, or the default
// for runs that predate executor.json. An unreadable record is reported as
// "unknown" rather than guessed.
func RunExecutor(root string) string {
	name, err := executor.ReadExecutorRecord(root)
	if err != nil {
		return "unknown"
	}
	if name == "" {
		return executor.DefaultExecutor
	}
	return name
}

// statusReport is the `irrigator status --json` document: the run state plus
// the executor.
type statusReport struct {
	*model.State
	Executor string `json:"executor"`
}

func NewStatusReport(s *model.State, root string) statusReport {
	return statusReport{State: s, Executor: RunExecutor(root)}
}

func PrintStatusExecutor(root string) {
	fmt.Printf("executor: %s\n", RunExecutor(root))
}
