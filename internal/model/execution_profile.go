package model

type ExecutionAdapter struct {
	Kind             string   `json:"kind"`
	Endpoint         string   `json:"endpoint,omitempty"`
	Protocol         string   `json:"protocol"`
	Command          []string `json:"command,omitempty"`
	ExecutableSHA256 string   `json:"executable_sha256,omitempty"`
}
type ExecutionCredentialRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}
type ExecutionInstance struct {
	Adapter        string                   `json:"adapter"`
	CredentialRefs []ExecutionCredentialRef `json:"credential_refs"`
}
type ExecutionLimits struct {
	InputBytes  int64 `json:"input_bytes"`
	OutputBytes int64 `json:"output_bytes"`
	// MaxOutputTokens is an optional provider-enforced generation cap. Zero
	// uses the adapter default; only local Ollama supports it.
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}
type ExecutionProfile struct {
	Instance             string          `json:"instance"`
	Model                string          `json:"model"`
	ModelDigest          string          `json:"model_digest,omitempty"`
	Effort               string          `json:"effort"`
	RequiredCapabilities []string        `json:"required_capabilities"`
	Limits               ExecutionLimits `json:"limits"`
	Tools                []string        `json:"tools"`
	OperationBudgetUSD   string          `json:"operation_budget_usd"`
	TimeoutSeconds       int             `json:"timeout_seconds,omitempty"`
}
type ExecutionCatalog struct {
	Schema         string                       `json:"schema"`
	Adapters       map[string]ExecutionAdapter  `json:"adapters"`
	Instances      map[string]ExecutionInstance `json:"instances"`
	Profiles       map[string]ExecutionProfile  `json:"profiles"`
	DefaultProfile string                       `json:"default_profile"`
}

// ExecutionBinding is a read-only resolution result, not an execution grant.
// Consumers must reverify identities before future admission/dispatch integration.
type ExecutionBinding struct {
	Schema                string            `json:"schema"`
	CatalogPath           string            `json:"catalog_path"`
	CatalogSHA256         string            `json:"catalog_sha256"`
	ProfileName           string            `json:"profile_name"`
	Profile               ExecutionProfile  `json:"profile"`
	InstanceName          string            `json:"instance_name"`
	Instance              ExecutionInstance `json:"instance"`
	AdapterName           string            `json:"adapter_name"`
	Adapter               ExecutionAdapter  `json:"adapter"`
	SupportedCapabilities []string          `json:"supported_capabilities"`
	CapabilityEvidence    string            `json:"capability_evidence"`
	ExecutableVerified    bool              `json:"executable_verified"`
	ActualModel           *string           `json:"actual_model"`
	ModelResolution       string            `json:"model_resolution"`
	AuthVerified          bool              `json:"auth_verified"`
	Dispatched            bool              `json:"dispatched"`
	OperationBudgetMicros int64             `json:"operation_budget_micros"`
	Digest                string            `json:"digest"`
}
