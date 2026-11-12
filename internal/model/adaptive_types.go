package model

import "time"

// RevisionProposal describes a complete replacement of the active task snapshot.
// It cannot change the original plan's provider capabilities or named context.
type RevisionProposal struct {
	ID                string    `json:"id"`
	BaseRevision      int       `json:"base_revision"`
	Tasks             []Task    `json:"tasks"`
	Reason            string    `json:"reason"`
	ExpectedSavingsMS float64   `json:"expected_savings_ms"`
	SwitchCostMS      float64   `json:"switch_cost_ms"`
	ExtraCostUSD      float64   `json:"extra_cost_usd"`
	Confidence        float64   `json:"confidence"`
	SubmittedAt       time.Time `json:"submitted_at,omitempty"`
}

// Policy bounds estimated benefit and admission. Provider reservations still
// enforce actual call budgets; model estimates never authorize extra spending.
type AdaptivePolicy struct {
	MinBenefitMS        float64 `json:"min_benefit_ms"`
	MinConfidence       float64 `json:"min_confidence"`
	CostPenaltyMSPerUSD float64 `json:"cost_penalty_ms_per_usd"`
	MaxExtraCostUSD     float64 `json:"max_extra_cost_usd"`
	MaxTotalCostUSD     float64 `json:"max_total_cost_usd"`
	CooldownMS          int64   `json:"cooldown_ms"`
	MaxRevisions        int     `json:"max_revisions"`
}

type AdaptiveDecision struct {
	ProposalID      string    `json:"proposal_id"`
	BaseRevision    int       `json:"base_revision"`
	Time            time.Time `json:"time"`
	Action          string    `json:"action"` // apply, reject, or defer
	Reason          string    `json:"reason"`
	NetBenefitMS    float64   `json:"net_benefit_ms"`
	AppliedRevision int       `json:"applied_revision,omitempty"`
}

type GraphRevisionRecord struct {
	Revision          int              `json:"revision"`
	ParentRevision    int              `json:"parent_revision"`
	ProposalID        string           `json:"proposal_id"`
	At                time.Time        `json:"at"`
	BeforeHash        string           `json:"before_hash"`
	AfterHash         string           `json:"after_hash"`
	TaskCount         int              `json:"task_count,omitempty"`
	RetiredOperations []string         `json:"retired_operations"`
	PredictedGainMS   float64          `json:"predicted_gain_ms"`
	CostAtCommitUSD   float64          `json:"cost_at_commit_usd"`
	Outcome           *AdaptiveOutcome `json:"outcome,omitempty"`
}

type AdaptiveOutcome struct {
	Status          string    `json:"status"`
	ObservedAt      time.Time `json:"observed_at"`
	ElapsedMS       int64     `json:"elapsed_ms"`
	ReportedCostUSD float64   `json:"reported_cost_usd"`
	VerifiedNodes   int       `json:"verified_nodes"`
	FailedNodes     int       `json:"failed_nodes"`
}
