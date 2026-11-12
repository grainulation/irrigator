package model

// ProviderConversation persists explicit prompts and accepted structured outputs,
// not provider-private reasoning or a vendor session ID. It is scoped to one run
// and logical main agent, and committed atomically with the operation receipt.
type ProviderConversation struct {
	RunID       string                      `json:"run_id"`
	AgentID     string                      `json:"agent_id"`
	TaskID      string                      `json:"task_id"`
	Index       int                         `json:"index"`
	ContextHash string                      `json:"context_hash"`
	Entries     []ProviderConversationEntry `json:"entries"`
	Digest      string                      `json:"digest"`
}
type ProviderConversationEntry struct {
	TaskID string            `json:"task_id"`
	Prompt string            `json:"prompt"`
	Output map[string]string `json:"output,omitempty"`
}
