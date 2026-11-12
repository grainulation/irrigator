package delivery

import (
	"github.com/grainulation/irrigator/internal/graph"
)

func commitExpansion(root, id, level string) error {
	return graph.CommitExpansionFenced(root, id, level, nil)
}
