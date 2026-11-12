package model

// Optional and hash-bound to the admitted plan. Omission preserves old bytes.
// Requirements name narrow runtime contracts, never inferred security properties.
type RuntimeGuardrails struct {
	Schema      string   `json:"schema"`
	Require     []string `json:"require,omitempty"`
	MaxInflight int      `json:"max_inflight,omitempty"`
}
