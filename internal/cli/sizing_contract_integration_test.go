//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func sizingContractFile(t *testing.T, balance, total float64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sizing.json")
	v := map[string]any{"version": "sizing.v1", "capacity": 4, "budget_usd": total, "provider_balance_usd": map[string]float64{"mock": balance}, "defaults": map[string]any{"service_s": 1, "cost_usd": .01, "provenance": "assumed"}}
	b, _ := json.Marshal(v)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSizingControllerInfeasibleStopsWithoutAttempt(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0), runfixture.Task("b", 0))
	before := runfixture.Read(t, root).PlanHash
	_, done := clitest.Start(t, root, "--workers", "4", "--sizing", sizingContractFile(t, 1, 0), "--budget", ".25")
	clitest.Finished(t, done, false)
	s := runfixture.Read(t, root)
	if s.Control != "blocked" || s.Tasks["a"].Attempt != 0 || s.Tasks["b"].Attempt != 0 {
		t.Fatalf("unsafe or nonterminal admission: %+v", s.Tasks)
	}
	if s.PlanHash != before {
		t.Fatal("sizing mutated immutable plan")
	}
}

func TestSizingControllerReservationPersistsAcrossResume(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .02), runfixture.Task("b", .02), runfixture.Task("c", .02))
	before := runfixture.Read(t, root).PlanHash
	_, done := clitest.Start(t, root, "--workers", "4", "--sizing", sizingContractFile(t, .30, 1), "--budget", ".25")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	live := map[string]bool{}
	for _, e := range s.Events {
		switch e.Kind {
		case "started":
			live[e.Task] = true
			if len(live) > 1 {
				t.Fatal("provider reservation cap exceeded")
			}
		case "verified":
			delete(live, e.Task)
		}
	}
	if s.PlanHash != before {
		t.Fatal("plan changed")
	}
	if _, err := os.Stat(filepath.Join(root, "sizing.json")); err != nil {
		t.Fatal(err)
	}
	_, done = clitest.Start(t, root, "--workers", "4", "--budget", ".25")
	clitest.Finished(t, done, true)
	for _, r := range runfixture.Read(t, root).Tasks {
		if r.Attempt != 1 {
			t.Fatal("verified work replayed")
		}
	}
}
