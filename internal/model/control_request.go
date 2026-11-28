package model

// ControlRequestReceipt binds a retryable control request to the graph and, for
// cancellation, the logical cell operation the caller inspected. Receipts are
// retained for the run's lifetime; request IDs are never silently recycled.
type ControlRequestReceipt struct {
	Digest  string `json:"digest"`
	Kind    string `json:"kind"`
	Cell    string `json:"cell,omitempty"`
	Changed bool   `json:"changed"`
}
