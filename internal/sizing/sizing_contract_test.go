package sizing

import (
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func TestSizingArchivedProviderSpendCannotBeReused(t *testing.T) {
	ts := szIndep(1, "")
	s := szState(ts, "mock")
	s.ArchivedTasks = []model.TaskState{{Spec: model.Task{ID: "retired"}, Cost: .1}}
	c := szCfg(1)
	c.ProviderBalanceUSD = map[string]float64{"mock": .30}
	g := &sizingGate{cfg: c}
	out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 1, .25)
	if len(out) != 0 {
		t.Fatal("archived spend reused as reservation capacity")
	}
}

func TestShapeReservationInfeasibleIsExplicit(t *testing.T) {
	sh, err := ParseShape("2x5")
	if err != nil {
		t.Fatal(err)
	}
	c := szCfg(1)
	c.ProviderBalanceUSD = nil
	r, err := buildShapeReport(sh, c, .25, "mock")
	if err != nil {
		t.Fatal(err)
	}
	if r.Feasible || r.RecommendedWorkers != 0 || r.PredictedWorkers < 1 {
		t.Fatalf("ambiguous infeasibility: %+v", r)
	}
	c.ProviderBalanceUSD = map[string]float64{"mock": .10}
	r, err = buildShapeReport(sh, c, .25, "mock")
	if err != nil || r.Feasible || r.RecommendedWorkers != 0 {
		t.Fatalf("reservation shortfall accepted: %+v %v", r, err)
	}
}

func TestShapeReliabilityUsesRecommendedConcurrency(t *testing.T) {
	sh, _ := ParseShape("1x2x2")
	c := szCfg(1)
	c.Defaults.RetryP = .01
	c.ContentionGamma = .5
	c.ReliabilityFloor = .965
	c.Capacity = 2
	r, err := buildShapeReport(sh, c, .25, "mock")
	if err != nil {
		t.Fatal(err)
	}
	if r.RecommendedWorkers != 2 || r.FullMeetsFloor {
		t.Fatalf("full tree reliability ignored selected contention: %+v", r)
	}
}

func TestShapeSingleLevelStillBoundsChildren(t *testing.T) {
	if _, err := ParseShape("1x1x1025"); err == nil {
		t.Fatal("single level bypassed child bound")
	}
	if maxSizingTasks != checkpoint.MaxShapeNodes {
		t.Fatal("shape and runtime task bounds disagree")
	}
}
