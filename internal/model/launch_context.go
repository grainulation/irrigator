package model

// LaunchCapsule is routing metadata, not a credential or permission grant.
// It deliberately excludes observer bearer tokens, prompts and environment values.
type LaunchCapsule struct {
	Schema            string `json:"schema"`
	SessionID         string `json:"session_id"`
	Workspace         string `json:"workspace"`
	Registry          string `json:"registry"`
	RuntimeVersion    string `json:"runtime_version"`
	RuntimeSHA256     string `json:"runtime_sha256"`
	HeadlessSupported bool   `json:"headless_supported"`
}

type WorkerLaunchContext struct {
	Schema      string         `json:"schema"`
	Capsule     *LaunchCapsule `json:"capsule,omitempty"`
	RunID       string         `json:"run_id"`
	TaskID      string         `json:"task_id"`
	ParentID    string         `json:"parent_id,omitempty"`
	Turn        *TurnIdentity  `json:"turn,omitempty"`
	RequestID   string         `json:"request_id"`
	OperationID string         `json:"operation_id"`
	Attempt     int            `json:"attempt"`
	Kind        string         `json:"kind"`
	ContextKeys []string       `json:"context_keys"`
	InputSHA256 string         `json:"input_sha256"`
	Digest      string         `json:"digest"`
}

type ContextReceipt struct {
	Schema   string `json:"schema"`
	Digest   string `json:"digest"`
	Consumer string `json:"consumer"`
}
