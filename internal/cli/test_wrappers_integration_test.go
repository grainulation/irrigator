//go:build integration

package cli

// Convenience wrappers used only by tests.

import (
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/runops"
)

func commitTurnExpansion(root, id, level string) error {
	return graph.CommitTurnExpansionFenced(root, id, level, nil)
}

func commitExpansion(root, id, level string) error {
	return graph.CommitExpansionFenced(root, id, level, nil)
}

func retryNode(root, id string, recursive, allowUncertain bool, confirmStopped ...bool) error {
	_, err := runops.RetryNodeWithReport(root, id, recursive, allowUncertain, confirmStopped...)
	return err
}
