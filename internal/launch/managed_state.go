package launch

import (
	"github.com/grainulation/irrigator/internal/model"
)

const ManagedPlanVersion = "swarm/v3"
const UnknownCostPlanVersion = "swarm/v4"

const ArtifactPlanVersion = "swarm/v6"

func SupportsGovernedPlan(v string) bool {
	return v == model.GovernedPlanVersion || v == ArtifactPlanVersion
}

func SupportsUnknownCost(v string) bool {
	return v == UnknownCostPlanVersion || SupportsGovernedPlan(v)
}

func IsManagedPlanVersion(v string) bool {
	return v == ManagedPlanVersion || SupportsUnknownCost(v)
}
