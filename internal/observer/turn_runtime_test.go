package observer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestTurnRuntimeV1AndInitialGuards(t *testing.T) {
	p := model.Plan{Name: "legacy", Tasks: []model.Task{runfixture.Task("a", 0)}}
	if e := checkpoint.Validate(p); e != nil {
		t.Fatal(e)
	}
	p.TurnShape = &model.TurnShape{Roots: 1, MainTurns: 1, HelpersPerTurn: 0}
	if checkpoint.Validate(p) == nil {
		t.Fatal("v1 accepted turn fields")
	}
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 0}, runfixture.Task("a", 0))
	s := runfixture.Read(t, root)
	p = s.Plan
	p.Tasks[0].Turn.Index = 2
	if checkpoint.ValidateInitialTurns(p) == nil {
		t.Fatal("initial laterturn accepted")
	}
	if e := graph.QueueRevision(root, model.RevisionProposal{ID: "blocked", Reason: "test v2 fence", Tasks: s.Plan.Tasks}); e == nil || !strings.Contains(e.Error(), "turn") {
		t.Fatalf("v2 revision not rejected: %v", e)
	}
}

func TestTurnRuntimeCheckpointPrefixAndObserverIdentity(t *testing.T) {
	next := runfixture.Task("next", 0)
	root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next, Helpers: []model.Task{runfixture.Task("h", 0)}}))
	runfixture.StageExpansion(t, root, "a")
	if e := commitTurnExpansion(root, "a", "structural"); e != nil {
		t.Fatal(e)
	}
	s := runfixture.Read(t, root)
	top := ProjectTopology(s)
	found := false
	for _, n := range top.Nodes {
		if n.ID == "a__turn2" {
			found = n.Turn != nil && n.Turn.AgentID == "a" && n.Turn.Index == 2 && n.Parent == ""
		}
	}
	if !found {
		t.Fatal("observer lost continuation identity")
	}
	s.Tasks["a"].Status = "running"
	if e := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); e != nil {
		t.Fatal(e)
	}
	if _, e := checkpoint.ReadState(root); e == nil {
		t.Fatal("overlapping/nonverified predecessor checkpoint accepted")
	}
}
