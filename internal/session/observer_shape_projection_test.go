package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/observer"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/sizing"
)

func obsShapePlan(roots, levels, children int) model.Plan {
	p := model.Plan{Name: "private-test-name", MaxTasks: 1024, MaxDepth: 16, ObserverShape: &model.ObserverShape{Roots: roots, Levels: levels, MaxChildren: children}}
	for i := 0; i < roots; i++ {
		x := runfixture.Task(fmt.Sprintf("r%d", i), 0)
		x.Requires = append(x.Requires, "children")
		x.Spawn = &model.SpawnPolicy{Field: "children", MaxChildren: children}
		p.Tasks = append(p.Tasks, x)
	}
	return p
}

func TestObserverShapeCanonicalShape(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int
	}{{"2x5", 10}, {"25x5x2", 775}, {"25x1x2", 25}} {
		t.Run(tc.input, func(t *testing.T) {
			sh, e := sizing.ParseShape(tc.input)
			if e != nil {
				t.Fatal(e)
			}
			p := obsShapePlan(sh.Roots, sh.Levels, sh.MaxChildren)
			if e = checkpoint.ValidateObserverShape(p); e != nil {
				t.Fatal(e)
			}
			got, e := checkpoint.ObsShapeCeiling(*p.ObserverShape)
			if e != nil || got != tc.want {
				t.Fatalf("got %d,%v want %d", got, e, tc.want)
			}
		})
	}
}

func TestObserverShapeLegacyJSON(t *testing.T) {
	p := model.Plan{Name: "legacy", Tasks: []model.Task{runfixture.Task("root", 0)}}
	got, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	// Compare exact encoding to the pre-extension Plan layout, preserving field order.
	legacy := struct {
		APIVersion string                       `json:"api_version,omitempty"`
		Name       string                       `json:"name"`
		Tasks      []model.Task                 `json:"tasks"`
		Context    map[string]string            `json:"context,omitempty"`
		Providers  map[string]model.CommandSpec `json:"providers,omitempty"`
		MaxTasks   int                          `json:"max_tasks,omitempty"`
		MaxDepth   int                          `json:"max_depth,omitempty"`
		Display    *model.PlanDisplay           `json:"display,omitempty"`
	}{Name: p.Name, Tasks: p.Tasks}
	want, _ := json.Marshal(legacy)
	if !bytes.Equal(got, want) {
		t.Fatalf("legacy plan encoding/hash input changed: %s vs %s", got, want)
	}
}

func TestObserverShapeMaterializedMismatch(t *testing.T) {
	for _, kind := range []string{"roots", "depth", "children"} {
		t.Run(kind, func(t *testing.T) {
			p := obsShapePlan(1, 2, 1)
			if err := checkpoint.ValidateObserverShape(p); err != nil {
				t.Fatal(err)
			}
			s := &model.State{Plan: p, ActiveTasks: append([]model.Task{}, p.Tasks...), Tasks: map[string]*model.TaskState{}}
			for _, x := range s.ActiveTasks {
				s.Tasks[x.ID] = s.NewTask(x)
			}
			add := func(id, parent string) {
				x := runfixture.Task(id, 0)
				x.Parent = parent
				s.ActiveTasks = append(s.ActiveTasks, x)
				s.Tasks[id] = s.NewTask(x)
			}
			switch kind {
			case "roots":
				add("extra", "")
			case "depth":
				add("child", "r0")
				add("grandchild", "child")
			case "children":
				add("child1", "r0")
				add("child2", "r0")
			}
			revised := p
			revised.Tasks = s.ActiveTasks
			if err := checkpoint.Validate(revised); err != nil {
				t.Fatalf("observer metadata vetoed generic graph validation: %v", err)
			}
			c := observer.ProjectCapacity(s)
			if c.Known || c.Reason == "" {
				t.Fatalf("actual mismatch presented as known: %+v", c)
			}
			v := observer.ProjectTopology(s)
			if v.NodesTotal != len(s.ActiveTasks) {
				t.Fatal("actual nodes disappeared")
			}
		})
	}
}

func TestObserverShapeTopologyAccounting(t *testing.T) {
	s := &model.State{Tasks: map[string]*model.TaskState{}}
	add := func(id, parent, status string) {
		x := runfixture.Task(id, 0)
		x.Parent = parent
		s.ActiveTasks = append(s.ActiveTasks, x)
		s.Tasks[id] = &model.TaskState{Spec: x, Status: status}
	}
	for r := 0; r < 7; r++ {
		id := fmt.Sprintf("r%d", r)
		add(id, "", "verified")
		for c := 0; c < 40; c++ {
			add(fmt.Sprintf("%s_c%02d", id, c), id, "pending")
		}
	}
	add("orphan", "missing", "failed")
	add("cycleA", "cycleB", "alien")
	add("cycleB", "cycleA", "running")
	v := observer.ProjectTopology(s)
	if v.NodesTotal != 290 || v.InvalidNodes != 3 || v.Shown != 200 || v.Omitted != 87 {
		t.Fatalf("incorrect accounting: %+v", v)
	}
	if v.Shown+v.Omitted+v.InvalidNodes != v.NodesTotal {
		t.Fatal("shown+omitted+invalid != actual")
	}
	sum := 0
	for _, n := range v.StatusCounts {
		sum += n
	}
	if sum != v.NodesTotal || v.StatusCounts["unknown"] != 1 {
		t.Fatal("status accounting lost invalid nodes")
	}
	for _, g := range v.Groups {
		if g.Shown+g.Omitted != g.Total || g.Shown > 32 {
			t.Fatalf("group invalid: %+v", g)
		}
	}
}

func TestObserverShapeProjectionPrivacyAndRunIsolation(t *testing.T) {
	var ids []string
	for i := 0; i < 2; i++ {
		p := obsShapePlan(1, 1, 2)
		p.Tasks[0].Prompt = "SECRET_PROMPT"
		p.Context = map[string]string{"secret": "SECRET_CONTEXT"}
		p.Display = &model.PlanDisplay{Version: 1, Publish: false, Title: "SECRET_TITLE"}
		p.Tasks[0].Display = &model.TaskDisplay{Title: "SECRET_AGENT_TITLE"}
		root := filepath.Join(t.TempDir(), "run")
		if e := checkpoint.Initialize(p, root, "mock"); e != nil {
			t.Fatal(e)
		}
		raw, e := livePanelSnapshot(root)
		if e != nil {
			t.Fatal(e)
		}
		for _, secret := range []string{"SECRET_PROMPT", "SECRET_CONTEXT", "SECRET_TITLE", "SECRET_AGENT_TITLE"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("private projection exposed %s", secret)
			}
		}
		var v struct {
			RunID    string                 `json:"run_id"`
			Topology observer.PanelTopology `json:"topology"`
		}
		if e = json.Unmarshal(raw, &v); e != nil {
			t.Fatal(e)
		}
		if v.Topology.NodesTotal != 1 || len(v.Topology.Nodes) != 1 || v.Topology.Nodes[0].ID != "r0" {
			t.Fatalf("same-id run topology missing: %s", raw)
		}
		ids = append(ids, v.RunID)
	}
	if ids[0] == ids[1] {
		t.Fatal("distinct runs share identity")
	}
}
