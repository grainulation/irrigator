package observer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/graph"
)

func bReadLedger(t *testing.T, dir string) ProviderLedger {
	t.Helper()
	var l ProviderLedger
	raw, err := os.ReadFile(filepath.Join(dir, BudgetLedgerFile))
	if err != nil || json.Unmarshal(raw, &l) != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return l
}

func commitTurnExpansion(root, id, level string) error {
	return graph.CommitTurnExpansionFenced(root, id, level, nil)
}
