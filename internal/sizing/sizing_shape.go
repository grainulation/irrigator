package sizing

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/jsonio"
)

func ParseShape(s string) (checkpoint.Shape, error) {
	norm := strings.NewReplacer("×", "x", "X", "x").Replace(strings.TrimSpace(s))
	parts := strings.Split(norm, "x")
	if len(parts) < 2 || len(parts) > 3 {
		return checkpoint.Shape{}, fmt.Errorf("shape %q must be AxDxS or WxD", s)
	}
	v := []int{0, 0, 1} // 2D W×D aliases W×D×1
	for i, p := range parts {
		if p == "" || strings.HasPrefix(p, "+") {
			return checkpoint.Shape{}, fmt.Errorf("shape %q has an invalid component %q", s, p)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return checkpoint.Shape{}, fmt.Errorf("shape %q has a non-integer or out-of-range component %q", s, p)
		}
		if n < 1 {
			return checkpoint.Shape{}, fmt.Errorf("shape %q components must be at least 1", s)
		}
		v[i] = n
	}
	sh := checkpoint.Shape{Roots: v[0], Levels: v[1], MaxChildren: v[2]}
	if sh.Levels > checkpoint.MaxShapeDepth {
		return checkpoint.Shape{}, fmt.Errorf("shape depth %d exceeds %d levels", sh.Levels, checkpoint.MaxShapeDepth)
	}
	if sh.MaxChildren > checkpoint.MaxShapeNodes {
		return checkpoint.Shape{}, fmt.Errorf("shape max children %d exceeds %d", sh.MaxChildren, checkpoint.MaxShapeNodes)
	}
	if _, err := checkpoint.ShapeMaxNodes(sh); err != nil {
		return checkpoint.Shape{}, err
	}
	return sh, nil
}

type shapeReport struct {
	Shape              string         `json:"shape"`
	Roots              int            `json:"roots"`
	Levels             int            `json:"levels"`
	MaxChildren        int            `json:"max_children"`
	MaxNodes           int            `json:"max_nodes"`
	WidestLevel        int            `json:"widest_level"`
	Basis              string         `json:"basis"`
	CostRootsOnlyUSD   float64        `json:"cost_roots_only_usd"`
	CostFullUSD        float64        `json:"cost_full_expansion_usd"`
	BudgetUSD          *float64       `json:"budget_usd,omitempty"`
	Feasible           bool           `json:"feasible"`
	Reason             string         `json:"reason"`
	FullWithinBudget   bool           `json:"full_expansion_within_budget"`
	FullMeetsFloor     bool           `json:"full_expansion_meets_reliability_floor"`
	PredictedWorkers   int            `json:"predicted_workers"`
	RecommendedWorkers int            `json:"recommended_workers"`
	ReservationCapUSD  float64        `json:"reservation_cap_usd"`
	ReservationSlots   map[string]int `json:"reservation_slots_estimate,omitempty"`
	ReservationN       *int           `json:"reservation_limited_n,omitempty"`
	FrontierBoundS     *float64       `json:"modelled_frontier_upper_bound_s,omitempty"`
	Uncertainty        string         `json:"uncertainty"`
	Notes              []string       `json:"notes"`
}

func buildShapeReport(sh checkpoint.Shape, cfg *sizingConfig, capUSD float64, backend string) (shapeReport, error) {
	nodes, err := checkpoint.ShapeMaxNodes(sh)
	if err != nil {
		return shapeReport{}, err
	}
	widest := sh.Roots
	for i := 1; i < sh.Levels; i++ {
		widest *= sh.MaxChildren
	}
	rep := shapeReport{Shape: fmt.Sprintf("%dx%dx%d", sh.Roots, sh.Levels, sh.MaxChildren), Roots: sh.Roots, Levels: sh.Levels,
		MaxChildren: sh.MaxChildren, MaxNodes: nodes, WidestLevel: widest, Basis: "no-data-conservative", Feasible: true,
		ReservationCapUSD: capUSD,
		Uncertainty:       "maximum is an upper bound, not a promised expansion; actual expansion, dependencies and durations are unknown until runtime",
		Notes: []string{
			"coordinator is excluded; workers/concurrency are separate from nodes.",
			"ownership depth is NOT the dependency critical path; the frontier figure is a modelled scenario upper bound, not an exact makespan.",
			"the runtime recomputes sizing from the materialized DAG; this dry run never runs work or mutates a plan.",
		}}
	e := cfg.Defaults
	q := e.RetryP
	m := 1 + q
	rep.CostRootsOnlyUSD = float64(sh.Roots) * e.CostUSD * m
	rep.CostFullUSD = float64(nodes) * e.CostUSD * m
	rep.FullWithinBudget, rep.FullMeetsFloor = true, true
	floor, eps, z := cfg.ReliabilityFloor, cfg.Epsilon, cfg.RiskZ
	if floor == 0 {
		floor = .9
	}
	if eps == 0 {
		eps = .02
	}
	if z == 0 {
		z = 1
	}
	relia := func(N, count int) float64 {
		return math.Pow(1-math.Min(.99, q*(1+cfg.ContentionGamma*float64(N-1))), float64(count))
	}
	rep.FullMeetsFloor = relia(1, nodes) >= floor
	if cfg.BudgetUSD != nil {
		b := *cfg.BudgetUSD
		rep.BudgetUSD = &b
		rep.FullWithinBudget = rep.CostFullUSD <= b+1e-12
		if rep.CostRootsOnlyUSD > b+1e-12 {
			rep.Feasible = false
			rep.Reason = fmt.Sprintf("infeasible budget: roots-only cost $%.4f exceeds budget $%.4f by $%.4f", rep.CostRootsOnlyUSD, b, rep.CostRootsOnlyUSD-b)
		}
	}
	if rep.Feasible && relia(1, sh.Roots) < floor {
		rep.Feasible = false
		rep.Reason = fmt.Sprintf("infeasible reliability: roots-only reliability %.4f is below floor %.4f even at N=1", relia(1, sh.Roots), floor)
	}
	if !rep.FullWithinBudget && rep.Feasible {
		rep.Notes = append(rep.Notes, "warning: full expansion would exceed the budget; the runtime gate recomputes from the materialized DAG and may then admit nothing.")
	}
	if !rep.FullMeetsFloor && rep.Feasible {
		rep.Notes = append(rep.Notes, "warning: full expansion would fall below the reliability floor.")
	}
	if rep.Feasible {
		rep.PredictedWorkers = 1
		rep.Reason = "no service-time data; conservative N=1, no speed claim"
		if e.ServiceS > 0 {
			o := cfg.Overhead
			k := e.Calls
			if k <= 0 {
				k = 1
			}
			base := e.ServiceS + k*o.PerCallS + e.Tools*o.PerToolS + o.ResidualS
			d := (base + z*math.Sqrt(e.ServiceVarS2)) * m
			work, cp := float64(nodes)*d, float64(sh.Levels)*d
			T := func(N int) float64 { return work/float64(N) + (1-1/float64(N))*cp }
			maxN := widest
			if cfg.Capacity > 0 && cfg.Capacity < maxN {
				maxN = cfg.Capacity
			}
			if maxN > 128 {
				maxN = 128
			}
			minT := math.MaxFloat64
			for N := 1; N <= maxN; N++ {
				if relia(N, sh.Roots) >= floor {
					minT = math.Min(minT, T(N))
				}
			}
			for N := 1; N <= maxN && minT < math.MaxFloat64; N++ {
				if relia(N, sh.Roots) >= floor && T(N) <= (1+eps)*minT {
					rep.PredictedWorkers = N
					break
				}
			}
			if minT >= math.MaxFloat64 {
				rep.Feasible, rep.PredictedWorkers = false, 0
				rep.Reason = "infeasible reliability: no worker count meets the reliability floor"
			} else {
				f := T(rep.PredictedWorkers)
				rep.FrontierBoundS = &f
				rep.Basis = "assumed-inputs"
				if e.Provenance == "measured" {
					rep.Basis = "measured"
				}
				rep.Reason = "smallest N within the epsilon band of the modelled conservative frontier bound (scenario only: assumes a full ownership tree, identical task durations and NO extra dependency or file-conflict edges; not an actual critical path or a bound for arbitrary graphs)"
				rep.Notes = append(rep.Notes, "frontier T(N) uses CP=D*d under the stated assumptions (full ownership tree, identical durations, no extra dependency/file-conflict edges); it is a scenario bound only.")
			}
		}
	}
	rep.RecommendedWorkers = rep.PredictedWorkers
	if len(cfg.ProviderBalanceUSD) == 0 {
		zero := 0
		rep.ReservationN, rep.RecommendedWorkers = &zero, 0
		if rep.Feasible {
			rep.Feasible = false
			rep.Reason = fmt.Sprintf("infeasible reservation: provider_balance_usd is missing and the runtime fails closed; unconstrained predicted N=%d retained as evidence only", rep.PredictedWorkers)
		}
		rep.Notes = append(rep.Notes, "warning: provider_balance_usd is missing; the unconstrained model is shown, but the runtime fails closed and the effective recommendation is 0.")
	} else {
		slots := reservationSlots(cfg, nil, nil, capUSD)
		rep.ReservationSlots = slots
		n, ok := slots[backend]
		if !ok {
			rep.Notes = append(rep.Notes, fmt.Sprintf("provider %q has no provider_balance_usd entry; runtime fails closed.", backend))
		}
		lim := rep.PredictedWorkers
		if n < lim {
			lim = n
		}
		rep.ReservationN, rep.RecommendedWorkers = &lim, lim
		if rep.Feasible && (!ok || lim == 0) {
			rep.Feasible = false
			rep.Reason = fmt.Sprintf("infeasible reservation: runtime would admit nothing (provider entry present=%v, reservation-limited N=%d); unconstrained predicted N=%d retained as evidence only", ok, lim, rep.PredictedWorkers)
		}
	}
	{
		nEff := rep.RecommendedWorkers
		if nEff < 1 {
			nEff = rep.PredictedWorkers
		}
		if nEff < 1 {
			nEff = 1
		}
		rep.FullMeetsFloor = relia(nEff, nodes) >= floor
	}
	return rep, nil
}

func SizeShapeCLI(shape, sizingPath string, asJSON bool, budget float64, backend string) (int, error) {
	if sizingPath == "" {
		return 2, errors.New("--sizing is required")
	}
	sh, err := ParseShape(shape)
	if err != nil {
		return 2, err
	}
	cfg, err := loadSizing(sizingPath)
	if err != nil {
		return 1, err
	}
	rep, err := buildShapeReport(sh, cfg, budget, backend)
	if err != nil {
		return 2, err
	}
	if asJSON {
		return 0, jsonio.PrintJSON(rep)
	}
	fmt.Printf("recursive shape=%s max_nodes=%d levels=%d roots=%d max_children=%d widest_level=%d\n", rep.Shape, rep.MaxNodes, rep.Levels, rep.Roots, rep.MaxChildren, rep.WidestLevel)
	fmt.Printf("cost range: roots-only $%.4f .. full-expansion $%.4f (maximum is not a promised expansion)\n", rep.CostRootsOnlyUSD, rep.CostFullUSD)
	fmt.Printf("feasible=%v basis=%s\n%s\n", rep.Feasible, rep.Basis, rep.Reason)
	fmt.Printf("workers: predicted=%d recommended=%d (reservation cap $%.4f per call)\n", rep.PredictedWorkers, rep.RecommendedWorkers, rep.ReservationCapUSD)
	if rep.FrontierBoundS != nil {
		fmt.Printf("modelled frontier upper bound (scenario, not exact makespan): %.3fs\n", *rep.FrontierBoundS)
	}
	fmt.Println("uncertainty:", rep.Uncertainty)
	for _, n := range rep.Notes {
		fmt.Println("note:", n)
	}
	return 0, nil
}
