package executor

import (
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func workerBoundaries(s *model.State, t model.Task) *model.WorkerBoundaries {
	maxTasks := s.Plan.MaxTasks
	if maxTasks == 0 {
		maxTasks = 128
	}
	b := &model.WorkerBoundaries{Topology: "recursive", TaskSlotsRemaining: max(0, maxTasks-len(s.Tasks)), Basis: "admission_snapshot_not_reservation"}
	if checkpoint.HasTurnSemantics(s.Plan.APIVersion) && t.Turn != nil && s.Plan.TurnShape != nil {
		b.Topology = "turns"
		b.CurrentTurn = t.Turn.Index
		if t.Turn.Role == "main" {
			b.RemainingTurns = max(0, s.Plan.TurnShape.MainTurns-t.Turn.Index)
			used := 0
			if row := s.Tasks[t.ID]; row != nil {
				used = len(row.Children)
			}
			b.HelpersRemaining = min(b.TaskSlotsRemaining, max(0, s.Plan.TurnShape.HelpersPerTurn-used))
			b.CanWaitForHelpers = b.RemainingTurns > 0 && b.TaskSlotsRemaining > 0
		}
		return b
	}
	depth := 0
	seen := map[string]bool{t.ID: true}
	for parent := t.Parent; parent != ""; {
		if seen[parent] || s.Tasks[parent] == nil {
			b.RemainingDepth = 0
			return b
		}
		seen[parent] = true
		depth++
		parent = s.Tasks[parent].Spec.Parent
	}
	limit := s.Plan.MaxDepth
	if limit == 0 {
		limit = 4
	}
	b.CurrentDepth = depth
	b.RemainingDepth = max(0, limit-depth)
	if t.Spawn != nil && b.RemainingDepth > 0 {
		children := t.Spawn.MaxChildren
		if children == 0 {
			children = 8
		}
		b.SpawnChildrenRemaining = min(children, b.TaskSlotsRemaining)
	}
	return b
}
