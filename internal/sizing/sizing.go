package sizing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

var ActiveSizingPath string

const maxSizingTasks = 1024

type sizingEstimate struct {
	ServiceS     float64 `json:"service_s,omitempty"`
	ServiceVarS2 float64 `json:"service_var_s2,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	RetryP       float64 `json:"retry_p,omitempty"`
	Calls        float64 `json:"calls,omitempty"`
	Tools        float64 `json:"tools,omitempty"`
	Provenance   string  `json:"provenance,omitempty"`
}
type sizingOverhead struct {
	PerCallS   float64 `json:"per_call_s,omitempty"`
	PerToolS   float64 `json:"per_tool_s,omitempty"`
	ResidualS  float64 `json:"residual_s,omitempty"`
	Provenance string  `json:"provenance,omitempty"`
}
type sizingCalibration struct {
	Source      string `json:"source"`
	CollectedAt string `json:"collected_at,omitempty"`
	NSamples    int    `json:"n_samples"`
	Note        string `json:"note,omitempty"`
}
type sizingConfig struct {
	Version          string   `json:"version"`
	Capacity         int      `json:"capacity,omitempty"`
	BudgetUSD        *float64 `json:"budget_usd,omitempty"`
	ReliabilityFloor float64  `json:"reliability_floor,omitempty"`
	RiskZ            float64  `json:"risk_z,omitempty"`
	Epsilon          float64  `json:"epsilon,omitempty"`
	ContentionGamma  float64  `json:"contention_gamma,omitempty"`
	// ProviderBalanceUSD is the configured starting balance per provider. When
	// non-empty, reservation mode is on: new calls are bounded by balance minus
	// recorded spend minus in-flight reservations (runtime per-call budget).
	ProviderBalanceUSD map[string]float64        `json:"provider_balance_usd,omitempty"`
	Overhead           sizingOverhead            `json:"overhead,omitempty"`
	Calibration        *sizingCalibration        `json:"calibration,omitempty"`
	Defaults           sizingEstimate            `json:"defaults,omitempty"`
	Tasks              map[string]sizingEstimate `json:"tasks,omitempty"`
}

type sizingRow struct {
	N             int     `json:"n"`
	MakespanS     float64 `json:"makespan_s"`
	ConservativeS float64 `json:"conservative_makespan_s"`
	Reliability   float64 `json:"reliability"`
}
type sizingReport struct {
	Basis              string         `json:"basis"`
	Chosen             int            `json:"chosen_n"`
	Feasible           bool           `json:"feasible"`
	Reason             string         `json:"reason"`
	Tasks              int            `json:"remaining_tasks"`
	CriticalPathS      float64        `json:"critical_path_s"`
	WorkS              float64        `json:"work_s"`
	ConflictBoundS     float64        `json:"conflict_bound_s"`
	ReadyWidth         int            `json:"ready_width_diagnostic"`
	ExpectedCostUSD    float64        `json:"expected_cost_usd"`
	BudgetRemainingUSD *float64       `json:"budget_remaining_estimate_usd,omitempty"`
	ShortfallUSD       float64        `json:"shortfall_usd,omitempty"`
	ReservationCapUSD  float64        `json:"reservation_cap_usd,omitempty"`
	ReservationSlots   map[string]int `json:"reservation_slots_estimate,omitempty"`
	ReservationN       *int           `json:"reservation_limited_n,omitempty"`
	Measured           int            `json:"measured_tasks"`
	Assumed            int            `json:"assumed_tasks"`
	Missing            int            `json:"missing_tasks"`
	Rows               []sizingRow    `json:"candidates,omitempty"`
	Notes              []string       `json:"notes"`
}

var sizingNotes = []string{
	"residual_s is measured elapsed-minus-API time per task call; it mixes network, tools, queueing and dispatch and is counted once.",
	"budget_usd is a configured estimate minus reported spend; the provider ledger remains authoritative and is not read here.",
	"reservation slots = floor((configured balance - recorded spend - in-flight reservations) / per-call budget); an estimate, the provider ledger decides.",
	"chosen N minimizes the conservative (variance-inflated) makespan among evaluated candidates (list-scheduling heuristic); not a global optimum or speedup claim.",
	"ready_width_diagnostic is the initially ready set size, a lower bound on maximum antichain width; it is not used as a cap.",
	"retry_p is an operator-supplied risk input; no automatic retry is ever added. Running tasks are modelled at full duration.",
}

func szFinite(name string, v, max float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > max {
		return fmt.Errorf("sizing: %s must be finite within 0..%g", name, max)
	}
	return nil
}
func validateEstimate(name string, e sizingEstimate) error {
	for _, c := range []struct {
		n    string
		v, m float64
	}{{"service_s", e.ServiceS, 1e7}, {"service_var_s2", e.ServiceVarS2, 1e14}, {"cost_usd", e.CostUSD, 1e6}, {"retry_p", e.RetryP, .5}, {"calls", e.Calls, 1e4}, {"tools", e.Tools, 1e4}} {
		if err := szFinite(name+"."+c.n, c.v, c.m); err != nil {
			return err
		}
	}
	if e.Provenance != "" && e.Provenance != "measured" && e.Provenance != "assumed" {
		return fmt.Errorf("sizing: %s provenance must be measured or assumed", name)
	}
	return nil
}
func validateSizing(c *sizingConfig) error {
	if c.Version != "sizing.v1" {
		return errors.New("sizing: version must be sizing.v1")
	}
	if c.Capacity < 0 || c.Capacity > 128 || len(c.Tasks) > 4096 || len(c.ProviderBalanceUSD) > 256 {
		return errors.New("sizing: capacity must be 0..128, tasks at most 4096, providers at most 256")
	}
	if c.BudgetUSD != nil {
		if err := szFinite("budget_usd", *c.BudgetUSD, 1e9); err != nil {
			return err
		}
	}
	for p, b := range c.ProviderBalanceUSD {
		if p == "" {
			return errors.New("sizing: empty provider name in provider_balance_usd")
		}
		if err := szFinite("provider_balance_usd."+p, b, 1e9); err != nil {
			return err
		}
	}
	for _, x := range []struct {
		n    string
		v, m float64
	}{{"reliability_floor", c.ReliabilityFloor, 1}, {"risk_z", c.RiskZ, 10}, {"epsilon", c.Epsilon, .5}, {"contention_gamma", c.ContentionGamma, 10},
		{"overhead.per_call_s", c.Overhead.PerCallS, 1e6}, {"overhead.per_tool_s", c.Overhead.PerToolS, 1e6}, {"overhead.residual_s", c.Overhead.ResidualS, 1e6}} {
		if err := szFinite(x.n, x.v, x.m); err != nil {
			return err
		}
	}
	if p := c.Overhead.Provenance; p != "" && p != "measured" && p != "assumed" {
		return errors.New("sizing: overhead provenance must be measured or assumed")
	}
	if c.Overhead.Provenance == "measured" && c.Overhead.ResidualS > 0 && (c.Overhead.PerCallS > 0 || c.Overhead.PerToolS > 0) {
		return errors.New("sizing: measured residual already contains dispatch/tool overhead; zero per_call_s/per_tool_s or mark them assumed")
	}
	measured := c.Overhead.Provenance == "measured" || c.Defaults.Provenance == "measured"
	if err := validateEstimate("defaults", c.Defaults); err != nil {
		return err
	}
	for id, e := range c.Tasks {
		if id == "" {
			return errors.New("sizing: empty task id")
		}
		if err := validateEstimate("tasks."+id, e); err != nil {
			return err
		}
		measured = measured || e.Provenance == "measured"
	}
	if measured && (c.Calibration == nil || c.Calibration.Source == "" || c.Calibration.NSamples < 1) {
		return errors.New("sizing: measured inputs require calibration.source and calibration.n_samples >= 1")
	}
	return nil
}
func loadSizing(path string) (*sizingConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1024*1024 {
		return nil, errors.New("sizing file exceeds 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c sizingConfig
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("sizing file must contain exactly one JSON object")
	}
	if err := validateSizing(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

func szPaths(t model.Task) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range t.Outputs {
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// reservationSlots returns, per configured provider, how many NEW calls fit:
// floor((balance - recorded spend - inflight*cap) / cap). capUSD is the runtime
// per-call budget. A non-positive cap admits nothing (fail closed).
func reservationSlots(c *sizingConfig, spent map[string]float64, inflight map[string]int, capUSD float64) map[string]int {
	if len(c.ProviderBalanceUSD) == 0 {
		return nil
	}
	out := map[string]int{}
	for p, bal := range c.ProviderBalanceUSD {
		n := 0
		// Balance, spend and cap are compared as exact micro-dollars.
		balance, balOK := cost.Limit(bal)
		spentMicros, spentOK := cost.Limit(spent[p])
		capMicros, capOK := cost.MicrosFromFloat(capUSD)
		if balOK && spentOK && capOK && capMicros > 0 && int64(inflight[p]) <= (math.MaxInt64-spentMicros)/capMicros {
			if rem := balance - spentMicros - int64(inflight[p])*capMicros; rem > 0 {
				n = int(rem / capMicros)
			}
		}
		out[p] = n
	}
	return out
}

// szSchedule is a deterministic list schedule: longest remaining path first,
// plan order on ties. Tasks sharing a conflict key (conf) are mutually
// exclusive instead of ordered, so conflicts cannot create cycles.
func szSchedule(n int, d []float64, preds, succs, conf [][]int, prio []float64, slots int) float64 {
	indeg := make([]int, n)
	for i := range preds {
		indeg[i] = len(preds[i])
	}
	started := make([]bool, n)
	fin := make([]float64, n)
	held := map[int]bool{}
	var running []int
	t, done := 0.0, 0
	for done < n {
		for len(running) < slots {
			best := -1
			for i := 0; i < n; i++ {
				if started[i] || indeg[i] != 0 {
					continue
				}
				blocked := false
				for _, p := range conf[i] {
					blocked = blocked || held[p]
				}
				if !blocked && (best < 0 || prio[i] > prio[best]+1e-12) {
					best = i
				}
			}
			if best < 0 {
				break
			}
			started[best] = true
			fin[best] = t + d[best]
			for _, p := range conf[best] {
				held[p] = true
			}
			running = append(running, best)
		}
		if len(running) == 0 {
			return math.MaxFloat64
		}
		k := 0
		for j, r := range running {
			if fin[r] < fin[running[k]] || (fin[r] == fin[running[k]] && r < running[k]) {
				k = j
			}
		}
		r := running[k]
		running = append(running[:k], running[k+1:]...)
		t = fin[r]
		done++
		for _, p := range conf[r] {
			delete(held, p)
		}
		for _, s := range succs[r] {
			indeg[s]--
		}
	}
	return t
}

// sizeReport evaluates remaining work. status maps task ID to run status;
// pending/running tasks carry work, waiting owners are zero-duration barriers,
// and every other status is excluded (no automatic retry of failed work).
// Budget and reliability safety are checked before any fallback; infeasible
// results have Chosen=0 and Feasible=false.
func sizeReport(tasks []model.Task, status func(string) string, spent float64, c *sizingConfig, workers int) sizingReport {
	rep := sizingReport{Chosen: 1, Feasible: true, Basis: "no-data-conservative", Notes: append([]string(nil), sizingNotes...)}
	var budget *float64
	if c.BudgetUSD != nil {
		b := math.Max(0, *c.BudgetUSD-spent)
		budget = &b
		rep.BudgetRemainingUSD = budget
	}
	idx := map[string]int{}
	var inc []model.Task
	for _, t := range tasks {
		switch status(t.ID) {
		case "pending", "running", "waiting":
			idx[t.ID] = len(inc)
			inc = append(inc, t)
		}
	}
	n := len(inc)
	rep.Tasks = n
	if n == 0 {
		rep.Basis, rep.Reason = "nothing-remaining", "no runnable work remains"
		return rep
	}
	if n > maxSizingTasks {
		rep.Chosen, rep.Feasible = 0, false
		rep.Basis, rep.Reason = "task-limit-refused", fmt.Sprintf("more than %d remaining tasks is unmodelled; refusing rather than claiming safety", maxSizingTasks)
		return rep
	}
	est := make([]sizingEstimate, n)
	barrier := make([]bool, n)
	maxKnown := 0.0
	for i, t := range inc {
		if status(t.ID) == "waiting" {
			barrier[i] = true
			continue
		}
		e, ok := c.Tasks[t.ID]
		if !ok {
			e = c.Defaults
		}
		est[i] = e
		switch {
		case e.ServiceS <= 0:
			rep.Missing++
		case e.Provenance == "measured":
			rep.Measured++
		default:
			rep.Assumed++
		}
		maxKnown = math.Max(maxKnown, e.ServiceS)
		rep.ExpectedCostUSD += e.CostUSD * (1 + e.RetryP)
	}
	eps, floor := c.Epsilon, c.ReliabilityFloor
	if eps == 0 {
		eps = .02
	}
	if floor == 0 {
		floor = .9
	}
	relia := func(N int) float64 {
		r := 1.0
		for i := range est {
			r *= 1 - math.Min(.99, est[i].RetryP*(1+c.ContentionGamma*float64(N-1)))
		}
		return r
	}
	// Safety first: neither fallback below may bypass these.
	if budget != nil && rep.ExpectedCostUSD > *budget+1e-12 {
		rep.Chosen, rep.Feasible = 0, false
		rep.ShortfallUSD = rep.ExpectedCostUSD - *budget
		rep.Reason = fmt.Sprintf("expected cost $%.4f exceeds configured remaining budget estimate $%.4f by $%.4f; no worker count changes cost, so nothing is admitted", rep.ExpectedCostUSD, *budget, rep.ShortfallUSD)
		return rep
	}
	if r1 := relia(1); r1 < floor {
		rep.Chosen, rep.Feasible = 0, false
		rep.Reason = fmt.Sprintf("predicted reliability %.4f is below floor %.4f even at N=1; nothing is admitted, no retry added", r1, floor)
		return rep
	}
	if rep.Measured+rep.Assumed == 0 {
		rep.Reason = "no service-time data for remaining tasks; conservative N=1, no speed claim"
		return rep
	}
	if rep.Missing > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d tasks lack service_s; filled with the largest known value %.3fs (assumed).", rep.Missing, maxKnown))
	}
	o, z := c.Overhead, c.RiskZ
	if z == 0 {
		z = 1
	}
	d, du := make([]float64, n), make([]float64, n)
	for i := range inc {
		if barrier[i] {
			continue
		}
		e := est[i]
		if e.ServiceS <= 0 {
			e.ServiceS = maxKnown
		}
		k := e.Calls
		if k <= 0 {
			k = 1
		}
		base := e.ServiceS + k*o.PerCallS + e.Tools*o.PerToolS + o.ResidualS
		m := 1 + e.RetryP
		d[i] = base * m
		du[i] = (base + z*math.Sqrt(e.ServiceVarS2)) * m
		rep.WorkS += d[i]
	}
	preds, succs, conf := make([][]int, n), make([][]int, n), make([][]int, n)
	add := func(a, b int) { preds[b] = append(preds[b], a); succs[a] = append(succs[a], b) }
	byPath := map[string][]int{}
	pathID := map[string]int{}
	for i, t := range inc {
		for _, dep := range t.DependsOn {
			if j, ok := idx[dep]; ok {
				add(j, i)
			}
		}
		if j, ok := idx[t.Parent]; ok && t.Parent != "" {
			add(i, j)
		}
		for _, p := range szPaths(t) {
			id, ok := pathID[p]
			if !ok {
				id = len(pathID)
				pathID[p] = id
			}
			conf[i] = append(conf[i], id)
			byPath[p] = append(byPath[p], i)
		}
	}
	for _, l := range byPath {
		sum := 0.0
		if len(l) > 1 {
			for _, i := range l {
				sum += d[i]
			}
		}
		rep.ConflictBoundS = math.Max(rep.ConflictBoundS, sum)
	}
	indeg := make([]int, n)
	var order []int
	for i := range preds {
		indeg[i] = len(preds[i])
		if indeg[i] == 0 {
			order = append(order, i)
			rep.ReadyWidth++
		}
	}
	for h := 0; h < len(order); h++ {
		for _, s := range succs[order[h]] {
			if indeg[s]--; indeg[s] == 0 {
				order = append(order, s)
			}
		}
	}
	if len(order) < n {
		rep.Reason = "dependency/ownership graph has a cycle; conservative N=1"
		return rep
	}
	bl := make([]float64, n)
	for k := n - 1; k >= 0; k-- {
		i, best := order[k], 0.0
		for _, s := range succs[i] {
			best = math.Max(best, bl[s])
		}
		bl[i] = d[i] + best
		rep.CriticalPathS = math.Max(rep.CriticalPathS, bl[i])
	}
	capN := workers
	if c.Capacity > 0 && c.Capacity < capN {
		capN = c.Capacity
	}
	maxN := int(math.Min(float64(n), math.Max(1, float64(capN))))
	for N := 1; N <= maxN; N++ {
		rep.Rows = append(rep.Rows, sizingRow{N, szSchedule(n, d, preds, succs, conf, bl, N), szSchedule(n, du, preds, succs, conf, bl, N), relia(N)})
	}
	rep.Basis = "measured"
	if rep.Assumed > 0 || rep.Missing > 0 || ((o.PerCallS > 0 || o.PerToolS > 0 || o.ResidualS > 0) && o.Provenance != "measured") {
		rep.Basis = "assumed-inputs"
	}
	// Selection uses the conservative (variance-inflated) makespan.
	minT := math.MaxFloat64
	for _, r := range rep.Rows {
		if r.Reliability >= floor {
			minT = math.Min(minT, r.ConservativeS)
		}
	}
	if minT >= math.MaxFloat64 {
		rep.Chosen, rep.Feasible = 0, false
		rep.Reason = "no evaluated candidate meets the reliability floor; nothing is admitted"
		return rep
	}
	for _, r := range rep.Rows {
		if r.Reliability >= floor && r.ConservativeS <= (1+eps)*minT {
			rep.Chosen = r.N
			break
		}
	}
	rep.Reason = fmt.Sprintf("smallest N within %.1f%% of the minimum conservative makespan among evaluated N=1..%d", eps*100, maxN)
	return rep
}

// sizingGate enforces a report at controller admission. It only lowers slots.
type sizingGate struct {
	cfg *sizingConfig
	key string
	rep sizingReport
}

func LoadRunSizing(root, path string) (*sizingGate, error) {
	side := filepath.Join(root, "sizing.json")
	explicit := path != ""
	if !explicit {
		if _, err := os.Stat(side); err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		path = side
	}
	cfg, err := loadSizing(path)
	if err != nil {
		return nil, err
	}
	if explicit {
		if err := jsonio.AtomicJSON(side, cfg); err != nil {
			return nil, err
		}
	}
	return &sizingGate{cfg: cfg}, nil
}

// szKey is a deterministic fingerprint of everything the report depends on.
func szKey(s *model.State, tasks []model.Task, workers, resTotal int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "r%d|w%d|res%d|", s.GraphRevision, workers, resTotal)
	for _, t := range tasks {
		row := s.Tasks[t.ID]
		fmt.Fprintf(&b, "%s:%s:%s:%v:%v:%.6f;", t.ID, row.Status, t.Parent, t.DependsOn, szPaths(t), row.Cost)
	}
	for _, a := range s.ArchivedTasks {
		fmt.Fprintf(&b, "A%s:%s:%.6f;", a.Spec.ID, a.Spec.Provider, a.Cost)
	}
	return b.String()
}

// admit returns the subset of ready that may start now. capUSD is the runtime
// per-call budget (opts.Budget). Infeasible reports and exhausted capacity
// return an empty list so the controller can block instead of spinning.
func (g *sizingGate) Admit(s *model.State, ready []string, busy map[string]bool, slots int, capUSD float64) ([]string, int) {
	tasks := s.AllTasks()
	specs := map[string]model.Task{}
	for _, t := range tasks {
		specs[t.ID] = t
	}
	prov := func(id string) string {
		if p := specs[id].Provider; p != "" {
			return p
		}
		return s.Backend
	}
	if len(g.cfg.ProviderBalanceUSD) == 0 {
		fmt.Fprintln(os.Stderr, "sizing: provider_balance_usd missing; reservation cannot be bounded, admitting nothing (fail closed)")
		return nil, 0
	}
	var res map[string]int
	resTotal := -1
	if len(g.cfg.ProviderBalanceUSD) > 0 {
		spentMicros, inflight := map[string]int64{}, map[string]int{}
		for id, row := range s.Tasks {
			spentMicros[prov(id)] += row.CostMicros()
		}
		for _, a := range s.ArchivedTasks {
			p := a.Spec.Provider
			if p == "" {
				p = s.Backend
			}
			spentMicros[p] += a.CostMicros()
		}
		spent := map[string]float64{}
		for p, m := range spentMicros {
			spent[p] = cost.MicrosToFloat(m)
		}
		for id := range busy {
			inflight[prov(id)]++
		}
		res = reservationSlots(g.cfg, spent, inflight, capUSD)
		resTotal = 0
		for _, n := range res {
			resTotal += n
		}
	}
	workers := slots + len(busy)
	key := szKey(s, tasks, workers, resTotal)
	if key != g.key {
		g.key = key
		g.rep = sizeReport(tasks, func(id string) string { return s.Tasks[id].Status }, s.ReportedCost(), g.cfg, workers)
		if !g.rep.Feasible {
			fmt.Fprintln(os.Stderr, "sizing:", g.rep.Reason)
		}
	}
	if !g.rep.Feasible {
		return nil, 0
	}
	allowed := g.rep.Chosen - len(busy)
	if allowed > slots {
		allowed = slots
	}
	if allowed <= 0 {
		return nil, 0
	}
	claimed := map[string]bool{}
	for id := range busy {
		for _, p := range szPaths(specs[id]) {
			claimed[p] = true
		}
	}
	var out []string
	for _, id := range ready {
		if len(out) >= allowed {
			break
		}
		if res != nil {
			if n, ok := res[prov(id)]; !ok || n <= 0 {
				continue // fail closed: unknown provider or no reservation capacity
			}
		}
		ok := true
		paths := szPaths(specs[id])
		for _, p := range paths {
			ok = ok && !claimed[p]
		}
		if !ok {
			continue
		}
		out = append(out, id)
		if res != nil {
			res[prov(id)]--
		}
		for _, p := range paths {
			claimed[p] = true
		}
	}
	return out, allowed
}

func SizeCLI(planPath, sizingPath string, asJSON bool, budget float64, backend string) (int, error) {
	if sizingPath == "" {
		return 2, errors.New("--sizing is required")
	}
	p, err := checkpoint.LoadPlan(planPath)
	if err != nil {
		return 1, err
	}
	cfg, err := loadSizing(sizingPath)
	if err != nil {
		return 1, err
	}
	rep := sizeReport(p.Tasks, func(string) string { return "pending" }, 0, cfg, 128)
	if len(cfg.ProviderBalanceUSD) > 0 {
		used := map[string]bool{}
		for _, t := range p.Tasks {
			pr := t.Provider
			if pr == "" {
				pr = backend
			}
			used[pr] = true
		}
		var names []string
		for pr := range used {
			names = append(names, pr)
		}
		sort.Strings(names)
		slots := reservationSlots(cfg, nil, nil, budget)
		sum := 0
		missingProv := false
		for _, pr := range names {
			n, ok := slots[pr]
			if !ok {
				missingProv = true
				rep.Notes = append(rep.Notes, fmt.Sprintf("provider %q has no provider_balance_usd entry; runtime fails closed for its tasks.", pr))
			}
			sum += n
		}
		lim := rep.Chosen
		if sum < lim {
			lim = sum
		}
		rep.ReservationCapUSD, rep.ReservationSlots, rep.ReservationN = budget, slots, &lim
		if rep.Feasible && (missingProv || lim == 0) {
			rep.Feasible = false
			rep.Reason = fmt.Sprintf("infeasible reservation: runtime would admit nothing (missing provider balance entry=%v, reservation-limited N=%d); unconstrained predicted N=%d retained as evidence only", missingProv, lim, rep.Chosen)
		}
	}
	if len(cfg.ProviderBalanceUSD) == 0 {
		zero := 0
		rep.ReservationN = &zero
		if rep.Feasible {
			rep.Feasible = false
			rep.Reason = fmt.Sprintf("infeasible reservation: provider_balance_usd is missing and the runtime fails closed; unconstrained predicted N=%d retained as evidence only", rep.Chosen)
		}
		rep.Notes = append(rep.Notes, "warning: provider_balance_usd is missing; the model above is unconstrained, but the runtime fails closed (effective recommendation 0).")
	}
	if asJSON {
		return 0, jsonio.PrintJSON(rep)
	}
	fmt.Printf("basis=%s chosen_n=%d feasible=%v\n%s\n", rep.Basis, rep.Chosen, rep.Feasible, rep.Reason)
	fmt.Printf("tasks=%d critical_path=%.3fs work=%.3fs conflict_bound=%.3fs ready_width(diagnostic)=%d expected_cost=$%.4f\n", rep.Tasks, rep.CriticalPathS, rep.WorkS, rep.ConflictBoundS, rep.ReadyWidth, rep.ExpectedCostUSD)
	if rep.BudgetRemainingUSD != nil {
		fmt.Printf("budget estimate=$%.4f shortfall=$%.4f (provider ledger is authoritative)\n", *rep.BudgetRemainingUSD, rep.ShortfallUSD)
	}
	if rep.ReservationN != nil {
		fmt.Printf("reservation cap=$%.4f per call; reservation-limited N=%d slots=%v (estimate; provider ledger is authoritative)\n", rep.ReservationCapUSD, *rep.ReservationN, rep.ReservationSlots)
	}
	fmt.Printf("data: measured=%d assumed=%d missing=%d\n  N  makespan_s  conservative_s  reliability\n", rep.Measured, rep.Assumed, rep.Missing)
	for _, r := range rep.Rows {
		fmt.Printf("%3d  %10.3f  %14.3f  %11.4f\n", r.N, r.MakespanS, r.ConservativeS, r.Reliability)
	}
	for _, n := range rep.Notes {
		fmt.Println("note:", n)
	}
	return 0, nil
}
