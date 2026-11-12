package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

const MailboxCapacity = 1024
const mailboxPayloadLimit = 4096

func NewMailboxState() *model.MailboxState {
	return &model.MailboxState{Schema: "irrigator.mailbox/v1", Entries: []model.MailboxEntry{}, Outbox: []model.MailboxOutbox{}}
}
func MailboxDigest(m model.MailboxMessage) string {
	raw, _ := json.Marshal(m)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

var MailboxTaskName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,256}$`)

func ValidateMailboxMessage(m model.MailboxMessage) error {
	if m.Schema != "irrigator.message/v1" || !launch.ExecutionName.MatchString(m.Key) || len(m.RunID) < 1 || len(m.RunID) > 128 || !MailboxTaskName.MatchString(m.TaskID) || m.Generation < 1 || m.Attempt < 0 || len(m.Payload) > mailboxPayloadLimit || !utf8.ValidString(m.Payload) {
		return errors.New("invalid mailbox message schema, identity, fence or payload bound")
	}
	if m.Kind != "signal" && m.Kind != "cancel" {
		return errors.New("unsupported mailbox message kind")
	}
	if len(m.OperationID) != 64 {
		return errors.New("mailbox requires exact operation id")
	}
	if _, err := hex.DecodeString(m.OperationID); err != nil {
		return errors.New("invalid mailbox operation id")
	}
	return nil
}
func MailboxEnabled(s *model.State) bool {
	return launch.SupportsGovernedPlan(s.Plan.APIVersion) && s.Version == ManagedCheckpointVersion(s) && s.Mailbox != nil
}
func ValidateMailboxState(s *model.State) error {
	if !launch.SupportsGovernedPlan(s.Plan.APIVersion) {
		if s.Mailbox != nil {
			return errors.New("mailbox requires swarm/v5")
		}
		return nil
	}
	if !MailboxEnabled(s) {
		return errors.New("swarm/v5 requires checkpoint4 mailbox")
	}
	m := s.Mailbox
	if m.Schema != "irrigator.mailbox/v1" || len(m.Entries) > MailboxCapacity || m.Cursor < 0 || m.Cursor > len(m.Entries) || len(m.Outbox) != m.Cursor {
		return errors.New("invalid mailbox bounds/cursor")
	}
	seen := map[string]bool{}
	for i, e := range m.Entries {
		if e.Sequence != i+1 || seen[e.Message.Key] || e.Digest != MailboxDigest(e.Message) {
			return errors.New("mailbox sequence/dedup/digest corruption")
		}
		if err := ValidateMailboxMessage(e.Message); err != nil {
			return err
		}
		seen[e.Message.Key] = true
		if i >= m.Cursor {
			if e.Receipt != nil {
				return errors.New("receipt beyond mailbox cursor")
			}
			continue
		}
		r := e.Receipt
		if r == nil || (r.Status != "applied" && r.Status != "noop" && r.Status != "stale") || len(r.Detail) == 0 || len(r.Detail) > 256 {
			return errors.New("invalid mailbox receipt")
		}
		o := m.Outbox[i]
		if o.Sequence != e.Sequence || o.Key != e.Message.Key || o.Digest != e.Digest || o.Status != r.Status {
			return errors.New("mailbox transactional outbox mismatch")
		}
	}
	return nil
}
