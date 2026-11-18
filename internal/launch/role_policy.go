package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/grainulation/irrigator/internal/model"
)

// RolePolicy selects existing admitted-capable profiles from explicit task hints.
// It is an offline plan transformation, never a live rerouting policy.
type RolePolicy struct {
	Schema string                       `json:"schema"`
	Roles  map[string]map[string]string `json:"roles"`
	Tasks  map[string]roleHint          `json:"tasks"`
}

type roleHint struct {
	Role       string `json:"role"`
	Difficulty string `json:"difficulty"`
}

type profileRoute struct {
	Task               string `json:"task"`
	Role               string `json:"role,omitempty"`
	Difficulty         string `json:"difficulty,omitempty"`
	Previous           string `json:"previous_profile"`
	Profile            string `json:"profile"`
	RequestedModel     string `json:"requested_model"`
	OperationBudgetUSD string `json:"operation_budget_usd"`
}

// RouteProfiles makes an independent plan; allowed profiles are never widened.
func RouteProfiles(plan model.Plan, policy RolePolicy) (model.Plan, []profileRoute, error) {
	if policy.Schema != "irrigator.roles/v1" || len(policy.Tasks) == 0 || len(policy.Tasks) > 10000 || len(policy.Roles) == 0 {
		return model.Plan{}, nil, errors.New("role policy requires schema irrigator.roles/v1, roles and explicit task hints")
	}
	if plan.Execution == nil || !IsManagedPlanVersion(plan.APIVersion) {
		return model.Plan{}, nil, errors.New("role routing requires a managed plan with an execution catalog")
	}
	if err := ValidateExecutionCatalog(plan.Execution.Catalog); err != nil {
		return model.Plan{}, nil, err
	}
	for role, tiers := range policy.Roles {
		if !ExecutionName.MatchString(role) || len(tiers) == 0 {
			return model.Plan{}, nil, errors.New("invalid or empty role")
		}
		for tier, profile := range tiers {
			if tier != "easy" && tier != "medium" && tier != "hard" {
				return model.Plan{}, nil, fmt.Errorf("invalid difficulty %q", tier)
			}
			if _, ok := plan.Execution.Catalog.Profiles[profile]; !ok || !slices.Contains(plan.Execution.AllowedProfiles, profile) {
				return model.Plan{}, nil, fmt.Errorf("role %s selects unavailable or disallowed profile %s", role, profile)
			}
		}
	}
	known := map[string]bool{}
	for _, task := range plan.Tasks {
		known[task.ID] = true
	}
	for id := range policy.Tasks {
		if !known[id] {
			return model.Plan{}, nil, fmt.Errorf("role policy references unknown task %s", id)
		}
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return model.Plan{}, nil, err
	}
	var routed model.Plan
	if err := json.Unmarshal(raw, &routed); err != nil {
		return model.Plan{}, nil, err
	}
	routes := []profileRoute{}
	for i := range routed.Tasks {
		task := &routed.Tasks[i]
		previous := model.ManagedProfile(routed, *task)
		hint, selected := policy.Tasks[task.ID]
		if selected {
			profile := policy.Roles[hint.Role][hint.Difficulty]
			if profile == "" {
				return model.Plan{}, nil, fmt.Errorf("task %s has no configured role/difficulty route", task.ID)
			}
			task.Profile = profile
		}
		profileName := model.ManagedProfile(routed, *task)
		profile := routed.Execution.Catalog.Profiles[profileName]
		routes = append(routes, profileRoute{Task: task.ID, Role: hint.Role, Difficulty: hint.Difficulty, Previous: previous, Profile: profileName, RequestedModel: profile.Model, OperationBudgetUSD: profile.OperationBudgetUSD})
	}
	return routed, routes, nil
}
