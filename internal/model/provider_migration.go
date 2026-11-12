package model

import (
	"time"
)

type ProviderMigrationProposal struct {
	Schema          string           `json:"schema"`
	RunID           string           `json:"run_id"`
	PlanHash        string           `json:"plan_hash"`
	BaseStateDigest string           `json:"base_state_digest"`
	Sequence        int              `json:"sequence"`
	Profile         string           `json:"profile"`
	FromBinding     string           `json:"from_binding"`
	Replacement     ExecutionBinding `json:"replacement"`
	PendingTasks    []string         `json:"pending_tasks"`
	Digest          string           `json:"digest"`
}
type ProviderMigrationRecord struct {
	Proposal         ProviderMigrationProposal `json:"proposal"`
	ApprovedAt       time.Time                 `json:"approved_at"`
	ConfirmedStopped bool                      `json:"confirmed_stopped"`
}

func ManagedCurrentBinding(s *State, profile string) ExecutionBinding {
	b := s.Execution.Bindings[profile]
	for _, m := range s.Execution.Migrations {
		if m.Proposal.Profile == profile {
			b = m.Proposal.Replacement
		}
	}
	return b
}
