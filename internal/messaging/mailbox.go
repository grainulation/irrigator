package messaging

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func admitMailbox(s *model.State, message model.MailboxMessage) (int, error) {
	if !checkpoint.MailboxEnabled(s) {
		return 0, errors.New("mailbox requires admitted swarm/v5 run")
	}
	if err := checkpoint.ValidateMailboxState(s); err != nil {
		return 0, err
	}
	if err := checkpoint.ValidateMailboxMessage(message); err != nil {
		return 0, err
	}
	digest := checkpoint.MailboxDigest(message)
	for _, e := range s.Mailbox.Entries {
		if e.Message.Key == message.Key {
			if e.Digest != digest {
				return 0, errors.New("mailbox deduplication key reused with different payload")
			}
			return e.Sequence, nil
		}
	}
	if len(s.Mailbox.Entries) >= checkpoint.MailboxCapacity {
		return 0, errors.New("mailbox lifetime capacity exhausted; retained tombstones prevent replay")
	}
	seq := len(s.Mailbox.Entries) + 1
	s.Mailbox.Entries = append(s.Mailbox.Entries, model.MailboxEntry{Sequence: seq, Message: message, Digest: digest})
	return seq, checkpoint.ValidateMailboxState(s)
}
func EnqueueMailbox(root string, message model.MailboxMessage) (int, error) {
	seq := 0
	err := checkpoint.Update(root, func(s *model.State) error { var e error; seq, e = admitMailbox(s, message); return e })
	return seq, err
}
func reduceMailbox(s *model.State, limit int) error {
	if limit < 1 || limit > checkpoint.MailboxCapacity {
		return errors.New("mailbox process limit must be 1..1024")
	}
	if !checkpoint.MailboxEnabled(s) {
		return errors.New("mailbox requires admitted swarm/v5 run")
	}
	if err := checkpoint.ValidateMailboxState(s); err != nil {
		return err
	}
	for count := 0; count < limit && s.Mailbox.Cursor < len(s.Mailbox.Entries); count++ {
		index := s.Mailbox.Cursor
		e := &s.Mailbox.Entries[index]
		m := e.Message
		row := s.Tasks[m.TaskID]
		receipt := model.MailboxReceipt{Status: "stale", Detail: "execution fence no longer matches"}
		if m.RunID == s.RunID && row != nil && m.OperationID == row.OperationID && m.Generation == row.Generation && m.Attempt == row.Attempt {
			if m.Kind == "signal" {
				receipt = model.MailboxReceipt{Status: "applied", Detail: "signal retained; no provider context or execution changed"}
			} else if row.Status == "verified" || row.Status == "cancelled" {
				receipt = model.MailboxReceipt{Status: "noop", Detail: "terminal result preserved"}
			} else {
				row.CancelRequested = true
				if row.Status == "pending" || row.Status == "failed" || row.Status == "waiting" {
					row.Status = "cancelled"
				}
				receipt = model.MailboxReceipt{Status: "applied", Detail: "cancellation intent retained; unresolved outcomes remain fenced"}
			}
		}
		e.Receipt = &receipt
		s.Mailbox.Outbox = append(s.Mailbox.Outbox, model.MailboxOutbox{Sequence: e.Sequence, Key: m.Key, Digest: e.Digest, Status: receipt.Status})
		s.Mailbox.Cursor++
	}
	return checkpoint.ValidateMailboxState(s)
}
func ProcessMailbox(root string, limit int) error {
	if limit < 1 || limit > checkpoint.MailboxCapacity {
		return errors.New("mailbox process limit must be 1..1024")
	}
	// Controllers without mailbox state share this hook but never acquire it implicitly.
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	if !launch.SupportsGovernedPlan(s.Plan.APIVersion) {
		return nil
	}
	if s.Mailbox != nil && s.Mailbox.Cursor == len(s.Mailbox.Entries) {
		return checkpoint.ValidateMailboxState(s)
	}
	return checkpoint.Update(root, func(s *model.State) error { return reduceMailbox(s, limit) })
}
func AcknowledgeMailbox(root string, sequence int) error {
	return checkpoint.Update(root, func(s *model.State) error {
		if !checkpoint.MailboxEnabled(s) {
			return errors.New("mailbox requires swarm/v5")
		}
		if err := checkpoint.ValidateMailboxState(s); err != nil {
			return err
		}
		if sequence < 1 || sequence > len(s.Mailbox.Outbox) {
			return errors.New("unknown mailbox outbox sequence")
		}
		s.Mailbox.Outbox[sequence-1].Acknowledged = true
		return checkpoint.ValidateMailboxState(s)
	})
}
func readMailboxMessage(path string) (model.MailboxMessage, error) {
	var m model.MailboxMessage
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return m, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return m, errors.New("message requires regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return m, err
	}
	if len(raw) > 16384 || !utf8.Valid(raw) {
		return m, errors.New("mailbox JSON exceeds byte/encoding bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	v, err := jsonio.ParseStrictJSON(d, 0)
	if err != nil {
		return m, err
	}
	if d.Decode(new(any)) != io.EOF {
		return m, errors.New("trailing mailbox JSON")
	}
	if err = launch.ExecutionJSONShape(v, reflect.TypeOf(m)); err != nil {
		return m, err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, err
	}
	return m, checkpoint.ValidateMailboxMessage(m)
}
func MailboxCLI(args []string) (int, error) {
	if err := commandflags.CommandFlagMetadata(flag.NewFlagSet("mailboxCLI", flag.ContinueOnError), args); err != nil {
		return 2, err
	}
	usage := errors.New("usage: irrigator mailbox enqueue RUN MESSAGE.json | process RUN [LIMIT] | outbox RUN | ack RUN SEQUENCE")
	if len(args) < 2 {
		return 2, usage
	}
	root := args[1]
	switch args[0] {
	case "enqueue":
		if len(args) != 3 {
			return 2, usage
		}
		m, err := readMailboxMessage(args[2])
		if err != nil {
			return 1, err
		}
		seq, err := EnqueueMailbox(root, m)
		if err != nil {
			return 1, err
		}
		fmt.Printf("{\"sequence\":%d}\n", seq)
	case "process":
		if len(args) > 3 {
			return 2, usage
		}
		limit := 64
		if len(args) == 3 {
			var err error
			limit, err = strconv.Atoi(args[2])
			if err != nil {
				return 1, err
			}
		}
		s, err := checkpoint.ReadState(root)
		if err != nil {
			return 1, err
		}
		if !checkpoint.MailboxEnabled(s) {
			return 1, errors.New("mailbox requires swarm/v5")
		}
		if err = ProcessMailbox(root, limit); err != nil {
			return 1, err
		}
	case "outbox":
		if len(args) != 2 {
			return 2, usage
		}
		s, err := checkpoint.ReadState(root)
		if err != nil {
			return 1, err
		}
		if !checkpoint.MailboxEnabled(s) {
			return 1, errors.New("mailbox requires swarm/v5")
		}
		out := []model.MailboxOutbox{}
		for _, o := range s.Mailbox.Outbox {
			if !o.Acknowledged {
				out = append(out, o)
			}
		}
		raw, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(raw))
	case "ack":
		if len(args) != 3 {
			return 2, usage
		}
		seq, err := strconv.Atoi(strings.TrimSpace(args[2]))
		if err != nil {
			return 1, err
		}
		if err = AcknowledgeMailbox(root, seq); err != nil {
			return 1, err
		}
	default:
		return 2, usage
	}
	return 0, nil
}
