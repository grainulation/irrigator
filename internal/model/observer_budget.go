package model

// providerAttribution is recorded only on NEW receipts and only when the actual
// WorkRequest's OperationID verifies against run/task/generation.
type ProviderAttribution struct {
	RunID      string `json:"run_id"`
	TaskID     string `json:"task_id"`
	Generation int    `json:"generation"`
}
