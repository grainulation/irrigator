package graph

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

// Revision may improve an admitted, unstarted turn's work, never rewrite lane
// history, capacity or identity. New turns/helpers still require their admission
// transaction. The existing revision machinery fences operations and archives
// replaced generations while preserving the immutable initial plan.
func ValidateTurnRevision(s *model.State, tasks []model.Task) error {
	if !checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
		return nil
	}
	if len(tasks) != len(s.Tasks) {
		return errors.New("turn revision must preserve the admitted task set")
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		row := s.Tasks[task.ID]
		if row == nil || seen[task.ID] {
			return errors.New("turn revision cannot add, remove or duplicate execution identities")
		}
		seen[task.ID] = true
		old := row.Spec
		if !reflect.DeepEqual(old.Turn, task.Turn) || old.Parent != task.Parent || !reflect.DeepEqual(old.DependsOn, task.DependsOn) || !reflect.DeepEqual(old.InputFields, task.InputFields) {
			return fmt.Errorf("turn revision cannot change identity, ownership or dependencies of %s", task.ID)
		}
		if taskJSON(old) == taskJSON(task) {
			continue
		}
		if old.Turn.Async != nil {
			return fmt.Errorf("turn revision cannot rewrite admitted asynchronous helper %s", task.ID)
		}
		if row.Status != "pending" || row.Attempt != 0 || row.ExpansionHash != "" || len(row.Children) > 0 {
			return fmt.Errorf("turn revision requires an unstarted pending execution: %s", task.ID)
		}
		if err := checkpoint.CapabilityError(s, old, task); err != nil {
			return fmt.Errorf("turn revision %s: %w", task.ID, err)
		}
	}
	return nil
}
