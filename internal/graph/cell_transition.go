package graph

import (
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

func FenceForCell(s *model.State, row *model.TaskState) managed.CellFence {
	return managed.CellFence{RunID: s.RunID, TaskID: row.Spec.ID, OperationID: row.OperationID, BindingDigest: row.BindingDigest, Attempt: row.Attempt}
}
