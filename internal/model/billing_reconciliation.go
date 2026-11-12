package model

// BillingReceipt is an explicit operator attestation, never a provider response.
// The complete bounded statement is retained in the transactional checkpoint.
type BillingReceipt struct {
	Schema            string `json:"schema"`
	RunID             string `json:"run_id"`
	TaskID            string `json:"task_id"`
	OperationID       string `json:"operation_id"`
	Attempt           int    `json:"attempt"`
	BindingDigest     string `json:"binding_digest"`
	ReceiptID         string `json:"receipt_id"`
	Currency          string `json:"currency"`
	AmountMicros      int64  `json:"amount_micros"`
	EvidenceSHA256    string `json:"evidence_sha256"`
	EvidenceReference string `json:"evidence_reference"`
	Attestation       string `json:"attestation"`
}
