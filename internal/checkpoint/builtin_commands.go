package checkpoint

import (
	"fmt"

	"github.com/grainulation/irrigator/internal/model"
)

const (
	// unbudgetedAllow records an explicit --legacy-unbudgeted=allow decision.
	UnbudgetedAllow = "allow"
	// unbudgetedInitial marks a run created by current code without call options;
	// it was never a legacy run, so the gate has nothing to decide.
	UnbudgetedInitial = "initial"
)

func validateUnbudgetedDecision(v string) error {
	switch v {
	case "", UnbudgetedAllow, UnbudgetedInitial:
		return nil
	}
	return fmt.Errorf("unknown unbudgeted_decision %q", v)
}

// potentiallyPaid is deliberately conservative: only a backend that is mock and
// has only mock tasks is truly free. Named command providers may bill and the
// haiku backend always does; later spawned children inherit the backend.
func PotentiallyPaid(backend string, tasks []model.Task) bool {
	if backend != "mock" {
		return true
	}
	for _, t := range tasks {
		if t.Provider != "" && t.Provider != "mock" {
			return true
		}
	}
	return false
}
