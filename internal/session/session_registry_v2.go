package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
)

const (
	registryV2Version = 2
	registryMaxBytes  = 64 * 1024
	registryMaxRecent = 16
	registryMaxActive = 32
	registryMaxFile   = 64 // hard parse bound for entries
	panelMaxAgents    = 32
	panelMaxEvents    = 64 // per-session serialized event tail
)

var errRegistryActiveFull = errors.New("session registry active set is full; run not registered (overflow recorded)")
var errRunNotRegistered = errors.New("run is not registered")

type registryEntry struct {
	RunID          string              `json:"run_id"`
	RunPath        string              `json:"run_path"`
	StartedAt      time.Time           `json:"started_at"`
	LastStartedAt  time.Time           `json:"last_started_at"`
	Generation     int                 `json:"generation"`
	Retirement     *registryRetirement `json:"retirement,omitempty"`
	History        []registryHistory   `json:"history,omitempty"`
	HistoryDropped int                 `json:"history_dropped,omitempty"`
}

type registryOverflow struct {
	RecentEvicted     int    `json:"recent_evicted"`
	LastEvictedRunID  string `json:"last_evicted_run_id,omitempty"`
	ActiveRejected    int    `json:"active_rejected"`
	LastRejectedRunID string `json:"last_rejected_run_id,omitempty"`
	RetiredEvicted    int    `json:"retired_evicted,omitempty"`
	LastRetiredRunID  string `json:"last_retired_evicted_run_id,omitempty"`
}

type sessionRegistryV2 struct {
	Version      int              `json:"version"`
	Generation   int              `json:"generation"`
	CurrentRunID string           `json:"current_run_id"`
	UpdatedAt    time.Time        `json:"updated_at"`
	Entries      []registryEntry  `json:"entries"`
	Overflow     registryOverflow `json:"overflow"`
	Migrated     bool             `json:"-"` // true when read from a v1 file
}

func validRegistryEntry(e registryEntry) bool {
	return backup.RegistryRunID.MatchString(e.RunID) && filepath.IsAbs(e.RunPath) && filepath.Clean(e.RunPath) == e.RunPath && !e.StartedAt.IsZero()
}

// readRegistryAny reads v1 (as a one-entry registry) or v2. It never follows
// symlinks, rejects unknown fields and bounds size and entry count.
func ReadRegistryAny(path string) (*sessionRegistryV2, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("registry must be a regular non-symlink file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, registryMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > registryMaxBytes {
		return nil, errors.New("registry exceeds 64 KiB")
	}
	var head struct {
		Version int `json:"version"`
	}
	if err = json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("malformed registry: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	switch head.Version {
	case 1:
		var r sessionRegistry
		if err = d.Decode(&r); err != nil {
			return nil, fmt.Errorf("malformed registry: %w", err)
		}
		if d.Decode(new(any)) != io.EOF {
			return nil, errors.New("registry must contain one JSON object")
		}
		e := registryEntry{RunID: r.RunID, RunPath: r.RunPath, StartedAt: r.StartedAt, LastStartedAt: r.StartedAt, Generation: r.Generation}
		if !validRegistryEntry(e) || r.Generation < 1 {
			return nil, errors.New("invalid registry v1")
		}
		return &sessionRegistryV2{Version: 1, Generation: r.Generation, CurrentRunID: r.RunID, UpdatedAt: r.StartedAt, Entries: []registryEntry{e}, Migrated: true}, nil
	case registryV2Version:
		var r sessionRegistryV2
		if err = d.Decode(&r); err != nil {
			return nil, fmt.Errorf("malformed registry: %w", err)
		}
		if d.Decode(new(any)) != io.EOF {
			return nil, errors.New("registry must contain one JSON object")
		}
		if r.Generation < 1 || len(r.Entries) == 0 || len(r.Entries) > registryMaxFile {
			return nil, errors.New("invalid registry v2")
		}
		ids, paths, hasCurrent := map[string]bool{}, map[string]bool{}, false
		for _, e := range r.Entries {
			if !validRegistryEntry(e) || !validRegistryRetention(e) || ids[e.RunID] || paths[e.RunPath] {
				return nil, errors.New("invalid registry v2 entry")
			}
			ids[e.RunID], paths[e.RunPath] = true, true
			hasCurrent = hasCurrent || e.RunID == r.CurrentRunID
		}
		if !hasCurrent {
			return nil, errors.New("registry current_run_id is not an entry")
		}
		return &r, nil
	}
	return nil, fmt.Errorf("unsupported registry version %d", head.Version)
}

// probeLock is a read-only, non-creating, non-waiting probe of a flock file.
func ProbeLock(path string) (bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	e := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(e, syscall.EWOULDBLOCK) {
		return true, nil
	}
	if e != nil {
		return false, e
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

type panelSessionAgent struct {
	ID      string `json:"id"`
	Parent  string `json:"parent,omitempty"`
	Status  string `json:"status"`
	Attempt int    `json:"attempt"`
}

type panelSessionSummary struct {
	RunID           string                  `json:"run_id"`
	RunName         string                  `json:"run_name,omitempty"`
	Status          string                  `json:"status"`
	Control         string                  `json:"control,omitempty"`
	Backend         string                  `json:"backend,omitempty"`
	Available       bool                    `json:"available"`
	Reason          string                  `json:"unavailable_reason,omitempty"`
	Current         bool                    `json:"current"`
	Selected        bool                    `json:"selected"`
	StartedAt       time.Time               `json:"started_at"`
	LastStartedAt   time.Time               `json:"last_started_at"`
	Generation      int                     `json:"generation"`
	Revision        int                     `json:"revision"`
	NodesTotal      int                     `json:"nodes_total"`
	StatusCounts    map[string]int          `json:"status_counts"`
	WorkersActive   *int                    `json:"workers_active"`
	WorkersKnown    bool                    `json:"workers_known"`
	ControllerLive  bool                    `json:"controller_live"`
	EventsTotal     int                     `json:"events_total"`
	Events          []PanelEvent            `json:"events"`
	EventsFloor     int                     `json:"events_floor"`
	EventsWatermark int                     `json:"events_watermark"`
	Agents          []panelSessionAgent     `json:"agents"`
	AgentsTotal     int                     `json:"agents_total"`
	AgentsTruncated bool                    `json:"agents_truncated"`
	Capacity        *observer.PanelCapacity `json:"capacity,omitempty"`
	Topology        *observer.PanelTopology `json:"topology,omitempty"`
	Delivery        graph.DeliveryView      `json:"delivery"`
	Visibility      string                  `json:"visibility"`
	Retirement      *registryRetirement     `json:"retirement,omitempty"`
	VisibilityNote  string                  `json:"visibility_note,omitempty"`
	History         []registryHistory       `json:"history,omitempty"`
}

func (m panelSessionSummary) registryActive() bool {
	if m.Visibility == "retired" {
		return false
	}
	switch m.Status {
	case "complete", "blocked", "inconsistent", "missing":
		return false
	}
	return true
}

// deriveRunStatus uses only durable state and lock probes, never chat text.
func deriveRunStatus(control string, allVerified bool, workers int, known, controllerLive bool) string {
	if !known {
		return "indeterminate"
	}
	if workers > 0 {
		return "running"
	}
	switch control {
	case "complete":
		if allVerified {
			return "complete"
		}
		return "inconsistent"
	case "blocked":
		return "blocked"
	case "pause", "down":
		return "paused"
	case "run":
		if controllerLive {
			return "running"
		}
		return "stalled"
	case "ready":
		return "ready"
	}
	return "indeterminate"
}

// recentPanelEvents returns the last max safe panelEvents of s. Seq values are
// the original 1-based indexes into s.Events, so they remain stable across reads.
func recentPanelEvents(s *model.State, max int) []PanelEvent {
	evs := PanelEvents(s)
	if len(evs) > max {
		evs = evs[len(evs)-max:]
	}
	return evs
}

func summarizeRegisteredRunBase(e registryEntry, withAgents bool) panelSessionSummary {
	sum := panelSessionSummary{RunID: e.RunID, StartedAt: e.StartedAt, LastStartedAt: e.LastStartedAt, Generation: e.Generation,
		Status: "unavailable", StatusCounts: map[string]int{}, Events: []PanelEvent{}, Agents: []panelSessionAgent{}, Delivery: unknownDelivery(), Visibility: "active"}
	s, err := checkpoint.ReadState(e.RunPath)
	if err != nil {
		sum.Reason = "state_unreadable"
		if os.IsNotExist(err) {
			sum.Reason, sum.Status = "state_missing", "missing"
		}
		return sum
	}
	if s.RunID != e.RunID {
		sum.Reason = "run_id_mismatch"
		return sum
	}
	sum.Available = true
	sum.RunName, sum.Control, sum.Backend, sum.Revision = registryRunName(s.Plan.Name), s.Control, s.Backend, s.GraphRevision
	sum.EventsTotal = len(s.Events)
	sum.EventsWatermark = len(s.Events)
	if withAgents {
		// Full projection only when agents are requested; capacity probes skip it.
		sum.Events = recentPanelEvents(s, panelMaxEvents)
		sum.Delivery = graph.ProjectDelivery(e.RunPath, s)
		sum.Capacity, sum.Topology = observer.ProjectCapacity(s), observer.ProjectTopology(s)
		if len(sum.Events) > 0 {
			sum.EventsFloor = sum.Events[0].Seq
		}
	}
	tasks := s.AllTasks()
	sum.NodesTotal, sum.AgentsTotal = len(tasks), len(tasks)
	active, known, all := 0, true, true
	for _, t := range tasks {
		row := s.Tasks[t.ID]
		sum.StatusCounts[row.Status]++
		all = all && row.Status == "verified"
		if held, perr := ProbeLock(filepath.Join(e.RunPath, "cells", t.ID, "lease")); perr != nil {
			known = false
		} else if held {
			active++
		}
		if withAgents {
			if len(sum.Agents) >= panelMaxAgents {
				sum.AgentsTruncated = true
			} else {
				sum.Agents = append(sum.Agents, panelSessionAgent{ID: t.ID, Parent: t.Parent, Status: row.Status, Attempt: row.Attempt})
			}
		}
	}
	live, cerr := ProbeLock(filepath.Join(e.RunPath, "controller.lock"))
	if cerr != nil {
		known = false
	}
	sum.ControllerLive, sum.WorkersKnown = live, known
	if known {
		sum.WorkersActive = &active
	}
	sum.Status = deriveRunStatus(s.Control, all, active, known, live)
	return sum
}

func IndexOfRun(entries []registryEntry, id string) int {
	for i, e := range entries {
		if e.RunID == id {
			return i
		}
	}
	return -1
}

func sortEntries(es []registryEntry) {
	sort.SliceStable(es, func(i, j int) bool {
		if !es[i].LastStartedAt.Equal(es[j].LastStartedAt) {
			return es[i].LastStartedAt.After(es[j].LastStartedAt)
		}
		return es[i].RunID < es[j].RunID
	})
}

// publishSession is the locked, atomic v2 upsert used by apply/resume. It
// retains every active run, bounds recent runs to 16, migrates v1 preserving
// its run, and never replaces other entries with a latest pointer.
//
// Every admitted start becomes active, including a resume of a recent run and
// an idempotent same-current restart. The capacity check counts the OTHER active
// runs (the target is excluded) and runs before the same-current early return.
func PublishSession(path, root, runID string) error {
	if path == "" {
		return nil
	}
	if !backup.RegistryRunID.MatchString(runID) {
		return errors.New("invalid run id")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := checkpoint.FileLock(path+".lock", false)
	if err != nil {
		return err
	}
	defer checkpoint.Unlock(lock)
	reg, err := ReadRegistryAny(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w at %q; inspect that file or choose a different --session-registry path", errRegistryUnreadable, path)
		}
		reg = &sessionRegistryV2{Version: registryV2Version} // only an ABSENT registry starts fresh; a malformed one fails closed
	}
	idx := IndexOfRun(reg.Entries, runID)
	if idx >= 0 && reg.Entries[idx].RunPath != abs {
		return errors.New("run_id already registered for a different path")
	}
	now := time.Now().UTC()
	var active, recent, retired []registryEntry
	// protected holds runs with a durable retirement record whose safe projection
	// is live or unknown: they count as active and are never retired-evicted.
	protected := map[string]bool{}
	for _, e := range reg.Entries {
		if e.RunID == runID {
			continue
		}
		sum := summarizeRegisteredRun(e, false) // one projection per entry
		switch {
		case sum.Visibility == "retired":
			retired = append(retired, e)
		case e.Retirement != nil:
			protected[e.RunID] = true
			active = append(active, e)
		case sum.registryActive():
			active = append(active, e)
		default:
			recent = append(recent, e)
		}
	}
	// Reject any admission that would exceed the active cap. The target is
	// excluded from the count, so an active restart at cap (others <= 31) passes.
	if len(active) >= registryMaxActive {
		reg.Version, reg.UpdatedAt = registryV2Version, now
		reg.Overflow.ActiveRejected++
		reg.Overflow.LastRejectedRunID = runID
		if werr := writeRegistry(path, reg); werr != nil { // bounded writer: an overflow counter must not push the file past the 64 KiB read bound
			return werr
		}
		return errRegistryActiveFull
	}
	if idx >= 0 && reg.CurrentRunID == runID && !reg.Migrated && reg.Entries[idx].Retirement == nil {
		return nil
	}
	cur := registryEntry{RunID: runID, RunPath: abs, StartedAt: now, LastStartedAt: now}
	if idx >= 0 {
		cur = reactivateRegistryEntry(reg.Entries[idx], now)
		cur.LastStartedAt = now
	}
	if reg.Generation < 0 {
		reg.Generation = 0
	}
	reg.Generation++
	cur.Generation = reg.Generation
	sortEntries(recent)
	if len(recent) > registryMaxRecent {
		ev := recent[registryMaxRecent:]
		reg.Overflow.RecentEvicted += len(ev)
		reg.Overflow.LastEvictedRunID = ev[0].RunID
		recent = recent[:registryMaxRecent]
	}
	reg.Entries = append(append(append(append([]registryEntry{}, active...), cur), recent...), retired...)
	enforceRetiredBoundExcept(reg, protected)
	sortEntries(reg.Entries)
	reg.CurrentRunID, reg.Version, reg.UpdatedAt = runID, registryV2Version, now
	return writeRegistry(path, reg)
}

// registryRunName projects only the literal plan name: control characters and
// whitespace are normalized, and the result is capped at 160 runes including
// a trailing ellipsis.
func registryRunName(name string) string {
	name = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)), " ")
	if r := []rune(name); len(r) > 160 {
		return string(r[:159]) + "…"
	}
	return name
}
