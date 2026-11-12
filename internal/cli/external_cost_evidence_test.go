package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// ---- 1. command-provider cost evidence: explicit micros ---------------------

// ---- 2. held reservations are never provider spend ---------------------------

// ---- 3. direct worker admission needs recorded consent -----------------------

// File-level: the real checkpoint transaction (as used by direct worker entry)
// refuses without writing, and honors a durable allow decision. No provider runs.
func TestBeginAttemptCheckpointTransaction(t *testing.T) {
	legacy := func(t *testing.T, schema any) string {
		root := runfixture.New(t, runfixture.Task("cell", 0.01))
		runfixture.MutateStateJSON(t, root, func(raw map[string]any) {
			raw["backend"] = "haiku"
			raw["control"] = "run"
			delete(raw, "call_limits")
			delete(raw, "unbudgeted_decision")
			if schema == nil {
				delete(raw, "schema_version")
			} else {
				raw["schema_version"] = schema
			}
		})
		return root
	}
	for name, schema := range map[string]any{"no schema": nil, "schema 2": 2} {
		t.Run(name, func(t *testing.T) {
			root := legacy(t, schema)
			statePath := filepath.Join(root, "state.json")
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (executor.CheckpointCells{Root: root}).BeginAttempt("cell", "", 123); !errors.Is(err, executor.ErrLegacyUnbudgeted) {
				t.Fatalf("BeginAttempt = %v, want errLegacyUnbudgeted", err)
			}
			after, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("refused admission rewrote the checkpoint")
			}
		})
	}
	t.Run("recorded allow admits", func(t *testing.T) {
		root := legacy(t, 2)
		if err := admitLegacyResume(root, true, false, runfixture.UnbudgetedTestOptions()); err != nil {
			t.Fatal(err)
		}
		a, err := (executor.CheckpointCells{Root: root}).BeginAttempt("cell", "", 123)
		if err != nil || a.Fence.Attempt != 1 {
			t.Fatalf("allowed run refused: %v %+v", err, a.Fence)
		}
	})
}
