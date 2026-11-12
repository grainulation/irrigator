package launch

import "github.com/grainulation/irrigator/internal/model"

// hard_token_limit is deliberately narrow: an explicit output-generation cap
// implemented by every selected managed profile. It is not total token billing.
func PlanHasHardOutputTokenLimit(p model.Plan) bool {
	if !IsManagedPlanVersion(p.APIVersion) || p.Execution == nil || len(p.Tasks) == 0 {
		return false
	}
	for _, t := range p.Tasks {
		profile, ok := p.Execution.Catalog.Profiles[model.ManagedProfile(p, t)]
		if !ok || profile.Limits.MaxOutputTokens < 1 || profile.Limits.MaxOutputTokens > OllamaMaxOutputTokens {
			return false
		}
		instance, ok := p.Execution.Catalog.Instances[profile.Instance]
		if !ok {
			return false
		}
		adapter, ok := p.Execution.Catalog.Adapters[instance.Adapter]
		if !ok || adapter.Kind != "ollama_http" || len(profile.Tools) != 0 {
			return false
		}
	}
	return true
}
