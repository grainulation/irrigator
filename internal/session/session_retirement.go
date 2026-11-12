package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
)

const (
	registryMaxReason  = 160
	registryMaxHistory = 6  // history records kept per entry; older ones are counted in history_dropped
	registryMaxRetired = 16 // retired entries retained; older ones are counted in overflow.retired_evicted
)

var (
	errRegistryUnreadable = errors.New("session registry is unreadable or malformed; it was not modified")
	errRegistryTooLarge   = errors.New("session registry would exceed its 64 KiB bound; it was not modified")
	errRetireNoRegistry   = errors.New("session registry not found")
	errRetireLock         = errors.New("session registry lock is unavailable")
	errRetireBusy         = errors.New("retire refused: a controller or worker lock is held; stop the run (pause or down) and retry")
	errRetireUncertain    = errors.New("retire refused: run liveness or state could not be established; nothing was changed")
	errRetireUnreconciled = errors.New("retire refused: run has running, produced or uncertain nodes; resume or resolve them first")
	errRetireAlready      = errors.New("run is already retired")
	errRetireSuperseder   = errors.New("superseded_by must be a different, registered, non-retired run")
	errRetireWrite        = errors.New("session registry could not be written; run state is unchanged")
)

type registryRetirement struct {
	Reason       string    `json:"reason"`
	RetiredAt    time.Time `json:"retired_at"`
	SupersededBy string    `json:"superseded_by,omitempty"`
}

type registryHistory struct {
	Action       string    `json:"action"` // retired | reactivated
	At           time.Time `json:"at"`
	Reason       string    `json:"reason,omitempty"`
	SupersededBy string    `json:"superseded_by,omitempty"`
}

// retireHoldHook is a test seam called while every lock is held, just before the
// registry write. It is nil in production.
var retireHoldHook func(runPath string)

// unknownDelivery is the honest projection when a run's state cannot be read.
// It is built from JSON so it depends only on the frozen deliveryView JSON names.
func unknownDelivery() graph.DeliveryView {
	var v graph.DeliveryView
	_ = json.Unmarshal([]byte(`{"stage":"unknown","acceptance":"unknown","recovery":"inspect"}`), &v)
	return v
}

func validRegistryReason(s string, required bool) bool {
	if s == "" {
		return !required
	}
	if !utf8.ValidString(s) || len(s) > 4*registryMaxReason || utf8.RuneCountInString(s) > registryMaxReason || s != strings.TrimSpace(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return false
		}
	}
	return true
}

func validRegistryRetention(e registryEntry) bool {
	if r := e.Retirement; r != nil {
		if r.RetiredAt.IsZero() || !validRegistryReason(r.Reason, true) {
			return false
		}
		if r.SupersededBy != "" && (!backup.RegistryRunID.MatchString(r.SupersededBy) || r.SupersededBy == e.RunID) {
			return false
		}
	}
	if len(e.History) > registryMaxHistory || e.HistoryDropped < 0 {
		return false
	}
	for _, h := range e.History {
		if (h.Action != "retired" && h.Action != "reactivated") || h.At.IsZero() || !validRegistryReason(h.Reason, h.Action == "retired") {
			return false
		}
		if h.SupersededBy != "" && !backup.RegistryRunID.MatchString(h.SupersededBy) {
			return false
		}
	}
	return true
}

func appendRegistryHistory(e *registryEntry, h registryHistory) {
	e.History = append(e.History, h)
	for len(e.History) > registryMaxHistory {
		e.History = e.History[1:]
		e.HistoryDropped++
	}
}

// reactivateRegistryEntry is used by publishSession (apply/resume). The retirement
// record stays durable in history; only the visibility flag is cleared.
func reactivateRegistryEntry(e registryEntry, now time.Time) registryEntry {
	if e.Retirement == nil {
		return e
	}
	e.Retirement = nil
	e.History = append([]registryHistory(nil), e.History...)
	appendRegistryHistory(&e, registryHistory{Action: "reactivated", At: now})
	return e
}

// retiredBoundProtected returns runs that carry a durable retirement record but
// whose safe projection is live or unknown. They are not retired for capacity or
// eviction. trusted (if set) is a run the caller already holds stopped and locked.
// Entries are only probed when the raw retirement count exceeds the bound.
func retiredBoundProtected(reg *sessionRegistryV2, trusted string) map[string]bool {
	n := 0
	for _, e := range reg.Entries {
		if e.Retirement != nil {
			n++
		}
	}
	if n <= registryMaxRetired {
		return nil
	}
	protected := map[string]bool{}
	for _, e := range reg.Entries {
		if e.Retirement != nil && e.RunID != trusted && summarizeRegisteredRun(e, false).Visibility != "retired" {
			protected[e.RunID] = true
		}
	}
	return protected
}

// enforceRetiredBoundExcept is enforceRetiredBound with live/unknown runs exempt.
func enforceRetiredBoundExcept(reg *sessionRegistryV2, protected map[string]bool) {
	var idx []int
	for i, e := range reg.Entries {
		if e.Retirement != nil && !protected[e.RunID] {
			idx = append(idx, i)
		}
	}
	if len(idx) <= registryMaxRetired {
		return
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ra, rb := reg.Entries[idx[a]], reg.Entries[idx[b]]
		if !ra.Retirement.RetiredAt.Equal(rb.Retirement.RetiredAt) {
			return ra.Retirement.RetiredAt.Before(rb.Retirement.RetiredAt)
		}
		return ra.RunID < rb.RunID
	})
	drop, remaining := map[int]bool{}, len(idx)
	for _, i := range idx {
		if remaining <= registryMaxRetired {
			break
		}
		if reg.Entries[i].RunID == reg.CurrentRunID {
			continue
		}
		drop[i] = true
		remaining--
		reg.Overflow.RetiredEvicted++
		reg.Overflow.LastRetiredRunID = reg.Entries[i].RunID
	}
	kept := make([]registryEntry, 0, len(reg.Entries))
	for i, e := range reg.Entries {
		if !drop[i] {
			kept = append(kept, e)
		}
	}
	reg.Entries = kept
}

// writeRegistry is the single registry writer used with the registry lock held.
// It keeps the file under the 64 KiB read bound by dropping the oldest history
// records (counted in history_dropped); it never drops entries or retirements.
func writeRegistry(path string, reg *sessionRegistryV2) error {
	for {
		data, err := json.MarshalIndent(reg, "", "  ")
		if err != nil {
			return err
		}
		if len(data) <= registryMaxBytes {
			return jsonio.AtomicJSON(path, reg)
		}
		best := -1
		for i := range reg.Entries {
			if len(reg.Entries[i].History) > 0 && (best < 0 || len(reg.Entries[i].History) > len(reg.Entries[best].History)) {
				best = i
			}
		}
		if best < 0 {
			return errRegistryTooLarge
		}
		reg.Entries[best].History = reg.Entries[best].History[1:]
		reg.Entries[best].HistoryDropped++
	}
}

// applyRetirementView projects durable retirement onto a session summary. A
// retired run that shows live or unknown work is NOT presented as retired: it
// stays active with an explicit note, and the retirement stays in history.
func applyRetirementView(sum panelSessionSummary, e registryEntry) panelSessionSummary {
	sum.Visibility = "active"
	if len(e.History) > 0 {
		sum.History = append([]registryHistory(nil), e.History...)
	}
	if e.Retirement == nil {
		return sum
	}
	live := sum.Available && (sum.ControllerLive || !sum.WorkersKnown || (sum.WorkersActive != nil && *sum.WorkersActive > 0))
	unreadable := !sum.Available && sum.Reason != "state_missing"
	if live || unreadable {
		sum.VisibilityNote = "live_or_unknown_after_retirement"
		return sum
	}
	r := *e.Retirement
	sum.Visibility, sum.Retirement = "retired", &r
	return sum
}

func summarizeRegisteredRun(e registryEntry, withAgents bool) panelSessionSummary {
	return applyRetirementView(summarizeRegisteredRunBase(e, withAgents), e)
}

func ValidateRetireInputs(runID, reason, supersededBy string) (string, error) {
	if !backup.RegistryRunID.MatchString(runID) {
		return "", errors.New("retire requires a registered RUN_ID (letters, digits, _ or -)")
	}
	reason = strings.TrimSpace(reason)
	if !validRegistryReason(reason, true) {
		return "", errors.New("retire requires --reason: 1-160 printable characters without control characters")
	}
	if supersededBy != "" && (!backup.RegistryRunID.MatchString(supersededBy) || supersededBy == runID) {
		return "", errRetireSuperseder
	}
	return reason, nil
}

func releaseRunLocks(held []*os.File) {
	for i := len(held) - 1; i >= 0; i-- {
		checkpoint.Unlock(held[i])
	}
}

// holdStoppedRun takes the controller lock and every node lease without waiting
// and returns them HELD. Any busy lock, unreadable state, run_id mismatch or
// lock error refuses retirement. Nodes still running/produced/uncertain are
// unreconciled work and also refuse. Run state is only read, never written; the
// only side effect is creating an empty lock file if one does not exist yet.
func holdStoppedRun(e registryEntry) ([]*os.File, error) {
	var held []*os.File
	take := func(path string) error {
		f, err := checkpoint.FileLock(path, true)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errRetireBusy
		}
		if err != nil {
			return errRetireUncertain
		}
		held = append(held, f)
		return nil
	}
	fail := func(err error) ([]*os.File, error) {
		releaseRunLocks(held)
		return nil, err
	}
	if err := take(filepath.Join(e.RunPath, "controller.lock")); err != nil {
		return fail(err)
	}
	// With the controller lock held the task set cannot change.
	s, err := checkpoint.ReadState(e.RunPath)
	if err != nil || s.RunID != e.RunID {
		return fail(errRetireUncertain)
	}
	tasks := s.AllTasks()
	for _, t := range tasks {
		if !backup.RegistryRunID.MatchString(t.ID) {
			return fail(errRetireUncertain)
		}
		cell := filepath.Join(e.RunPath, "cells", t.ID)
		if _, serr := os.Lstat(cell); serr != nil {
			if errors.Is(serr, os.ErrNotExist) {
				continue // no cell directory: no worker can hold a lease there
			}
			return fail(errRetireUncertain)
		}
		if err := take(filepath.Join(cell, "lease")); err != nil {
			return fail(err)
		}
	}
	for _, t := range tasks {
		row := s.Tasks[t.ID]
		if row == nil {
			return fail(errRetireUncertain)
		}
		switch row.Status {
		case "running", "produced", "uncertain":
			return fail(errRetireUnreconciled)
		}
	}
	return held, nil
}

// retireSession marks a registered, fully stopped run retired in the registry.
// It changes nothing under the run directory except possibly creating empty lock
// files. A malformed or missing registry is never overwritten.
func RetireSession(path, runID, reason, supersededBy string) (*registryRetirement, error) {
	reason, err := ValidateRetireInputs(runID, reason, supersededBy)
	if err != nil {
		return nil, err
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("an absolute session registry path is required")
	}
	if _, err = os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errRetireNoRegistry
		}
		return nil, errRegistryUnreadable
	}
	lock, err := checkpoint.FileLock(path+".lock", false)
	if err != nil {
		return nil, errRetireLock
	}
	defer checkpoint.Unlock(lock)
	reg, err := ReadRegistryAny(path)
	if err != nil {
		return nil, errRegistryUnreadable
	}
	idx := IndexOfRun(reg.Entries, runID)
	if idx < 0 {
		return nil, errRunNotRegistered
	}
	if reg.Entries[idx].Retirement != nil {
		return nil, errRetireAlready
	}
	if supersededBy != "" {
		si := IndexOfRun(reg.Entries, supersededBy)
		if si < 0 || reg.Entries[si].Retirement != nil {
			return nil, errRetireSuperseder
		}
	}
	held, err := holdStoppedRun(reg.Entries[idx])
	if err != nil {
		return nil, err
	}
	defer releaseRunLocks(held)
	now := time.Now().UTC()
	rec := &registryRetirement{Reason: reason, RetiredAt: now, SupersededBy: supersededBy}
	e := &reg.Entries[idx]
	e.Retirement = rec
	e.History = append([]registryHistory(nil), e.History...)
	appendRegistryHistory(e, registryHistory{Action: "retired", At: now, Reason: reason, SupersededBy: supersededBy})
	reg.Version, reg.UpdatedAt = registryV2Version, now // generation is a start counter and is not bumped
	enforceRetiredBoundExcept(reg, retiredBoundProtected(reg, runID))
	if retireHoldHook != nil {
		retireHoldHook(reg.Entries[IndexOfRun(reg.Entries, runID)].RunPath)
	}
	if err = writeRegistry(path, reg); err != nil {
		if errors.Is(err, errRegistryTooLarge) {
			return nil, err
		}
		return nil, errRetireWrite
	}
	return rec, nil
}
