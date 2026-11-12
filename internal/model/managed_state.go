package model

const GovernedPlanVersion = "swarm/v5"

type ManagedExecutionPlan struct {
	Catalog         ExecutionCatalog `json:"catalog"`
	AllowedProfiles []string         `json:"allowed_profiles"`
	CostPolicy      string           `json:"cost_policy,omitempty"`
	RunBudgetUSD    string           `json:"run_budget_usd"`
}
type ManagedReservation struct {
	TaskID         string `json:"task_id"`
	OperationID    string `json:"operation_id"`
	Attempt        int    `json:"attempt"`
	BindingDigest  string `json:"binding_digest"`
	ReservedMicros int64  `json:"reserved_micros"`
	ReportedMicros int64  `json:"reported_micros"`
	Status         string `json:"status"`
}
type ManagedExecutionState struct {
	Migrations      []ProviderMigrationRecord     `json:"provider_migrations,omitempty"`
	BillingRegistry string                        `json:"billing_registry,omitempty"`
	BillingReceipts map[string]BillingReceipt     `json:"billing_receipts,omitempty"`
	Bindings        map[string]ExecutionBinding   `json:"bindings"`
	CeilingMicros   int64                         `json:"ceiling_micros"`
	Reservations    map[string]ManagedReservation `json:"reservations"`
	Frozen          bool                          `json:"frozen"`
	FreezeReason    string                        `json:"freeze_reason,omitempty"`
}

func ManagedProfile(p Plan, task Task) string {
	if task.Profile != "" {
		return task.Profile
	}
	if p.Execution != nil {
		return p.Execution.Catalog.DefaultProfile
	}
	return ""
}
