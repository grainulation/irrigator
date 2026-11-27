package checkpoint

import (
	"fmt"
	"strings"

	"github.com/grainulation/irrigator/internal/model"
)

// IDs and references are validated before this pass. Kahn traversal avoids
// repeatedly scanning a reverse-ordered chain once for each dependency level.
// The unresolved diagnostic retains original plan order for compatibility.
func validateDependencyGraph(tasks []model.Task) error {
	remaining := make(map[string]int, len(tasks))
	children := make(map[string][]string, len(tasks))
	ready := make([]string, 0, len(tasks))
	for _, task := range tasks {
		remaining[task.ID] = len(task.DependsOn)
		if len(task.DependsOn) == 0 {
			ready = append(ready, task.ID)
		}
		for _, dep := range task.DependsOn {
			children[dep] = append(children[dep], task.ID)
		}
	}
	for head := 0; head < len(ready); head++ {
		for _, id := range children[ready[head]] {
			remaining[id]--
			if remaining[id] == 0 {
				ready = append(ready, id)
			}
		}
	}
	if len(ready) == len(tasks) {
		return nil
	}
	blocked := make([]string, 0, len(tasks)-len(ready))
	for _, task := range tasks {
		if remaining[task.ID] > 0 {
			blocked = append(blocked, task.ID)
		}
	}
	return fmt.Errorf("dependency cycle; unresolved nodes: %s", strings.Join(blocked, ", "))
}
