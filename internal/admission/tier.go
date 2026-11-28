package admission

import (
	"fmt"

	"github.com/grainulation/irrigator/internal/model"
)

// ParseTier reads a tier name.
func ParseTier(name string) (Tier, error) {
	switch name {
	case "minimal":
		return TierMinimal, nil
	case "reduced":
		return TierReduced, nil
	case "full":
		return TierFull, nil
	}
	return 0, fmt.Errorf("model tier %q must be minimal, reduced or full", name)
}

// ProfileTiers returns a tier lookup from an operator-declared map of managed
// profile name to tier. It never guesses a tier from a model name: a task whose
// profile is not in the map declares no tier and is never filtered by tier, so
// the feature is strictly opt-in and rebinds nothing. A tier is a ceiling: when
// the environment allows at most TierReduced, a task declared full waits and a
// task declared reduced or minimal may start.
func ProfileTiers(byProfile map[string]Tier) func(s *model.State, id string) (Tier, bool) {
	if len(byProfile) == 0 {
		return nil
	}
	return func(s *model.State, id string) (Tier, bool) {
		row := s.Tasks[id]
		if row == nil {
			return 0, false
		}
		t, ok := byProfile[model.ManagedProfile(s.Plan, row.Spec)]
		return t, ok
	}
}
