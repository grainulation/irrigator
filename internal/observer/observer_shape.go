package observer

import (
	"sort"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

const (
	topologyMaxShown     = 200
	topologyMaxPerParent = 32
	topologyDepthWalk    = 64
	// topologyMaxDependencyEdges bounds dependency edges independently of the
	// node display caps.
	topologyMaxDependencyEdges = 256
)

func obsEffMaxTasks(p model.Plan) int {
	if p.MaxTasks == 0 {
		return 128
	}
	return p.MaxTasks
}

type PanelCapacity struct {
	Semantics                   string               `json:"semantics"`
	Source                      string               `json:"source"`
	Known                       bool                 `json:"known"`
	Reason                      string               `json:"reason,omitempty"`
	Detail                      string               `json:"detail,omitempty"`
	Shape                       *model.ObserverShape `json:"shape,omitempty"`
	ShapeCeilingNodes           int                  `json:"shape_ceiling_nodes,omitempty"`
	AllocationCapNodes          int                  `json:"allocation_cap_nodes,omitempty"`
	EffectiveCeilingNodes       int                  `json:"effective_ceiling_nodes,omitempty"`
	CeilingExceedsAllocationCap bool                 `json:"ceiling_exceeds_allocation_cap"`
}

// projectCapacity compares the declared shape with the ACTUAL active graph
// (s.allTasks and row.Spec). It never infers a shape, never guesses capacity
// on mismatch, and never panics on malformed persisted state. Capacity here is
// a shared allocation ceiling, not a per-parent promise.
func ProjectCapacity(s *model.State) *PanelCapacity {
	c := &PanelCapacity{Semantics: "capacity_ceiling_not_progress"}
	if s == nil || s.Plan.ObserverShape == nil {
		c.Source, c.Reason = "absent", "no_explicit_observer_shape"
		return c
	}
	p := s.Plan
	if err := checkpoint.ValidateObserverShape(p); err != nil {
		c.Source, c.Reason, c.Detail = "invalid_persisted_shape", "invalid_observer_shape", err.Error()
		return c
	}
	sh := *p.ObserverShape
	ceiling, err := checkpoint.ObsShapeCeiling(sh)
	if err != nil {
		c.Source, c.Reason, c.Detail = "invalid_persisted_shape", "invalid_observer_shape", err.Error()
		return c
	}
	mismatch := func(reason, detail string) *PanelCapacity {
		cp := sh // provenance only; no ceilings are reported
		c.Source, c.Reason, c.Detail, c.Shape = "explicit_observer_shape_mismatch", reason, detail, &cp
		return c
	}
	tasks := s.AllTasks()
	parent := make(map[string]string, len(tasks))
	specs := make([]model.Task, 0, len(tasks))
	for _, t := range tasks {
		row := s.Tasks[t.ID]
		if row == nil {
			return mismatch("invalid_actual_topology", "active node has no state row")
		}
		parent[t.ID] = row.Spec.Parent
		specs = append(specs, row.Spec)
	}
	if len(parent) != len(tasks) {
		return mismatch("invalid_actual_topology", "duplicate active node id")
	}
	roots, maxDepth := 0, 0
	kids := map[string]int{}
	for _, t := range specs {
		d, ok := obsDepth(parent, t.ID)
		if !ok {
			return mismatch("invalid_actual_topology", "ownership cycle, missing owner or excessive depth")
		}
		if d == 0 {
			roots++
		}
		if d > maxDepth {
			maxDepth = d
		}
		if t.Parent != "" {
			kids[t.Parent]++
		}
	}
	if roots != sh.Roots {
		return mismatch("root_count_mismatch", "active root count differs from declared roots")
	}
	if maxDepth > sh.Levels-1 {
		return mismatch("depth_exceeds_declared", "actual max depth exceeds declared levels-1")
	}
	for _, n := range kids {
		if n > sh.MaxChildren {
			return mismatch("children_exceed_declared", "a node has more direct children than declared max_children")
		}
	}
	if len(specs) > ceiling {
		return mismatch("task_count_exceeds_declared", "actual task count exceeds declared shape ceiling")
	}
	if sh.Levels > 1 {
		for _, t := range specs {
			if t.Spawn != nil && checkpoint.ObsSpawnLimit(t.Spawn) > sh.MaxChildren {
				return mismatch("spawn_cap_exceeds_declared", "an active spawn cap exceeds declared max_children")
			}
		}
	}
	cp := sh
	cap := obsEffMaxTasks(p)
	eff := ceiling
	if cap < eff {
		eff = cap
	}
	c.Source, c.Known, c.Shape = "explicit_observer_shape", true, &cp
	c.ShapeCeilingNodes, c.AllocationCapNodes, c.EffectiveCeilingNodes = ceiling, cap, eff
	c.CeilingExceedsAllocationCap = ceiling > cap
	return c
}

type panelTopoNode struct {
	Turn      *model.TurnIdentity `json:"turn,omitempty"`
	ID        string              `json:"id"`
	Parent    string              `json:"parent,omitempty"`
	Depth     int                 `json:"depth"`
	Status    string              `json:"status"`
	Lifecycle *PanelLifecycle     `json:"lifecycle,omitempty"`
}
type panelTopoGroup struct {
	Parent  string `json:"parent"`
	Depth   int    `json:"depth"`
	Total   int    `json:"total"`
	Shown   int    `json:"shown"`
	Omitted int    `json:"omitted"`
}
type panelDepthCount struct {
	Depth int `json:"depth"`
	Count int `json:"count"`
}

// panelDepEdge is an ACTUAL execution dependency. From is the dependency (it
// must finish first, and appears in the dependent's DependsOn); To is the
// dependent task that lists it. Direction: From -> To.
type panelDepEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type PanelTopology struct {
	Basis          string            `json:"basis"`
	NodesTotal     int               `json:"nodes_total"`
	InvalidNodes   int               `json:"invalid_nodes"`
	StatusCounts   map[string]int    `json:"status_counts"`
	RootCount      int               `json:"root_count"`
	DepthCounts    []panelDepthCount `json:"depth_counts"`
	ShownLimit     int               `json:"shown_limit"`
	PerParentLimit int               `json:"per_parent_limit"`
	Shown          int               `json:"shown"`
	Omitted        int               `json:"omitted"`
	Groups         []panelTopoGroup  `json:"groups"`
	Nodes          []panelTopoNode   `json:"nodes"`

	// Dependency edges (from=dependency -> to=dependent), unique, valid,
	// sorted by (from,to), capped independently of the node caps. Endpoints
	// may be real nodes omitted from Nodes. Invalid = self-edge or dependency
	// that is not an active task (counted uniquely, never emitted).
	DependencyEdges        []panelDepEdge `json:"dependency_edges"`
	DependencyEdgesTotal   int            `json:"dependency_edges_total"`
	DependencyEdgesShown   int            `json:"dependency_edges_shown"`
	DependencyEdgesOmitted int            `json:"dependency_edges_omitted"`
	DependencyEdgesLimit   int            `json:"dependency_edges_limit"`
	DependencyEdgesInvalid int            `json:"dependency_edges_invalid"`
}

var ObsKnownStatus = map[string]bool{"pending": true, "running": true, "waiting": true, "produced": true,
	"verified": true, "failed": true, "uncertain": true, "cancelled": true}

func obsDepth(parent map[string]string, id string) (int, bool) {
	d := 0
	for p := parent[id]; p != ""; p = parent[p] {
		if _, ok := parent[p]; !ok {
			return 0, false
		}
		d++
		if d > topologyDepthWalk {
			return 0, false
		}
	}
	return d, true
}

// obsDependencyEdges projects actual DependsOn edges among active tasks.
// Read-only; returns sorted capped edges, unique valid total and unique
// invalid count.
func obsDependencyEdges(tasks []model.Task) ([]panelDepEdge, int, int) {
	ids := make(map[string]bool, len(tasks))
	for _, x := range tasks {
		ids[x.ID] = true
	}
	valid := map[panelDepEdge]bool{}
	invalid := map[panelDepEdge]bool{}
	for _, x := range tasks {
		for _, d := range x.DependsOn {
			e := panelDepEdge{From: d, To: x.ID}
			if d == x.ID || !ids[d] || !ids[x.ID] {
				invalid[e] = true
				continue
			}
			valid[e] = true
		}
	}
	edges := make([]panelDepEdge, 0, len(valid))
	for e := range valid {
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	total := len(edges)
	if len(edges) > topologyMaxDependencyEdges {
		edges = edges[:topologyMaxDependencyEdges]
	}
	return edges, total, len(invalid)
}

// projectTopology summarizes ACTUAL materialized nodes only. Omitted nodes are
// real nodes that exist but were not listed; nothing potential is invented.
func ProjectTopology(s *model.State) *PanelTopology {
	t := &PanelTopology{Basis: "materialized_nodes_only", StatusCounts: map[string]int{}, DepthCounts: []panelDepthCount{},
		Groups: []panelTopoGroup{}, Nodes: []panelTopoNode{}, ShownLimit: topologyMaxShown, PerParentLimit: topologyMaxPerParent,
		DependencyEdges: []panelDepEdge{}, DependencyEdgesLimit: topologyMaxDependencyEdges}
	life := ProjectLifecycles(s)
	tasks := s.AllTasks()
	parent := make(map[string]string, len(tasks))
	for _, x := range tasks {
		parent[x.ID] = x.Parent
	}
	groups := map[string][]panelTopoNode{}
	depthCounts := map[int]int{}
	valid := 0
	for _, x := range tasks {
		t.NodesTotal++
		status := "unknown"
		if row := s.Tasks[x.ID]; row != nil && ObsKnownStatus[row.Status] {
			status = row.Status
		}
		t.StatusCounts[status]++
		d, ok := obsDepth(parent, x.ID)
		if !ok {
			t.InvalidNodes++
			continue
		}
		valid++
		depthCounts[d]++
		if d == 0 {
			t.RootCount++
		}
		groups[x.Parent] = append(groups[x.Parent], panelTopoNode{Turn: x.Turn, ID: x.ID, Parent: x.Parent, Depth: d, Status: status, Lifecycle: life[x.ID]})
	}
	for d, n := range depthCounts {
		t.DepthCounts = append(t.DepthCounts, panelDepthCount{Depth: d, Count: n})
	}
	sort.Slice(t.DepthCounts, func(i, j int) bool { return t.DepthCounts[i].Depth < t.DepthCounts[j].Depth })
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		di, dj := groups[keys[i]][0].Depth, groups[keys[j]][0].Depth
		if di != dj {
			return di < dj
		}
		return keys[i] < keys[j]
	})
	budget := topologyMaxShown
	for _, k := range keys {
		items := groups[k]
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
		show := len(items)
		if show > topologyMaxPerParent {
			show = topologyMaxPerParent
		}
		if show > budget {
			show = budget
		}
		budget -= show
		t.Nodes = append(t.Nodes, items[:show]...)
		t.Groups = append(t.Groups, panelTopoGroup{Parent: k, Depth: items[0].Depth, Total: len(items), Shown: show, Omitted: len(items) - show})
	}
	t.Shown = len(t.Nodes)
	t.Omitted = valid - t.Shown

	edges, total, invalid := obsDependencyEdges(tasks)
	t.DependencyEdges = edges
	t.DependencyEdgesTotal = total
	t.DependencyEdgesShown = len(edges)
	t.DependencyEdgesOmitted = total - len(edges)
	t.DependencyEdgesInvalid = invalid
	return t
}
