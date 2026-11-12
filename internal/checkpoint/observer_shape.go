package checkpoint

import (
	"errors"
	"fmt"

	"github.com/grainulation/irrigator/internal/model"
)

func ObsSpawnLimit(sp *model.SpawnPolicy) int {
	if sp.MaxChildren == 0 {
		return 8
	}
	return sp.MaxChildren
}

// obsShapeCeiling reuses the canonical shapeMaxNodes bound (<= 1024).
func ObsShapeCeiling(sh model.ObserverShape) (int, error) {
	n, err := ShapeMaxNodes(Shape{Roots: sh.Roots, Levels: sh.Levels, MaxChildren: sh.MaxChildren})
	if err != nil {
		return 0, fmt.Errorf("observer_shape: %w", err)
	}
	return n, nil
}

// validateObserverShape is called ONLY at initial plan load/initialize, never
// from generic validate(), so later graph revisions are not vetoed by
// observer metadata. nil shape is always valid. The projection
// also calls it to re-check the persisted original plan.
func ValidateObserverShape(p model.Plan) error {
	sh := p.ObserverShape
	if sh == nil {
		return nil
	}
	if p.MaxTasks < 0 || p.MaxTasks > 1024 || p.MaxDepth < 0 || p.MaxDepth > 16 {
		return errors.New("observer_shape: plan max_tasks/max_depth out of bounds")
	}
	if sh.Roots < 1 || sh.Levels < 1 || sh.MaxChildren < 1 {
		return errors.New("observer_shape: roots, levels and max_children must be positive")
	}
	if sh.Levels > MaxShapeDepth {
		return fmt.Errorf("observer_shape: total levels must be at most %d", MaxShapeDepth)
	}
	if _, err := ObsShapeCeiling(*sh); err != nil {
		return err
	}
	roots := 0
	for _, t := range p.Tasks {
		if t.Parent == "" {
			roots++
		}
	}
	if roots != sh.Roots {
		return errors.New("observer_shape: roots does not match plan root task count")
	}
	depth := p.MaxDepth
	if depth == 0 {
		depth = 4
	}
	if sh.Levels-1 > depth {
		return errors.New("observer_shape: levels-1 exceeds effective max_depth")
	}
	if sh.Levels > 1 {
		for _, t := range p.Tasks {
			if t.Parent != "" {
				continue
			}
			if t.Spawn == nil {
				return errors.New("observer_shape: root without spawn policy contradicts levels > 1")
			}
			if ObsSpawnLimit(t.Spawn) > sh.MaxChildren {
				return errors.New("observer_shape: a root's spawn cap exceeds declared max_children")
			}
		}
	}
	return nil
}
