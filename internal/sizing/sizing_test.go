package sizing

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func szCfg(s float64) *sizingConfig {
	return &sizingConfig{Version: "sizing.v1", ProviderBalanceUSD: map[string]float64{"mock": 1e6}, Defaults: sizingEstimate{ServiceS: s, Provenance: "assumed"}}
}
func szT(id string, deps []string, out string) model.Task {
	t := model.Task{ID: id, DependsOn: deps}
	if out != "" {
		t.Outputs = map[string]string{"f": out}
	}
	return t
}
func szPending(string) string { return "pending" }
func szIndep(n int, out string) []model.Task {
	var ts []model.Task
	for i := 0; i < n; i++ {
		ts = append(ts, szT(fmt.Sprintf("t%03d", i), nil, out))
	}
	return ts
}
func szIDs(ts []model.Task) []string {
	var ids []string
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	return ids
}
func szState(ts []model.Task, backend string) *model.State {
	s := &model.State{Backend: backend, Plan: model.Plan{Tasks: ts}, Tasks: map[string]*model.TaskState{}}
	for _, tk := range ts {
		s.Tasks[tk.ID] = &model.TaskState{Spec: tk, Status: "pending"}
	}
	return s
}
func szNear(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}
func szSane(t *testing.T, r sizingReport) {
	t.Helper()
	for _, v := range []float64{r.CriticalPathS, r.WorkS, r.ConflictBoundS, r.ExpectedCostUSD, r.ShortfallUSD} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			t.Fatalf("bad value in %+v", r)
		}
	}
	for _, row := range r.Rows {
		if math.IsNaN(row.MakespanS) || row.MakespanS >= math.MaxFloat64 || row.MakespanS < 0 || row.Reliability < 0 || row.Reliability > 1 {
			t.Fatalf("bad row %+v", row)
		}
		if row.MakespanS+1e-9 < math.Max(r.CriticalPathS, r.WorkS/float64(row.N)) {
			t.Fatalf("makespan below lower bound: %+v", row)
		}
	}
}

func TestSizingChain(t *testing.T) {
	ts := []model.Task{szT("a", nil, ""), szT("b", []string{"a"}, ""), szT("c", []string{"b"}, ""), szT("d", []string{"c"}, "")}
	r := sizeReport(ts, szPending, 0, szCfg(10), 8)
	szSane(t, r)
	szNear(t, "cp", r.CriticalPathS, 40)
	if r.Chosen != 1 || r.Rows[0].MakespanS != 40 {
		t.Fatalf("%+v", r)
	}
}

func TestSizingWide(t *testing.T) {
	r := sizeReport(szIndep(8, ""), szPending, 0, szCfg(10), 8)
	szSane(t, r)
	if r.Chosen != 8 || r.Rows[0].MakespanS != 80 || r.Rows[len(r.Rows)-1].MakespanS != 10 {
		t.Fatalf("%+v", r)
	}
	if r2 := sizeReport(szIndep(8, ""), szPending, 0, szCfg(10), 3); r2.Chosen != 3 {
		t.Fatalf("capacity not honoured: %+v", r2)
	}
}

func TestSizingFileConflictIsExclusionNotDividedByN(t *testing.T) {
	r := sizeReport(szIndep(4, "out.md"), szPending, 0, szCfg(10), 8)
	szSane(t, r)
	szNear(t, "conflict", r.ConflictBoundS, 40)
	if r.Chosen != 1 || r.Rows[0].MakespanS != 40 || r.Rows[len(r.Rows)-1].MakespanS != 40 {
		t.Fatalf("%+v", r)
	}
}

func TestSizingConflictCannotCreateFalseCycle(t *testing.T) {
	// a depends on b, both write x: plan-order chaining would make a->b and b->a.
	ts := []model.Task{szT("a", []string{"b"}, "x"), szT("b", nil, "x")}
	r := sizeReport(ts, szPending, 0, szCfg(10), 4)
	if strings.Contains(r.Reason, "cycle") || len(r.Rows) == 0 || r.Rows[len(r.Rows)-1].MakespanS != 20 {
		t.Fatalf("%+v", r)
	}
}

func TestSizingBudgetInfeasible(t *testing.T) {
	c := szCfg(10)
	c.Defaults.CostUSD, c.Defaults.RetryP = .1, .2
	b := .25
	c.BudgetUSD = &b
	c.ReliabilityFloor = .1
	r := sizeReport(szIndep(3, ""), szPending, 0, c, 8)
	if r.Feasible || r.Chosen != 0 {
		t.Fatalf("%+v", r)
	}
	szNear(t, "cost", r.ExpectedCostUSD, .36)
	szNear(t, "shortfall", r.ShortfallUSD, .11)
	if r2 := sizeReport(szIndep(3, ""), szPending, .2, c, 8); r2.BudgetRemainingUSD == nil || math.Abs(*r2.BudgetRemainingUSD-.05) > 1e-12 {
		t.Fatalf("%+v", r2)
	}
}

func TestSizingReliabilityInfeasibleAdmitsZero(t *testing.T) {
	c := szCfg(10)
	c.Defaults.RetryP = .1
	r := sizeReport(szIndep(6, ""), szPending, 0, c, 8)
	if r.Feasible || r.Chosen != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestSizingRetryInflatesDuration(t *testing.T) {
	c := szCfg(10)
	c.Defaults.RetryP = .2
	c.ReliabilityFloor = .01
	r := sizeReport(szIndep(1, ""), szPending, 0, c, 8)
	szNear(t, "d", r.WorkS, 12)
}

func TestSizingMixedAndOverhead(t *testing.T) {
	ts := []model.Task{szT("a", nil, ""), szT("b", []string{"a"}, ""), szT("c", []string{"a"}, ""), szT("d", []string{"a"}, ""), szT("e", []string{"b", "c", "d"}, ""), szT("f", nil, "")}
	r := sizeReport(ts, szPending, 0, szCfg(10), 8)
	szSane(t, r)
	szNear(t, "cp", r.CriticalPathS, 30)
	c := szCfg(1)
	c.Overhead = sizingOverhead{ResidualS: 100, Provenance: "assumed"}
	r = sizeReport(ts, szPending, 0, c, 8)
	szSane(t, r)
	if r.WorkS != 606 {
		t.Fatalf("residual must be counted once per task: %v", r.WorkS)
	}
}

func TestSizingOwnershipBarrier(t *testing.T) {
	p := szT("p", nil, "")
	c1, c2 := szT("c1", nil, ""), szT("c2", nil, "")
	c1.Parent, c2.Parent = "p", "p"
	z := szT("z", []string{"p"}, "")
	st := func(id string) string {
		if id == "p" {
			return "waiting"
		}
		return "pending"
	}
	r := sizeReport([]model.Task{p, c1, c2, z}, st, 0, szCfg(10), 4)
	szSane(t, r)
	szNear(t, "cp", r.CriticalPathS, 20)
	szNear(t, "work", r.WorkS, 30)
	szNear(t, "T1", r.Rows[0].MakespanS, 30)
}

func TestSizingNoDataAndExcludedStatuses(t *testing.T) {
	r := sizeReport(szIndep(4, ""), szPending, 0, &sizingConfig{Version: "sizing.v1"}, 8)
	if r.Chosen != 1 || r.Basis != "no-data-conservative" || r.Missing != 4 {
		t.Fatalf("%+v", r)
	}
	failed := func(string) string { return "failed" }
	if r := sizeReport(szIndep(4, ""), failed, 0, szCfg(10), 8); r.Tasks != 0 || r.Chosen != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestSizingNoDataFallbackKeepsBudgetAndReliabilitySafety(t *testing.T) {
	b := .05
	c := &sizingConfig{Version: "sizing.v1", BudgetUSD: &b, Defaults: sizingEstimate{CostUSD: .1}}
	if r := sizeReport(szIndep(2, ""), szPending, 0, c, 8); r.Feasible || r.Chosen != 0 {
		t.Fatalf("no-data bypassed budget: %+v", r)
	}
	c2 := &sizingConfig{Version: "sizing.v1", Defaults: sizingEstimate{RetryP: .2}}
	if r := sizeReport(szIndep(2, ""), szPending, 0, c2, 8); r.Feasible || r.Chosen != 0 {
		t.Fatalf("no-data bypassed reliability: %+v", r)
	}
}

func TestSizingOversizeRefused(t *testing.T) {
	r := sizeReport(szIndep(maxSizingTasks+1, ""), szPending, 0, szCfg(1), 8)
	if r.Feasible || r.Chosen != 0 || r.Basis != "task-limit-refused" {
		t.Fatalf("%+v", r.Basis)
	}
}

func TestSizingHighVarianceChangesSelection(t *testing.T) {
	c := &sizingConfig{Version: "sizing.v1", Tasks: map[string]sizingEstimate{
		"a": {ServiceS: 10, Provenance: "assumed"},
		"b": {ServiceS: 10, Provenance: "assumed"},
		"c": {ServiceS: .1, ServiceVarS2: 100, Provenance: "assumed"},
	}}
	ts := []model.Task{szT("a", nil, ""), szT("b", nil, ""), szT("c", nil, "")}
	r := sizeReport(ts, szPending, 0, c, 8)
	if r.Rows[1].MakespanS > 10.2 || r.Rows[1].ConservativeS < 19.9 {
		t.Fatalf("%+v", r.Rows)
	}
	// Expected makespan alone would pick N=2; conservative duration needs N=3.
	if r.Chosen != 3 {
		t.Fatalf("chosen %d %+v", r.Chosen, r.Rows)
	}
}

func TestSizingDeterministic(t *testing.T) {
	a := sizeReport(szIndep(7, ""), szPending, 0, szCfg(3), 5)
	b := sizeReport(szIndep(7, ""), szPending, 0, szCfg(3), 5)
	if a.Chosen != b.Chosen || len(a.Rows) != len(b.Rows) {
		t.Fatal("nondeterministic")
	}
	for i := range a.Rows {
		if a.Rows[i] != b.Rows[i] {
			t.Fatal("nondeterministic rows")
		}
	}
}

// Replay of three completed samples from the supplied telemetry (selection-biased).
func TestSizingMeasuredReplay(t *testing.T) {
	samples := [][3]float64{{17.059, 16.441, .00647809}, {19.442, 18.964, .00701039}, {22.535, 21.792, .00693479}}
	var api, resid, cost []float64
	for _, s := range samples {
		api, resid, cost = append(api, s[1]), append(resid, s[0]-s[1]), append(cost, s[2])
	}
	med := func(x []float64) float64 { sort.Float64s(x); return x[len(x)/2] }
	c := &sizingConfig{Version: "sizing.v1", Calibration: &sizingCalibration{Source: "irrigator-boids-live-review metrics", NSamples: 3},
		Overhead: sizingOverhead{ResidualS: med(resid), Provenance: "measured"},
		Defaults: sizingEstimate{ServiceS: med(api), CostUSD: med(cost), Provenance: "measured"}}
	if err := validateSizing(c); err != nil {
		t.Fatal(err)
	}
	r := sizeReport(szIndep(8, ""), szPending, 0, c, 8)
	szSane(t, r)
	if r.Basis != "measured" || r.Chosen < 2 {
		t.Fatalf("%+v", r)
	}
}

func TestSizingValidation(t *testing.T) {
	bad := szCfg(1)
	bad.Defaults.ServiceS = -1
	if validateSizing(bad) == nil {
		t.Fatal("negative accepted")
	}
	bad = szCfg(1)
	bad.Defaults.ServiceS = math.Inf(1)
	if validateSizing(bad) == nil {
		t.Fatal("inf accepted")
	}
	bad = szCfg(1)
	bad.Defaults.Provenance = "measured"
	if validateSizing(bad) == nil {
		t.Fatal("measured without calibration accepted")
	}
	bad = szCfg(1)
	bad.ProviderBalanceUSD = map[string]float64{"haiku": -1}
	if validateSizing(bad) == nil {
		t.Fatal("negative balance accepted")
	}
	bad = szCfg(1)
	bad.Calibration = &sizingCalibration{Source: "x", NSamples: 1}
	bad.Overhead = sizingOverhead{ResidualS: 1, PerCallS: 1, Provenance: "measured"}
	if validateSizing(bad) == nil {
		t.Fatal("double-counted overhead accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	os.WriteFile(path, []byte(`{"version":"sizing.v1","bogus":1}`), 0600)
	if _, err := loadSizing(path); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func resCfg(bal map[string]float64) *sizingConfig {
	c := szCfg(10)
	c.ProviderBalanceUSD = bal
	return c
}

func TestGateReservationBalanceAndCap(t *testing.T) {
	ts := szIndep(3, "")
	s := szState(ts, "mock")
	g := &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	if out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25); len(out) != 1 {
		t.Fatalf("balance .30 cap .25 want 1, got %v", out)
	}
	// The runtime cap is what counts, not configuration or expected cost.
	g = &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	if out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .10); len(out) != 3 {
		t.Fatalf("cap .10 want 3, got %v", out)
	}
}

func TestGateReservationBusyAndExhausted(t *testing.T) {
	ts := szIndep(3, "")
	s := szState(ts, "mock")
	s.Tasks["t000"].Status = "running"
	g := &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	out, _ := g.Admit(s, []string{"t001", "t002"}, map[string]bool{"t000": true}, 3, .25)
	if len(out) != 0 {
		t.Fatalf("busy reservation must leave 0 new, got %v", out)
	}
	// Busy is subtracted once: balance .60 with one busy leaves room for 1.
	g = &sizingGate{cfg: resCfg(map[string]float64{"mock": .60})}
	if out, _ = g.Admit(s, []string{"t001", "t002"}, map[string]bool{"t000": true}, 3, .25); len(out) != 1 {
		t.Fatalf("want 1, got %v", out)
	}
	// Exhausted by recorded spend.
	s2 := szState(ts, "mock")
	s2.Tasks["t000"].Status, s2.Tasks["t000"].Cost = "verified", .30
	g = &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	if out, _ = g.Admit(s2, []string{"t001", "t002"}, map[string]bool{}, 4, .25); len(out) != 0 {
		t.Fatalf("exhausted must admit 0, got %v", out)
	}
	s2.Tasks["t000"].Cost = .10 // .20 left < .25
	g = &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	if out, _ = g.Admit(s2, []string{"t001"}, map[string]bool{}, 4, .25); len(out) != 0 {
		t.Fatalf("partial spend must admit 0, got %v", out)
	}
	s2.Tasks["t000"].Cost = .04 // .26 left
	g = &sizingGate{cfg: resCfg(map[string]float64{"mock": .30})}
	if out, _ = g.Admit(s2, []string{"t001"}, map[string]bool{}, 4, .25); len(out) != 1 {
		t.Fatalf("want 1, got %v", out)
	}
}

func TestGateReservationProvidersSeparateAndFailClosed(t *testing.T) {
	ts := szIndep(3, "")
	ts[0].Provider, ts[1].Provider, ts[2].Provider = "p1", "p1", "p2"
	s := szState(ts, "mock")
	g := &sizingGate{cfg: resCfg(map[string]float64{"p1": .30, "p2": .30})}
	out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25)
	if len(out) != 2 || out[0] != "t000" || out[1] != "t002" {
		t.Fatalf("one per provider expected, got %v", out)
	}
	// Backend fallback: tasks without Provider use state.Backend, absent here.
	ts2 := szIndep(2, "")
	s2 := szState(ts2, "mock")
	g = &sizingGate{cfg: resCfg(map[string]float64{"p1": 5})}
	if out, _ = g.Admit(s2, szIDs(ts2), map[string]bool{}, 4, .25); len(out) != 0 {
		t.Fatalf("unknown provider must fail closed, got %v", out)
	}
	g = &sizingGate{cfg: resCfg(map[string]float64{"mock": 5})}
	if out, _ = g.Admit(s2, szIDs(ts2), map[string]bool{}, 4, 0); len(out) != 0 {
		t.Fatalf("non-positive cap must fail closed, got %v", out)
	}
}

func TestGateWithoutReservationConfigUnchanged(t *testing.T) {
	ts := szIndep(3, "")
	s := szState(ts, "mock")
	g := &sizingGate{cfg: szCfg(10)}
	out, slots := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25)
	if len(out) != 3 || slots != 3 {
		t.Fatalf("%v %d", out, slots)
	}
	if out, _ = g.Admit(s, szIDs(ts), map[string]bool{}, 2, .25); len(out) != 2 {
		t.Fatalf("gate must never raise the worker limit: %v", out)
	}
}

func TestGateInfeasibleReturnsEmptyReady(t *testing.T) {
	ts := szIndep(2, "")
	s := szState(ts, "mock")
	c := szCfg(10)
	c.Defaults.CostUSD = .1
	b := .01
	c.BudgetUSD = &b
	g := &sizingGate{cfg: c}
	if out, sl := g.Admit(s, szIDs(ts), map[string]bool{}, 2, .25); sl != 0 || len(out) != 0 {
		t.Fatalf("%v %d", out, sl)
	}
	c2 := szCfg(10)
	c2.Defaults.RetryP = .1
	ts6 := szIndep(6, "")
	g = &sizingGate{cfg: c2}
	if out, sl := g.Admit(szState(ts6, "mock"), szIDs(ts6), map[string]bool{}, 4, .25); sl != 0 || len(out) != 0 {
		t.Fatalf("reliability infeasible must admit 0: %v %d", out, sl)
	}
}

func TestGateSerializesFileConflicts(t *testing.T) {
	ts := szIndep(2, "x.md")
	s := szState(ts, "mock")
	g := &sizingGate{cfg: szCfg(10)}
	if out, _ := g.Admit(s, szIDs(ts), map[string]bool{}, 4, .25); len(out) != 1 || out[0] != "t000" {
		t.Fatalf("%v", out)
	}
	if out, _ := g.Admit(s, []string{"t001"}, map[string]bool{"t000": true}, 3, .25); len(out) != 0 {
		t.Fatalf("%v", out)
	}
}

func TestSizingCacheKeyFingerprint(t *testing.T) {
	ts := szIndep(2, "")
	ts[1].DependsOn = []string{"t000"}
	s := szState(ts, "mock")
	base := szKey(s, ts, 2, -1)
	if szKey(s, ts, 2, -1) != base {
		t.Fatal("unstable key")
	}
	mut := func(name string, f func(s *model.State, ts []model.Task) (int, int)) {
		s2 := szState(ts, "mock")
		w, r := f(s2, ts)
		if szKey(s2, s2.AllTasks(), w, r) == base {
			t.Fatalf("key ignores %s", name)
		}
	}
	mut("waiting barrier", func(s *model.State, _ []model.Task) (int, int) { s.Tasks["t000"].Status = "waiting"; return 2, -1 })
	mut("running", func(s *model.State, _ []model.Task) (int, int) { s.Tasks["t000"].Status = "running"; return 2, -1 })
	mut("revision", func(s *model.State, _ []model.Task) (int, int) { s.GraphRevision = 3; return 2, -1 })
	mut("spend", func(s *model.State, _ []model.Task) (int, int) { s.Tasks["t000"].Cost = .1; return 2, -1 })
	mut("workers", func(s *model.State, _ []model.Task) (int, int) { return 3, -1 })
	mut("reservation cap", func(s *model.State, _ []model.Task) (int, int) { return 2, 1 })
	mut("dependencies", func(s *model.State, ts []model.Task) (int, int) {
		copyTasks := append([]model.Task(nil), ts...)
		copyTasks[1].DependsOn = nil
		s.Plan.Tasks = copyTasks
		return 2, -1
	})
}
