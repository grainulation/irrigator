package runops

import (
	"fmt"
	"sort"

	"github.com/grainulation/irrigator/internal/model"
)

// prioritizeReady starts work that gates the deepest unfinished dependency
// chain first. This is a unit-cost heuristic, not a prediction of model latency.
// Stable ties preserve plan order. Ownership edges represent completion barriers:
// a parent's consumers cannot run until its children have finished.
func prioritizeReady(s *model.State, ready []string) {
	if len(ready) < 2 {
		return
	}
	next := map[string][]string{}
	for _, t := range s.AllTasks() {
		if s.Tasks[t.ID].Status == "verified" {
			continue
		}
		for _, dep := range t.DependsOn {
			next[dep] = append(next[dep], t.ID)
		}
		if t.Parent != "" {
			next[t.ID] = append(next[t.ID], t.Parent)
		}
	}
	ranks := map[string]int{}
	visiting := map[string]bool{}
	var rank func(string) int
	rank = func(id string) int {
		if n, ok := ranks[id]; ok {
			return n
		}
		// Validation owns cycle rejection; stay bounded on a corrupt snapshot.
		if visiting[id] {
			return 0
		}
		visiting[id] = true
		n := 1
		for _, child := range next[id] {
			if m := 1 + rank(child); m > n {
				n = m
			}
		}
		delete(visiting, id)
		ranks[id] = n
		return n
	}
	for _, id := range ready {
		rank(id)
	}
	sort.SliceStable(ready, func(i, j int) bool { return ranks[ready[i]] > ranks[ready[j]] })
}

const (
	scheduleDepth   = "depth"
	ScheduleBreadth = "breadth"
)

// ActiveSchedulingOrder carries an explicit apply/resume/demo request into run.
// Empty means "keep whatever order the run already persisted"; it never selects
// breadth by itself. Same pattern as activeSizingPath/activeSessionRegistry.
var ActiveSchedulingOrder string

func NormalizeSchedulingOrder(v string) (string, error) {
	switch v {
	case "", scheduleDepth, ScheduleBreadth:
		return v, nil
	}
	return "", fmt.Errorf("unsupported --schedule %q; use depth or breadth", v)
}

// applySchedulingOrder records an explicit request in the run checkpoint. Depth
// is the default and is stored as absent so default runs stay byte-compatible.
// A change is journaled as one scheduling_order event; no-ops add nothing.
func applySchedulingOrder(s *model.State, requested string) {
	if requested == "" {
		return
	}
	want := ""
	if requested == ScheduleBreadth {
		want = ScheduleBreadth
	}
	if s.Scheduling == want {
		return
	}
	s.Scheduling = want
	s.Event("scheduling_order", "", map[string]string{"order": requested})
}

// orderReady orders already dependency-ready work. It only permutes the slice:
// readiness, capacity, sizing admission and ownership barriers are decided by
// the caller. Default: critical-chain depth (prioritizeReady). Opt-in breadth:
// the same ordering, then a stable sort by ownership level so plan roots start
// before their descendants; ties keep the critical-chain order.
func orderReady(s *model.State, ready []string) {
	prioritizeReady(s, ready)
	if s.Scheduling != ScheduleBreadth || len(ready) < 2 {
		return
	}
	levels := ownershipLevels(s)
	sort.SliceStable(ready, func(i, j int) bool { return levels[ready[i]] < levels[ready[j]] })
}

// ownershipLevels maps task ID to ownership depth (0 for plan roots). Walks are
// bounded so a corrupt snapshot cannot loop; validation owns cycle rejection.
func ownershipLevels(s *model.State) map[string]int {
	parent := map[string]string{}
	for _, t := range s.AllTasks() {
		parent[t.ID] = t.Parent
	}
	levels := map[string]int{}
	for id := range parent {
		n := 0
		for p, hops := parent[id], 0; p != "" && hops <= len(parent); p, hops = parent[p], hops+1 {
			n++
		}
		levels[id] = n
	}
	return levels
}
