package checkpoint

import (
	"errors"
	"fmt"
	"math"

	"github.com/grainulation/irrigator/internal/model"
)

// Inclusive upper bounds for validateAdaptivePolicy.
const (
	maxPolicyBenefitMS  = 3600000  // one hour
	maxPolicyPenaltyMS  = 1000000  // penalty per USD
	maxPolicyCostUSD    = 1000     // per-proposal and total cost ceilings
	maxPolicyCooldownMS = 86400000 // one day
	maxPolicyRevisions  = 64
)

func IsFiniteFloat(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func AdaptiveInRange(v, lo, hi float64) bool {
	return IsFiniteFloat(v) && v >= lo && v <= hi
}

func ValidateAdaptivePolicy(p model.AdaptivePolicy) error {
	switch {
	case !AdaptiveInRange(p.MinBenefitMS, 0, maxPolicyBenefitMS):
		return fmt.Errorf("min_benefit_ms must be finite and within 0..%d", maxPolicyBenefitMS)
	case !AdaptiveInRange(p.MinConfidence, 0, 1):
		return errors.New("min_confidence must be finite and within 0..1")
	case !AdaptiveInRange(p.CostPenaltyMSPerUSD, 0, maxPolicyPenaltyMS):
		return fmt.Errorf("cost_penalty_ms_per_usd must be finite and within 0..%d", maxPolicyPenaltyMS)
	case !AdaptiveInRange(p.MaxExtraCostUSD, 0, maxPolicyCostUSD):
		return fmt.Errorf("max_extra_cost_usd must be finite and within 0..%d", maxPolicyCostUSD)
	case !AdaptiveInRange(p.MaxTotalCostUSD, 0, maxPolicyCostUSD):
		return fmt.Errorf("max_total_cost_usd must be finite and within 0..%d", maxPolicyCostUSD)
	case p.CooldownMS < 0 || p.CooldownMS > maxPolicyCooldownMS:
		return fmt.Errorf("cooldown_ms must be within 0..%d", maxPolicyCooldownMS)
	case p.MaxRevisions < 0 || p.MaxRevisions > maxPolicyRevisions:
		return fmt.Errorf("max_revisions must be within 0..%d", maxPolicyRevisions)
	}
	return nil
}
