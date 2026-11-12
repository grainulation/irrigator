package graph

import (
	"errors"
	"fmt"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/model"
)

// Estimate-based admission only. Provider reservations remain the actual
// spending limit; these bounds decide whether a revision is worth attempting.
func DefaultAdaptivePolicy() model.AdaptivePolicy {
	return model.AdaptivePolicy{
		MinBenefitMS:        50,
		MinConfidence:       0.6,
		CostPenaltyMSPerUSD: 1000,
		MaxExtraCostUSD:     0.25,
		MaxTotalCostUSD:     5,
		CooldownMS:          1000,
		MaxRevisions:        8,
	}
}

func validateAdaptiveProposalInputs(p model.RevisionProposal) error {
	switch {
	case !checkpoint.IsFiniteFloat(p.ExpectedSavingsMS):
		return errors.New("expected_savings_ms must be finite")
	case !checkpoint.IsFiniteFloat(p.SwitchCostMS) || p.SwitchCostMS < 0:
		return errors.New("switch_cost_ms must be finite and nonnegative")
	case !checkpoint.IsFiniteFloat(p.ExtraCostUSD) || p.ExtraCostUSD < 0:
		return errors.New("extra_cost_usd must be finite and nonnegative")
	case !checkpoint.AdaptiveInRange(p.Confidence, 0, 1):
		return errors.New("confidence must be finite and within 0..1")
	}
	return nil
}

// adaptiveNetBenefitMS is the estimated gain after switching and extra spend
// are converted to milliseconds. Inputs must already be validated as finite.
func adaptiveNetBenefitMS(p model.RevisionProposal, policy model.AdaptivePolicy) float64 {
	return p.ExpectedSavingsMS - p.SwitchCostMS - p.ExtraCostUSD*policy.CostPenaltyMSPerUSD
}

// adaptiveDecision is a pure function of state, proposal, and now. Checks run
// in a fixed order so each rejection or deferral carries one specific reason.
func adaptiveDecision(s *model.State, p model.RevisionProposal, now time.Time) model.AdaptiveDecision {
	d := model.AdaptiveDecision{ProposalID: p.ID, BaseRevision: p.BaseRevision, Time: now, Action: "reject"}
	policy := DefaultAdaptivePolicy()
	if s.Policy != nil {
		policy = *s.Policy
	}
	if err := checkpoint.ValidateAdaptivePolicy(policy); err != nil {
		d.Reason = "invalid policy: " + err.Error()
		return d
	}
	if err := validateAdaptiveProposalInputs(p); err != nil {
		d.Reason = "invalid proposal: " + err.Error()
		return d
	}
	if p.BaseRevision != s.GraphRevision {
		d.Reason = fmt.Sprintf("stale base revision %d; current revision is %d", p.BaseRevision, s.GraphRevision)
		return d
	}
	if p.Confidence < policy.MinConfidence {
		d.Reason = "confidence below minimum"
		return d
	}
	if p.ExtraCostUSD > policy.MaxExtraCostUSD {
		d.Reason = "extra cost exceeds per-proposal maximum"
		return d
	}
	net := adaptiveNetBenefitMS(p, policy)
	if !checkpoint.IsFiniteFloat(net) {
		d.Reason = "net benefit is not finite"
		return d
	}
	d.NetBenefitMS = net
	if net < policy.MinBenefitMS {
		d.Reason = "net benefit below minimum"
		return d
	}
	extra, extraOK := cost.MicrosFromFloat(p.ExtraCostUSD)
	limit, limitOK := cost.Limit(policy.MaxTotalCostUSD)
	if sum, ok := cost.Add(model.AdaptiveTotalMicros(s), extra); !extraOK || !limitOK || !ok || sum > limit {
		d.Reason = "total reported cost with extra cost exceeds maximum"
		return d
	}
	if s.GraphRevision >= policy.MaxRevisions {
		d.Reason = fmt.Sprintf("revision limit %d reached", policy.MaxRevisions)
		return d
	}
	if n := len(s.Revisions); n > 0 {
		cooldown := time.Duration(policy.CooldownMS) * time.Millisecond
		if now.Sub(s.Revisions[n-1].At) < cooldown {
			d.Action = "defer"
			d.Reason = "cooldown active since last revision"
			return d
		}
	}
	d.Action = "apply"
	d.Reason = "accepted"
	return d
}

// recordAdaptiveOutcome attaches a terminal observation to the most recent
// revision if it has none. It records measurements only; it does not assert
// that the revision caused any improvement.
func RecordAdaptiveOutcome(s *model.State, status string, now time.Time) {
	if len(s.Revisions) == 0 {
		return
	}
	last := &s.Revisions[len(s.Revisions)-1]
	if last.Outcome != nil {
		return
	}
	elapsed := now.Sub(last.At).Milliseconds()
	if elapsed < 0 {
		elapsed = 0
	}
	// Archived costs never decrease, so a negative delta means inconsistent
	// input; clamp it rather than report a negative spend.
	atCommit, _ := cost.MicrosFromFloat(last.CostAtCommitUSD)
	delta := model.AdaptiveTotalMicros(s) - atCommit
	if delta < 0 {
		delta = 0
	}
	cost := cost.MicrosToFloat(delta)
	outcome := &model.AdaptiveOutcome{Status: status, ObservedAt: now, ElapsedMS: elapsed, ReportedCostUSD: cost}
	for _, row := range s.Tasks {
		if row == nil {
			continue
		}
		switch row.Status {
		case "verified":
			outcome.VerifiedNodes++
		case "failed":
			outcome.FailedNodes++
		}
	}
	last.Outcome = outcome
}
