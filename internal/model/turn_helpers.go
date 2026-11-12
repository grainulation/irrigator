package model

// HelperAdmission is immutable local admission evidence, not authentication.
// The run directory's filesystem authority controls who may request work.
type HelperAdmission struct {
	ID          string `json:"id"`
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
	Count       int    `json:"count"`
	RequestHash string `json:"request_hash"`
	SpecHash    string `json:"spec_hash"`
}
