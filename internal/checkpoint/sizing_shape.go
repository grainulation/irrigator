package checkpoint

import (
	"errors"
	"fmt"
)

// MaxShapeNodes bounds a swarm shape. Shape is the repo's A×D×S swarm shorthand: A root agents, D TOTAL ownership
// levels including roots, S maximum children per node. The coordinator is not
// counted; workers/concurrency are a separate quantity. The maximum is an
// upper bound, never a promised expansion.
const MaxShapeNodes = 1024
const MaxShapeDepth = 16

type Shape struct{ Roots, Levels, MaxChildren int }

// shapeMaxNodes = A*sum(S^k, k=0..D-1) with every step overflow-checked
// against maxShapeNodes (S=1 gives A*D).
func ShapeMaxNodes(sh Shape) (int, error) {
	if sh.Roots < 1 || sh.Levels < 1 || sh.MaxChildren < 1 || sh.Levels > MaxShapeDepth {
		return 0, errors.New("shape components must be positive and depth at most 16")
	}
	total, level := 0, sh.Roots
	for k := 0; k < sh.Levels; k++ {
		if level > MaxShapeNodes || total > MaxShapeNodes-level {
			return 0, fmt.Errorf("shape exceeds the %d node bound", MaxShapeNodes)
		}
		total += level
		if k < sh.Levels-1 {
			if level > MaxShapeNodes/sh.MaxChildren {
				return 0, fmt.Errorf("shape exceeds the %d node bound", MaxShapeNodes)
			}
			level *= sh.MaxChildren
		}
	}
	return total, nil
}
