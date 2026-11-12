//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestRejectedTypedSpawnRetainsPaidParentEvidence(t *testing.T) {
	raw := `{"structured_output":{"answer":"paid research","children":{"tasks":[{"id":"child","prompt":"missing requires"}]}},"total_cost_usd":0.07}`
	runfixture.HaikuClaudeStub(t, "cat >/dev/null\nprintf '%s' '"+raw+"'\n")
	parent := model.Task{ID: "parent", Prompt: "research", Requires: []string{"answer", "children"}, Spawn: &model.SpawnPolicy{Field: "children"}}
	root := fixture(t, parent)
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Backend = "haiku"
		model.SaveCallLimits(s, model.Options{Budget: .25, Timeout: 120 * time.Second})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, done := start(t, root)
	finished(t, done, false)
	s := read(t, root)
	row := s.Tasks["parent"]
	if row.Status != "failed" || row.Cost != .07 || row.Result["answer"] != "paid research" || !strings.Contains(row.Result["children"], "missing requires") || len(s.Tasks) != 1 {
		t.Fatalf("lost result or admitted malformed child: %+v", row)
	}
	evidence, err := os.ReadFile(filepath.Join(root, "cells", "parent", "1.stdout.json"))
	if err != nil || string(evidence) != raw {
		t.Fatal("lost raw paid evidence", err)
	}
}
