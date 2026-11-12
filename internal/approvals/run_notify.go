package approvals

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
)

const (
	notifyRecordName = "notify.json"
	notifySentName   = "notify-sent.json"
	NotifyLogName    = "notify.log"
)

var (
	runNotifySendTimeout  = 5 * time.Second
	runNotifyFlushTimeout = 3 * time.Second
)

// ActiveNotify holds the --notify flags of the controller this process starts.
var ActiveNotify RunNotifyFlags

type RunNotifyFlags struct {
	Via   string // ntfy, webhook or off; empty defers to the run record and env
	Topic string // --notify-topic
	URL   string // --notify-url
}

type runNotifyRecord struct {
	Via    string `json:"via"`
	Server string `json:"server,omitempty"`
	Topic  string `json:"topic,omitempty"`
	URL    string `json:"url,omitempty"`
}

func ValidNotifyVia(via, source string) error {
	switch via {
	case "ntfy", "webhook", "off":
		return nil
	}
	return fmt.Errorf("%s %q is not a known notifier; use ntfy, webhook or off", source, via)
}

func readRunNotifyRecord(root string) (*runNotifyRecord, error) {
	raw, err := os.ReadFile(filepath.Join(root, notifyRecordName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec runNotifyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("unreadable %s: %w", notifyRecordName, err)
	}
	if err := ValidNotifyVia(rec.Via, "recorded notifier"); err != nil {
		return nil, err
	}
	return &rec, nil
}

// chooseRunNotify applies the precedence: explicit flag > the run's recorded
// choice > IRRIGATOR_NOTIFY_VIA > off. record reports that the result should
// be written to the run (it came from a flag or the environment).
func chooseRunNotify(root string, f RunNotifyFlags) (rec runNotifyRecord, record bool, err error) {
	recorded, err := readRunNotifyRecord(root)
	if err != nil {
		return rec, false, err
	}
	switch {
	case f.Via != "":
		if err = ValidNotifyVia(f.Via, "--notify"); err != nil {
			return rec, false, err
		}
		rec.Via, record = f.Via, true
	case recorded != nil:
		return *recorded, false, nil
	case os.Getenv("IRRIGATOR_NOTIFY_VIA") != "":
		via := os.Getenv("IRRIGATOR_NOTIFY_VIA")
		if err = ValidNotifyVia(via, "IRRIGATOR_NOTIFY_VIA"); err != nil {
			return rec, false, err
		}
		rec.Via, record = via, true
	default:
		return runNotifyRecord{Via: "off"}, false, nil
	}
	// Only values given as flags are recorded; the environment is read again
	// on every start so a token or secret topic never has to be stored.
	if recorded != nil && recorded.Via == rec.Via {
		rec.Server, rec.Topic, rec.URL = recorded.Server, recorded.Topic, recorded.URL
	}
	if f.Topic != "" {
		rec.Topic = f.Topic
	}
	if f.URL != "" {
		rec.URL = f.URL
	}
	return rec, record, nil
}

func (rec runNotifyRecord) notifier(stderr io.Writer) (approvalNotifier, error) {
	switch rec.Via {
	case "ntfy":
		return newNtfyNotifierAt(rec.Server, rec.Topic, stderr)
	case "webhook":
		return newWebhookNotifier(rec.URL)
	}
	return nil, nil
}

// newRunNotifier resolves, validates and records the notifier for a run. It
// returns nil when notifications are off. A bad explicit flag is an error; a
// bad recorded or environment choice only disables notifications, so a
// misconfigured channel cannot stop a run.
func NewRunNotifier(root string, f RunNotifyFlags, stderr io.Writer) (*runNotifier, error) {
	rec, record, err := chooseRunNotify(root, f)
	if err != nil {
		return nil, softNotifyError(f, err, stderr)
	}
	var n approvalNotifier
	if rec.Via != "off" {
		if n, err = rec.notifier(stderr); err != nil {
			return nil, softNotifyError(f, err, stderr)
		}
	}
	if record {
		if err := jsonio.AtomicJSON(filepath.Join(root, notifyRecordName), rec); err != nil {
			return nil, err
		}
	}
	if n == nil {
		return nil, nil
	}
	sent, err := readNotifySent(root)
	if err != nil {
		return nil, softNotifyError(f, err, stderr)
	}
	return &runNotifier{root: root, n: n, stderr: stderr, sent: sent}, nil
}

// softNotifyError returns err for an explicit flag and otherwise reports it
// and lets the run go on without notifications.
func softNotifyError(f RunNotifyFlags, err error, stderr io.Writer) error {
	if f.Via != "" {
		return err
	}
	fmt.Fprintf(stderr, "notify: disabled: %v\n", err)
	return nil
}

func readNotifySent(root string) (map[string]bool, error) {
	sent := map[string]bool{}
	raw, err := os.ReadFile(filepath.Join(root, notifySentName))
	if errors.Is(err, os.ErrNotExist) {
		return sent, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Sent []string `json:"sent"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("unreadable %s: %w", notifySentName, err)
	}
	for _, k := range doc.Sent {
		sent[k] = true
	}
	return sent, nil
}

type runNotifier struct {
	root   string
	n      approvalNotifier
	stderr io.Writer
	sent   map[string]bool // owned by the controller goroutine
	wg     sync.WaitGroup
	logMu  sync.Mutex
}

type runTransition struct {
	key    string
	notice approvalNotice
}

// runNotifyTransitions lists the notices s calls for. A key identifies one
// transition: a cell attempt reaching a status, or one controller episode
// (counted by "resumed" events) ending blocked or complete. Messages carry
// names, counts, a status word and the run cost total; never prompts, outputs
// or error text.
func runNotifyTransitions(root string, s *model.State) []runTransition {
	name := filepath.Base(root)
	verified, failed, uncertain := 0, 0, 0
	ids := make([]string, 0, len(s.Tasks))
	for id, row := range s.Tasks {
		ids = append(ids, id)
		switch row.Status {
		case "verified":
			verified++
		case "failed":
			failed++
		case "uncertain":
			uncertain++
		}
	}
	sort.Strings(ids)
	counts := fmt.Sprintf("%d/%d verified", verified, len(s.Tasks))
	if failed > 0 {
		counts += fmt.Sprintf(", %d failed", failed)
	}
	if uncertain > 0 {
		counts += fmt.Sprintf(", %d uncertain", uncertain)
	}
	tail := fmt.Sprintf("%s. %s. Inspect: irrigator status %s", counts, provider.CostStatusText(s), shellQuote(root))
	var out []runTransition
	for _, id := range ids {
		row := s.Tasks[id]
		if row.Status != "uncertain" && row.Status != "failed" {
			continue
		}
		out = append(out, runTransition{
			key: fmt.Sprintf("cell:%s:%s:%d:%d", id, row.Status, row.Generation, row.Attempt),
			notice: approvalNotice{Run: s.RunID, Request: id, Kind: "cell_" + row.Status,
				Title:   fmt.Sprintf("Irrigator %s: cell %s %s", name, id, row.Status),
				Message: fmt.Sprintf("Run %s: cell %s is %s. %s", name, id, row.Status, tail)},
		})
	}
	if s.Control == "blocked" || s.Control == "complete" {
		// Key on what the cells look like, not on how often the run was
		// resumed: resuming a finished run must not repeat its notice, while a
		// new attempt that ends blocked again must notify.
		h := sha256.New()
		for _, id := range ids {
			row := s.Tasks[id]
			fmt.Fprintf(h, "%s:%s:%d:%d\n", id, row.Status, row.Generation, row.Attempt)
		}
		out = append(out, runTransition{
			key: fmt.Sprintf("run:%s:%x", s.Control, h.Sum(nil)[:8]),
			notice: approvalNotice{Run: s.RunID, Kind: "run_" + s.Control,
				Title:   fmt.Sprintf("Irrigator %s: run %s", name, s.Control),
				Message: fmt.Sprintf("Run %s is %s. %s", name, s.Control, tail)},
		})
	}
	return out
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:@%+=,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// observe announces the transitions in s that were not announced before. It
// runs on the controller goroutine, but only does a small local write; the
// network send happens in a goroutine. A transition is recorded before its
// send, so a crash or failure never produces a repeat (at most once).
func (r *runNotifier) Observe(s *model.State) {
	if r == nil {
		return
	}
	var fresh []runTransition
	for _, t := range runNotifyTransitions(r.root, s) {
		if !r.sent[t.key] {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) == 0 {
		return
	}
	for _, t := range fresh {
		r.sent[t.key] = true
	}
	if err := r.persistSent(); err != nil {
		// Without a durable record a restart could repeat the message; skip it.
		for _, t := range fresh {
			delete(r.sent, t.key)
		}
		r.logFailure("cannot record sent notifications: " + err.Error())
		return
	}
	for _, t := range fresh {
		r.wg.Add(1)
		go func(n approvalNotice) {
			defer r.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), runNotifySendTimeout)
			defer cancel()
			if err := r.n.Notify(ctx, n); err != nil {
				r.logFailure(fmt.Sprintf("%s notification (%s) failed: %v", r.n.Name(), n.Kind, err))
			}
		}(t.notice)
	}
}

func (r *runNotifier) persistSent() error {
	keys := make([]string, 0, len(r.sent))
	for k := range r.sent {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return jsonio.AtomicJSON(filepath.Join(r.root, notifySentName), map[string][]string{"sent": keys})
}

func (r *runNotifier) logFailure(msg string) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintln(r.stderr, "notify: "+msg)
	f, err := os.OpenFile(filepath.Join(r.root, NotifyLogName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), msg)
}

// flush gives in-flight sends a few seconds before the controller exits, so
// the final complete or blocked message is not lost to process exit.
func (r *runNotifier) Flush() {
	if r == nil {
		return
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(runNotifyFlushTimeout):
	}
}
