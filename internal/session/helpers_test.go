package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func dstatWriteJournal(t *testing.T, root string, rec graph.DeliveryRecord) {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "delivery.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// retTwo registers an "old" run then a "new" run (new is current).
func retTwo(t *testing.T) (dir, reg, rA, idA, rB, idB string) {
	t.Helper()
	dir = t.TempDir()
	reg = filepath.Join(dir, ".irrigator", "current-session.json")
	rA, idA = runfixture.V2Run(t, dir, "old")
	rB, idB = runfixture.V2Run(t, dir, "new")
	for _, x := range [][2]string{{rA, idA}, {rB, idB}} {
		if err := PublishSession(reg, x[0], x[1]); err != nil {
			t.Fatal(err)
		}
	}
	return
}

type panelNode struct {
	Turn      *model.TurnIdentity      `json:"turn,omitempty"`
	Title     string                   `json:"title,omitempty"`
	Summary   string                   `json:"summary,omitempty"`
	ID        string                   `json:"id"`
	Parent    string                   `json:"parent,omitempty"`
	DependsOn []string                 `json:"depends_on"`
	Status    string                   `json:"status"`
	Attempt   int                      `json:"attempt"`
	Lifecycle *observer.PanelLifecycle `json:"lifecycle,omitempty"`
}

type panelSnapshot struct {
	ExecutionModel string                  `json:"execution_model,omitempty"`
	TurnShape      *model.TurnShape        `json:"turn_shape,omitempty"`
	RunID          string                  `json:"run_id"`
	Control        string                  `json:"control"`
	Backend        string                  `json:"backend"`
	Revision       int                     `json:"revision"`
	CostAccounting *provider.CostSummary   `json:"cost_accounting,omitempty"`
	Cost           float64                 `json:"cost_usd"`
	ObservedAt     *time.Time              `json:"observed_at,omitempty"`
	Nodes          []panelNode             `json:"nodes"`
	WorkersActive  *int                    `json:"workers_active"`
	WorkersKnown   bool                    `json:"workers_known"`
	RunName        string                  `json:"run_name,omitempty"`
	Events         []PanelEvent            `json:"events"`
	EventsTotal    int                     `json:"events_total"`
	EventsBasis    string                  `json:"events_basis"`
	Capacity       *observer.PanelCapacity `json:"capacity,omitempty"`
	Topology       *observer.PanelTopology `json:"topology,omitempty"`
	Delivery       graph.DeliveryView      `json:"delivery"`
}

// livePanelSnapshot assembles the retained project* functions into one JSON
// document for test assertions. Lease probes are read-only and never create
// files or wait for a worker lock.
func livePanelSnapshot(root string) ([]byte, error) {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return nil, err
	}
	active := 0
	life := observer.ProjectLifecycles(s)
	p := panelSnapshot{RunID: s.RunID, RunName: registryRunName(s.Plan.Name), Control: s.Control, Backend: s.Backend, Revision: s.GraphRevision, Cost: s.ReportedCost(), Nodes: []panelNode{}, WorkersKnown: true}
	accounting := provider.ProjectCost(s)
	p.CostAccounting = &accounting
	p.Cost = accounting.KnownReportedUSD
	if checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
		p.ExecutionModel = "turns/v1"
		p.TurnShape = s.Plan.TurnShape
	}
	for _, t := range s.AllTasks() {
		r := s.Tasks[t.ID]
		node := panelNode{Turn: t.Turn, ID: t.ID, Parent: t.Parent, DependsOn: append([]string{}, t.DependsOn...), Status: r.Status, Attempt: r.Attempt, Lifecycle: life[t.ID]}
		p.Nodes = append(p.Nodes, node)
		f, e := os.Open(filepath.Join(root, "cells", t.ID, "lease"))
		if e != nil {
			p.WorkersKnown = false
			continue
		}
		e = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if errors.Is(e, syscall.EWOULDBLOCK) {
			active++
		} else if e != nil {
			p.WorkersKnown = false
		} else {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		_ = f.Close()
	}
	p.Events, p.EventsTotal = PanelEvents(s), len(s.Events)
	p.EventsBasis = "allowlisted safe state.Events kind/task/time entries with runid:seq ids; newest 200 only; not complete durable history; no flight or cause is inferred"
	if p.WorkersKnown {
		p.WorkersActive = &active
	}
	p.Capacity, p.Topology = observer.ProjectCapacity(s), observer.ProjectTopology(s)
	p.Delivery = graph.ProjectDelivery(root, s)
	return json.Marshal(p)
}

// sessionHookArgs prepends per-session hook settings unless the user passed
// their own --settings, which is left untouched.
func sessionHookArgs(runtime string, args []string) []string {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--settings" || strings.HasPrefix(a, "--settings=") {
			fmt.Fprintln(os.Stderr, "Irrigator: keeping your --settings; the Session view will show this session as terminal-only.")
			return args
		}
	}
	return append([]string{"--settings", sessionHookSettings(runtime)}, args...)
}

// readSessionRegistry returns the CURRENT entry in the single-run (v1) shape.
// Other runs are in readRegistryAny.
func readSessionRegistry(path string) (*sessionRegistry, error) {
	reg, err := ReadRegistryAny(path)
	if err != nil {
		return nil, err
	}
	for _, e := range reg.Entries {
		if e.RunID == reg.CurrentRunID {
			return &sessionRegistry{Version: reg.Version, RunID: e.RunID, RunPath: e.RunPath, Generation: reg.Generation, StartedAt: e.LastStartedAt}, nil
		}
	}
	return nil, errors.New("registry has no current entry")
}

// enforceRetiredBound keeps at most registryMaxRetired retired entries. The
// oldest are evicted (never the current run) and the eviction is recorded.
func enforceRetiredBound(reg *sessionRegistryV2) {
	enforceRetiredBoundExcept(reg, retiredBoundProtected(reg, ""))
}
