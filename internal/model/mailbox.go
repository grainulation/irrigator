package model

type MailboxMessage struct {
	Schema      string `json:"schema"`
	Key         string `json:"key"`
	RunID       string `json:"run_id"`
	TaskID      string `json:"task_id"`
	OperationID string `json:"operation_id"`
	Generation  int    `json:"generation"`
	Attempt     int    `json:"attempt"`
	Kind        string `json:"kind"`
	Payload     string `json:"payload"`
}
type MailboxReceipt struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}
type MailboxEntry struct {
	Sequence int             `json:"sequence"`
	Message  MailboxMessage  `json:"message"`
	Digest   string          `json:"digest"`
	Receipt  *MailboxReceipt `json:"receipt,omitempty"`
}
type MailboxOutbox struct {
	Sequence     int    `json:"sequence"`
	Key          string `json:"key"`
	Digest       string `json:"digest"`
	Status       string `json:"status"`
	Acknowledged bool   `json:"acknowledged"`
}
type MailboxState struct {
	Schema  string          `json:"schema"`
	Entries []MailboxEntry  `json:"entries"`
	Cursor  int             `json:"cursor"`
	Outbox  []MailboxOutbox `json:"outbox"`
}
