//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestManagedHistoryActualTurnRuntime(t *testing.T) {
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	profile := catalog.Profiles["small"]
	profile.RequiredCapabilities = append(profile.RequiredCapabilities, "explicit_history")
	profile.Limits = model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}
	catalog.Profiles["small"] = profile
	next := runfixture.Task("next", 0)
	first := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next})
	first.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
	p := model.Plan{APIVersion: model.GovernedPlanVersion, Name: "history runtime", MaxTasks: 2, TurnShape: &model.TurnShape{Roots: 1, MainTurns: 2}, Tasks: []model.Task{first}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, CostPolicy: "retain_unknown", RunBudgetUSD: "0"}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, root, "--workers", "1")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	if s.Tasks["a__turn2"].Status != "verified" {
		t.Fatal("continuation did not finish")
	}
	raw, err := os.ReadFile(filepath.Join(root, "cells", "a__turn2", "1.input.json"))
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		History *managed.ManagedHistory `json:"history"`
	}
	if json.Unmarshal(raw, &input) != nil || input.History == nil || len(input.History.Entries) != 1 || input.History.Entries[0].Output["answer"] != "ok" {
		t.Fatalf("history not delivered: %s", raw)
	}
}
