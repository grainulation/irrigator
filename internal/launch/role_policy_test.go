package launch

import (
	"encoding/json"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestRoleRoutingPreservesSourceAndAllowedProfiles(t *testing.T) {
	_, catalog, _ := executionFixture(t, "mock")
	catalog.Profiles["careful"] = catalog.Profiles["small"]
	plan := model.Plan{APIVersion: "swarm/v3", Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small", "careful"}}, Tasks: []model.Task{{ID: "a"}, {ID: "b", Profile: "small"}}}
	before, _ := json.Marshal(plan)
	policy := RolePolicy{Schema: "irrigator.roles/v1", Roles: map[string]map[string]string{"research": {"easy": "small", "hard": "careful"}}, Tasks: map[string]roleHint{"a": {Role: "research", Difficulty: "hard"}}}
	routed, routes, err := RouteProfiles(plan, policy)
	if err != nil {
		t.Fatal(err)
	}
	if routed.Tasks[0].Profile != "careful" || routed.Tasks[1].Profile != "small" || len(routes) != 2 || routes[0].RequestedModel != "mock" {
		t.Fatalf("unexpected routing: %+v %+v", routed.Tasks, routes)
	}
	after, _ := json.Marshal(plan)
	if string(before) != string(after) {
		t.Fatal("routing mutated source plan")
	}
	routed.Execution.Catalog.Profiles["injected"] = catalog.Profiles["small"]
	if _, ok := plan.Execution.Catalog.Profiles["injected"]; ok {
		t.Fatal("routed plan aliases source catalog")
	}
	policy.Roles["research"]["hard"] = "missing"
	if _, _, err := RouteProfiles(plan, policy); err == nil {
		t.Fatal("accepted unavailable profile")
	}
	policy.Roles["research"]["hard"] = "careful"
	plan.Execution.AllowedProfiles = []string{"small"}
	if _, _, err := RouteProfiles(plan, policy); err == nil {
		t.Fatal("widened allowed profiles")
	}
}

func TestRoleRoutingRejectsUnknownTaskAndMissingTier(t *testing.T) {
	_, catalog, _ := executionFixture(t, "mock")
	plan := model.Plan{APIVersion: "swarm/v3", Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}}, Tasks: []model.Task{{ID: "a"}}}
	policy := RolePolicy{Schema: "irrigator.roles/v1", Roles: map[string]map[string]string{"research": {"easy": "small"}}, Tasks: map[string]roleHint{"unknown": {Role: "research", Difficulty: "easy"}}}
	if _, _, err := RouteProfiles(plan, policy); err == nil {
		t.Fatal("accepted unknown task")
	}
	policy.Tasks = map[string]roleHint{"a": {Role: "research", Difficulty: "hard"}}
	if _, _, err := RouteProfiles(plan, policy); err == nil {
		t.Fatal("silently guessed missing tier")
	}
}
