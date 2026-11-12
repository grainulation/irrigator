package observer

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func depTask(id string, deps ...string) model.Task {
	return model.Task{ID: id, Prompt: "SECRET-PROMPT", Requires: []string{"out"}, DependsOn: deps,
		Context: []string{"SECRET-CONTEXT"}}
}

func depState(tasks ...model.Task) *model.State {
	return actualState(model.Plan{Name: "n", Tasks: tasks})
}

func TestDependencyEdgeDirection(t *testing.T) {
	tp := ProjectTopology(depState(depTask("a"), depTask("b", "a")))
	if len(tp.DependencyEdges) != 1 || tp.DependencyEdges[0] != (panelDepEdge{From: "a", To: "b"}) {
		t.Fatalf("%+v", tp.DependencyEdges)
	}
	if tp.DependencyEdgesTotal != 1 || tp.DependencyEdgesShown != 1 || tp.DependencyEdgesOmitted != 0 ||
		tp.DependencyEdgesLimit != 256 || tp.DependencyEdgesInvalid != 0 {
		t.Fatalf("%+v", tp)
	}
}

func TestDependencyEdgesNoParentInference(t *testing.T) {
	p := model.Plan{Name: "n", Tasks: []model.Task{depTask("r0")}}
	s := actualState(p, model.Task{ID: "r0__c0", Parent: "r0", Prompt: "x", Requires: []string{"out"}})
	tp := ProjectTopology(s)
	if tp.DependencyEdgesTotal != 0 || len(tp.DependencyEdges) != 0 || tp.DependencyEdges == nil {
		t.Fatalf("%+v", tp)
	}
}

func TestDependencyEdgesDeterministicShuffle(t *testing.T) {
	var base []model.Task
	base = append(base, depTask("a"), depTask("b"), depTask("c", "b", "a"), depTask("d", "c", "a", "b"), depTask("e", "d"))
	want := ""
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 20; i++ {
		tasks := append([]model.Task{}, base...)
		r.Shuffle(len(tasks), func(x, y int) { tasks[x], tasks[y] = tasks[y], tasks[x] })
		for k := range tasks {
			deps := append([]string{}, tasks[k].DependsOn...)
			r.Shuffle(len(deps), func(x, y int) { deps[x], deps[y] = deps[y], deps[x] })
			tasks[k].DependsOn = deps
		}
		tp := ProjectTopology(depState(tasks...))
		got := fmt.Sprint(tp.DependencyEdges)
		if want == "" {
			want = got
			exp := "[{a c} {a d} {b c} {b d} {c d} {d e}]"
			if got != exp {
				t.Fatalf("got %s want %s", got, exp)
			}
		}
		if got != want {
			t.Fatalf("nondeterministic: %s vs %s", got, want)
		}
	}
}

func TestDependencyEdgesDuplicateSuppression(t *testing.T) {
	tp := ProjectTopology(depState(depTask("a"), depTask("b", "a", "a", "a")))
	if tp.DependencyEdgesTotal != 1 || len(tp.DependencyEdges) != 1 || tp.DependencyEdgesInvalid != 0 {
		t.Fatalf("%+v", tp)
	}
}

func TestDependencyEdgesBound256AndOmittedNodes(t *testing.T) {
	tasks := []model.Task{depTask("z")}
	for i := 0; i < 300; i++ {
		tasks = append(tasks, depTask(fmt.Sprintf("t%03d", i), "z"))
	}
	tp := ProjectTopology(depState(tasks...))
	if tp.DependencyEdgesTotal != 300 || tp.DependencyEdgesShown != 256 || tp.DependencyEdgesOmitted != 44 ||
		len(tp.DependencyEdges) != 256 || tp.DependencyEdgesLimit != 256 {
		t.Fatalf("total=%d shown=%d omitted=%d len=%d", tp.DependencyEdgesTotal, tp.DependencyEdgesShown, tp.DependencyEdgesOmitted, len(tp.DependencyEdges))
	}
	if tp.DependencyEdges[0] != (panelDepEdge{From: "z", To: "t000"}) || tp.DependencyEdges[255] != (panelDepEdge{From: "z", To: "t255"}) {
		t.Fatalf("order: %+v %+v", tp.DependencyEdges[0], tp.DependencyEdges[255])
	}
	// Node caps are independent: node t100 is a real but omitted node, yet its edge is kept.
	shownNode := map[string]bool{}
	for _, n := range tp.Nodes {
		shownNode[n.ID] = true
	}
	if shownNode["t100"] || tp.Omitted == 0 {
		t.Fatalf("t100 should be an omitted node; omitted=%d", tp.Omitted)
	}
	found := false
	for _, e := range tp.DependencyEdges {
		if e.To == "t100" {
			found = true
		}
	}
	if !found {
		t.Fatal("edge to omitted node t100 missing")
	}
	if tp.NodesTotal != 301 {
		t.Fatalf("nodes_total %d", tp.NodesTotal)
	}
}

func TestDependencyEdgesInvalidEndpoints(t *testing.T) {
	tasks := []model.Task{
		depTask("a"),
		depTask("c", "ghost", "ghost", "c", "a"),
		depTask("d", "other-missing"),
	}
	tp := ProjectTopology(depState(tasks...))
	if tp.DependencyEdgesTotal != 1 || tp.DependencyEdges[0] != (panelDepEdge{From: "a", To: "c"}) {
		t.Fatalf("%+v", tp.DependencyEdges)
	}
	// ghost->c (once), c->c, other-missing->d
	if tp.DependencyEdgesInvalid != 3 {
		t.Fatalf("invalid=%d", tp.DependencyEdgesInvalid)
	}
	b, _ := json.Marshal(tp)
	if strings.Contains(string(b), "ghost") || strings.Contains(string(b), "other-missing") {
		t.Fatalf("invalid ids leaked: %s", b)
	}
}

func TestDependencyProjectionNoSecretPayloadAndStateUnchanged(t *testing.T) {
	s := depState(depTask("a"), depTask("b", "a"))
	s.Tasks["a"].Result = map[string]string{"out": "SECRET-RESULT"}
	s.Tasks["a"].Error = "SECRET-ERROR"
	s.Plan.Context = map[string]string{"k": "SECRET-PLANCTX"}
	before, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	tp := ProjectTopology(s)
	out, _ := json.Marshal(tp)
	for _, secret := range []string{"SECRET"} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("secret leaked: %s", out)
		}
	}
	if !strings.Contains(string(out), `"dependency_edges":[{"from":"a","to":"b"}]`) {
		t.Fatalf("edge json: %s", out)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("state mutated by projection")
	}
}
