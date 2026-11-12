package graph

import (
	"testing"
)

func dstatWant(t *testing.T, got DeliveryView, stage, acceptance, recovery string) {
	t.Helper()
	if got.Stage != stage || got.Acceptance != acceptance || got.Recovery != recovery {
		t.Fatalf("projection %+v, want %s/%s/%s", got, stage, acceptance, recovery)
	}
}

func commitTurnExpansion(root, id, level string) error {
	return CommitTurnExpansionFenced(root, id, level, nil)
}

func commitExpansion(root, id, level string) error {
	return CommitExpansionFenced(root, id, level, nil)
}
