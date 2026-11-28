package checkpoint

import (
	"fmt"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func sharedProfileState(tb testing.TB, count int, completed bool) *model.State {
	tb.Helper()
	catalog := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema,
		Adapters:  map[string]model.ExecutionAdapter{"mock": {Kind: "mock", Protocol: model.ProtocolVersion}},
		Instances: map[string]model.ExecutionInstance{"local": {Adapter: "mock", CredentialRefs: []model.ExecutionCredentialRef{}}},
		Profiles:  map[string]model.ExecutionProfile{"one": {Instance: "local", Model: "mock", Effort: "none", RequiredCapabilities: []string{"structured_strings", "outcome_certainty"}, Limits: model.ExecutionLimits{InputBytes: 1024, OutputBytes: 1024}, Tools: []string{}, OperationBudgetUSD: "0"}}, DefaultProfile: "one"}
	p := model.Plan{APIVersion: launch.ArtifactPlanVersion, MaxTasks: count, TurnShape: &model.TurnShape{Roots: count, MainTurns: 1}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"one"}, CostPolicy: "track", RunBudgetUSD: "0"}}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("task_%05d", i)
		p.Tasks = append(p.Tasks, model.Task{ID: id, Prompt: "produce answer", Requires: []string{"answer"}, Turn: &model.TurnIdentity{AgentID: id, Role: "main", Index: 1}})
	}
	e, err := InitializeManagedExecution(p)
	if err != nil {
		tb.Fatal(err)
	}
	s := &model.State{Version: 5, Plan: p, Backend: "managed", RunID: "shared-profile", Execution: e, Tasks: map[string]*model.TaskState{}}
	for _, task := range p.Tasks {
		row := s.NewTask(task)
		if completed {
			row.Status, row.Attempt, row.CostUnknown = "verified", 1, true
			row.Result = map[string]string{"answer": "ok"}
			e.Reservations[row.OperationID] = model.ManagedReservation{TaskID: task.ID, OperationID: row.OperationID, Attempt: 1, BindingDigest: row.BindingDigest, Status: "unknown_cost"}
		}
		s.Tasks[task.ID] = row
	}
	return s
}

func TestManagedSharedProfileValidationRetainsCorruptionChecks(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*model.State)
	}{
		{"unknown-row-binding", func(s *model.State) { s.Tasks["task_00009"].BindingDigest = "unknown" }},
		{"altered-binding-content", func(s *model.State) {
			b := s.Execution.Bindings["one"]
			b.Profile.Model = "different"
			s.Execution.Bindings["one"] = b
		}},
		{"reservation-binding", func(s *model.State) {
			for op, r := range s.Execution.Reservations {
				r.BindingDigest = "unknown"
				s.Execution.Reservations[op] = r
				break
			}
		}},
		{"reservation-attempt", func(s *model.State) {
			for op, r := range s.Execution.Reservations {
				r.Attempt = 2
				s.Execution.Reservations[op] = r
				break
			}
		}},
		{"owner-profile", func(s *model.State) { s.Tasks["task_00009"].Spec.Parent = "missing" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := sharedProfileState(t, 10, true)
			if err := ValidateManagedState(s); err != nil {
				t.Fatal(err)
			}
			change.apply(s)
			if err := ValidateManagedState(s); err == nil {
				t.Fatal("corrupt shared-profile state accepted")
			}
		})
	}
}

func BenchmarkValidateManagedStateSharedProfile(b *testing.B) {
	for _, completed := range []bool{false, true} {
		b.Run(fmt.Sprintf("completed=%t", completed), func(b *testing.B) {
			s := sharedProfileState(b, model.MaxGraphTasks, completed)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := ValidateManagedState(s); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
