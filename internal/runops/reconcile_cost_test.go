package runops

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestReconcileUnresolvedCleanupRetainsObservedHaikuCharge(t *testing.T) {
	for _, marker := range []string{`{"status":"active","supervisor_pid":0,"cleanup_error":"unconfirmed"}`, `{incomplete-marker`} {
		root := runfixture.HaikuRoot(t)
		if err := checkpoint.Update(root, func(s *model.State) error {
			row := s.Tasks["a"]
			row.Status = "running"
			row.Attempt = 2
			row.Cost = .02
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		prefix := filepath.Join(root, "cells", "a", "2")
		if err := os.WriteFile(prefix+".stdout.json", []byte(`{"structured_output":{"answer":"not accepted"},"total_cost_usd":0.07}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prefix+".process.json", []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		// Another attempt's report must never be read or charged here.
		if err := os.WriteFile(filepath.Join(root, "cells", "a", "1.stdout.json"), []byte(`{"total_cost_usd":9}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(root); err != nil {
			t.Fatal(err)
		}
		s := runfixture.Read(t, root)
		row := s.Tasks["a"]
		if row.Status != "uncertain" || math.Abs(row.Cost-(.02+.07)) > 1e-12 || len(row.Result) != 0 || !strings.Contains(row.Error, "cleanup unconfirmed") || provider.ProjectCost(s).UnknownOperations != 1 {
			t.Fatalf("observed charge lost or certainty invented: %+v / %+v", row, provider.ProjectCost(s))
		}
		before := row.OperationID
		if err := Reconcile(root); err != nil {
			t.Fatal(err)
		}
		s = runfixture.Read(t, root)
		if math.Abs(s.Tasks["a"].Cost-(.02+.07)) > 1e-12 || s.Tasks["a"].OperationID != before || provider.ProjectCost(s).UnknownOperations != 1 {
			t.Fatal("reconciliation duplicated charge or changed execution identity")
		}
		raw, err := os.ReadFile(prefix + ".process.json")
		if err != nil || string(raw) != marker {
			t.Fatal("reconciliation rewrote cleanup evidence")
		}
		if err := retryNode(root, "a", false, false); err == nil {
			t.Fatal("unresolved paid work replay authorized")
		}
	}
}

func TestReconcileUnresolvedCleanupRejectsIncompleteCharge(t *testing.T) {
	for _, raw := range []string{`{"total_cost_usd":`, `{"total_cost_usd":-1}`, `{"answer":"no billing"}`} {
		root := runfixture.HaikuRoot(t)
		if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "running"; s.Tasks["a"].Attempt = 1; return nil }); err != nil {
			t.Fatal(err)
		}
		prefix := filepath.Join(root, "cells", "a", "1")
		if err := os.WriteFile(prefix+".stdout.json", []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prefix+".process.json", []byte(`{"status":"active","supervisor_pid":0}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(root); err != nil {
			t.Fatal(err)
		}
		s := runfixture.Read(t, root)
		if s.Tasks["a"].Status != "uncertain" || s.Tasks["a"].Cost != 0 || provider.ProjectCost(s).UnknownOperations != 1 {
			t.Fatal("invalid charge became settled evidence")
		}
	}
}
