//go:build integration

package cli

import (
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestManagedTurnInheritanceAndProfileBoundary(t *testing.T) {
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	p := catalog.Profiles["small"]
	p.Limits = model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}
	catalog.Profiles["small"] = p
	catalog.Profiles["other"] = p
	makeRun := func(change bool) string {
		child := runfixture.Task("helper", 0)
		child.MockResult = map[string]string{"answer": "helper"}
		if change {
			child.Profile = "other"
		}
		next := runfixture.Task("next", 0)
		next.MockResult = map[string]string{"answer": "next"}
		first := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Helpers: []model.Task{child}, Continue: &next, WaitForHelpers: true})
		first.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
		second := runfixture.Task("b", 0)
		second.Profile = "other"
		second.Turn = &model.TurnIdentity{AgentID: "b", Role: "main", Index: 1}
		shape := model.TurnShape{Roots: 2, MainTurns: 2, HelpersPerTurn: 1}
		report, e := model.ReportTurnShape(shape)
		if e != nil {
			t.Fatal(e)
		}
		plan := model.Plan{APIVersion: launch.ManagedPlanVersion, Name: "mixed profiles", TurnShape: &shape, MaxTasks: report.MaxExecutions, Tasks: []model.Task{first, second}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small", "other"}, RunBudgetUSD: "0"}}
		root := filepath.Join(t.TempDir(), "run")
		if e := checkpoint.Initialize(plan, root, "managed"); e != nil {
			t.Fatal(e)
		}
		return root
	}
	t.Run("inherit", func(t *testing.T) {
		root := makeRun(false)
		_, done := clitest.Start(t, root, "--workers", "2")
		clitest.Finished(t, done, true)
		s := runfixture.Read(t, root)
		if len(s.Tasks) != 4 {
			t.Fatalf("got%d cells", len(s.Tasks))
		}
		for id, row := range s.Tasks {
			if row.Status != "verified" {
				t.Fatalf("%s: %s %s", id, row.Status, row.Error)
			}
			b, e := checkpoint.ManagedBinding(s, row)
			if e != nil {
				t.Fatal(e)
			}
			want := "small"
			if id == "b" {
				want = "other"
			}
			if b.ProfileName != want {
				t.Fatalf("%s routed to%s", id, b.ProfileName)
			}
		}
	})
	t.Run("reject-generated-reroute", func(t *testing.T) {
		root := makeRun(true)
		_, done := clitest.Start(t, root, "--workers", "2")
		clitest.Finished(t, done, false)
		s := runfixture.Read(t, root)
		if len(s.Tasks) != 2 || s.Tasks["a"].Status != "failed" {
			t.Fatal("generated profile escalation admitted")
		}
	})
}
