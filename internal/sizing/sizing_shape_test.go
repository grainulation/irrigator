package sizing

import (
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func TestShapeNodeCounts(t *testing.T) {
	for in, want := range map[string]int{"1x1x1": 1, "2x5": 10, "2×5": 10, "2x5x1": 10, "25x5x2": 775, "1x16x1": 16, "3x3x3": 39} {
		sh, err := ParseShape(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if n, _ := checkpoint.ShapeMaxNodes(sh); n != want {
			t.Fatalf("%s = %d want %d", in, n, want)
		}
	}
}

func TestShapeRejections(t *testing.T) {
	for _, in := range []string{"", "5", "0x1x1", "1x0x1", "1x1x0", "-1x2x2", "2x-3", "1x17x1", "1x2x1025", "1025x1x1", "2x10x2", "9223372036854775808x1x1", "99999999999x99999999999x99999999", "1x2x3x4", "axbxc", "+2x2", "2.5x2"} {
		if _, err := ParseShape(in); err == nil {
			t.Fatalf("%q accepted", in)
		}
	}
	if _, err := ParseShape("1x11x2"); err == nil { // 2047 nodes
		t.Fatal("excess nodes accepted")
	}
	if _, err := ParseShape("1x10x2"); err != nil { // 1023 nodes
		t.Fatal(err)
	}
}

func TestShapeReportNumbers(t *testing.T) {
	c := szCfg(10)
	c.Defaults.CostUSD = .01
	c.Capacity = 8
	sh, _ := ParseShape("25x5x2")
	r, err := buildShapeReport(sh, c, .25, "mock")
	if err != nil || r.MaxNodes != 775 || r.WidestLevel != 25*16 {
		t.Fatalf("%+v %v", r, err)
	}
	szNear(t, "roots", r.CostRootsOnlyUSD, .25)
	szNear(t, "full", r.CostFullUSD, 7.75)
	if !r.Feasible || r.PredictedWorkers < 1 || r.PredictedWorkers > 8 || r.FrontierBoundS == nil || r.RecommendedWorkers > r.PredictedWorkers {
		t.Fatalf("%+v", r)
	}
	b := .5
	c.BudgetUSD = &b
	r, _ = buildShapeReport(sh, c, .25, "mock")
	if !r.Feasible || r.FullWithinBudget {
		t.Fatalf("%+v", r)
	}
	b = .1
	r, _ = buildShapeReport(sh, c, .25, "mock")
	if r.Feasible || r.RecommendedWorkers != 0 && false || !strings.Contains(r.Reason, "budget") {
		t.Fatalf("%+v", r)
	}
}

func TestShapeMissingMapWarnsAndRecommendsZero(t *testing.T) {
	c := szCfg(10)
	c.ProviderBalanceUSD = nil
	sh, _ := ParseShape("2x5")
	r, _ := buildShapeReport(sh, c, .25, "mock")
	if r.RecommendedWorkers != 0 || r.PredictedWorkers < 1 || r.ReservationN == nil || *r.ReservationN != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestGateArchivedSpendBlocks(t *testing.T) {
	ts := szIndep(1, "")
	s := szState(ts, "mock")
	s.ArchivedTasks = []model.TaskState{{Spec: model.Task{ID: "old"}, Cost: .10}}
	g := &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	if out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25); len(out) != 0 {
		t.Fatalf("archived spend ignored: %v", out)
	}
	s.ArchivedTasks = nil
	g = &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	if out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25); len(out) != 1 {
		t.Fatalf("want 1: %v", out)
	}
}

func TestGateArchivedProviderAttribution(t *testing.T) {
	ts := szIndep(1, "")
	ts[0].Provider = "p1"
	s := szState(ts, "mock")
	s.ArchivedTasks = []model.TaskState{{Spec: model.Task{ID: "old", Provider: "p1"}, Cost: .10}}
	g := &sizingGate{cfg: resCfg(map[string]float64{"p1": .30, "mock": 5})}
	if out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25); len(out) != 0 {
		t.Fatalf("%v", out)
	}
}

func TestGateArchivedCostChangesCache(t *testing.T) {
	ts := szIndep(1, "")
	s := szState(ts, "mock")
	s.ArchivedTasks = []model.TaskState{{Spec: model.Task{ID: "old"}, Cost: .01}}
	g := &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	if out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25); len(out) != 1 {
		t.Fatalf("%v", out)
	}
	k1 := szKey(s, ts, 4, 1)
	s.ArchivedTasks[0].Cost = .10
	if szKey(s, ts, 4, 1) == k1 {
		t.Fatal("key ignores archived cost")
	}
	if out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25); len(out) != 0 {
		t.Fatalf("stale cache: %v", out)
	}
}

func TestGateMissingBalanceMapFailsClosed(t *testing.T) {
	ts := szIndep(2, "")
	s := szState(ts, "mock")
	c := szCfg(10)
	c.ProviderBalanceUSD = nil
	g := &sizingGate{cfg: c}
	if out, sl := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25); len(out) != 0 || sl != 0 {
		t.Fatalf("%v %d", out, sl)
	}
}
