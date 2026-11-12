package launch

import (
	"fmt"

	"github.com/grainulation/irrigator/internal/model"
)

// InputFields is an allowlist over the artifacts already accessible to a task.
// Omission passes the complete dependency/ancestor input. A nonempty
// selector includes only the named nodes and fields, without changing readiness.
func SelectInputFields(task model.Task, inputs map[string]map[string]string) (map[string]map[string]string, error) {
	if len(task.InputFields) == 0 {
		return inputs, nil
	}
	selected := make(map[string]map[string]string, len(task.InputFields))
	for id, fields := range task.InputFields {
		available, ok := inputs[id]
		if !ok {
			return nil, fmt.Errorf("%s input_fields source %s is unavailable", task.ID, id)
		}
		selected[id] = make(map[string]string, len(fields))
		for _, field := range fields {
			value, ok := available[field]
			if !ok {
				return nil, fmt.Errorf("%s input_fields field %s.%s is unavailable", task.ID, id, field)
			}
			selected[id][field] = value
		}
	}
	return selected, nil
}

func ValidateInputFields(p model.Plan, task model.Task) error {
	if len(task.InputFields) == 0 {
		return nil
	}
	tasks := make(map[string]model.Task, len(p.Tasks))
	for _, t := range p.Tasks {
		tasks[t.ID] = t
	}
	ancestors := func(id string) map[string]bool {
		seen := map[string]bool{}
		for id != "" && !seen[id] {
			seen[id] = true
			id = tasks[id].Parent
		}
		return seen
	}
	parents := ancestors(task.Parent)
	for id, fields := range task.InputFields {
		source, exists := tasks[id]
		allowed := parents[id]
		lineage := ancestors(id)
		for _, dep := range task.DependsOn {
			allowed = allowed || lineage[dep]
		}
		if !exists || !allowed || id == task.ID {
			return fmt.Errorf("%s input_fields source %s is not a dependency subtree or ancestor", task.ID, id)
		}
		required := map[string]bool{}
		for _, field := range source.Requires {
			required[field] = true
		}
		seen := map[string]bool{}
		for _, field := range fields {
			if !required[field] || seen[field] {
				return fmt.Errorf("%s input_fields field %s.%s must be required and unique", task.ID, id, field)
			}
			seen[field] = true
		}
	}
	return nil
}
