package admission

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the findings of the second review pass.

func TestSlotRampWaitsOutItsOwnDwellForALaterLoosening(t *testing.T) {
	clk := newFakeClock()
	s := newSmoother(clk, 10*time.Second, 2*time.Second)
	apply := func(local int) int { return s.apply(target(local), 8).MaxLocalSlots }
	if got := apply(1); got != 1 {
		t.Fatal(got)
	}
	clk.Advance(time.Second)
	if got := apply(2); got != 1 { // looser, but the dwell has only begun
		t.Fatalf("relaxed before the dwell: %d", got)
	}
	clk.Advance(10 * time.Second)
	if got := apply(2); got != 2 {
		t.Fatalf("did not reach 2 after the dwell: %d", got)
	}
	// A new, later loosening has to wait out a dwell of its own, not inherit the
	// time since the first one.
	clk.Advance(2 * time.Second)
	if got := apply(8); got != 2 {
		t.Fatalf("a fresh loosening skipped its dwell: %d", got)
	}
	clk.Advance(9 * time.Second)
	if got := apply(8); got != 2 {
		t.Fatalf("relaxed after only 9s of a 10s dwell: %d", got)
	}
	clk.Advance(time.Second)
	if got := apply(8); got != 3 {
		t.Fatalf("did not step after its dwell: %d", got)
	}
}

func TestSlotRampWorksWhenTheClockStartsAtTheZeroTime(t *testing.T) {
	clk := newFakeClock()
	clk.now = time.Time{}
	s := newSmoother(clk, 10*time.Second, 5*time.Second)
	apply := func(local int) int { return s.apply(target(local), 4).MaxLocalSlots }
	if apply(1) != 1 || apply(4) != 1 {
		t.Fatal("setup")
	}
	clk.Advance(9 * time.Second)
	if got := apply(4); got != 1 {
		t.Fatalf("stepped inside the dwell: %d", got)
	}
	clk.Advance(time.Second)
	if got := apply(4); got != 2 {
		t.Fatalf("first step after the dwell: %d", got)
	}
	clk.Advance(time.Second)
	if got := apply(4); got != 2 {
		t.Fatalf("stepped inside the throttle: %d", got)
	}
	clk.Advance(4 * time.Second)
	if got := apply(4); got != 3 {
		t.Fatalf("second step after the throttle: %d", got)
	}
}

func TestAIMDDoesNotGrowPastTheClampBetweenObservations(t *testing.T) {
	a := newAIMD(8, 1, 1)
	env := open()
	env.MaxRemoteSlots = 2 // another sensor binds at two remote slots
	a.observe(env, remote)
	if a.limit != 3 {
		t.Fatalf("clamp = %d, want effective + headroom", a.limit)
	}
	for i := 0; i < 6; i++ {
		a.onSuccess()
	}
	if a.limit != 3 {
		t.Fatalf("successes under a binding constraint wound the limit up to %d", a.limit)
	}
	a.observe(open(), remote) // the constraint is gone
	a.onSuccess()
	if a.limit != 4 {
		t.Fatalf("growth did not resume once the clamp lifted: %d", a.limit)
	}
}

func TestBreakerStaysHalfOpenAfterASuccessfulProbe(t *testing.T) {
	clk := newFakeClock()
	// A slow decay keeps the rate above the threshold so that only the kind of
	// the probe's outcome can decide.
	b := newBreaker(breakerConfig{Alpha: 0.1, Threshold: 0.5, MinOutcomes: 5, IntervalSeconds: 20, OpenSeconds: 60, HalfLifeSeconds: 1e6}, 4, clk)
	b.observeOutcomes(fail(20), clk.Now())
	for i := 0; i < 80 && b.sample(context.Background(), runSnapshot{}).hold != HoldTransient; i++ {
		clk.Advance(5 * time.Second)
	}
	opened := b.openUntil
	if opened.IsZero() {
		t.Fatal("the breaker never opened")
	}
	for clk.Now().Before(opened) {
		clk.Advance(5 * time.Second)
	}
	clk.Advance(time.Second)            // strictly after the open period
	b.observeOutcomes(ok(1), clk.Now()) // the half-open probe succeeds
	clk.Advance(time.Second)
	if got := b.sample(context.Background(), runSnapshot{}); got.hold != holdNone || !b.openUntil.Equal(opened) {
		t.Fatalf("a successful probe re-opened the breaker: %+v (openUntil %v, was %v)", got, b.openUntil, opened)
	}
	clk.Advance(time.Second)
	b.observeOutcomes(fail(1), clk.Now()) // a failed probe does
	clk.Advance(time.Second)
	if got := b.sample(context.Background(), runSnapshot{}); got.hold != HoldTransient || !b.openUntil.After(opened) {
		t.Fatalf("a failed probe did not re-open the breaker: %+v", got)
	}
}

func TestOneBurstOfRateLimitFailuresHalvesTheLimitOnce(t *testing.T) {
	clk := newFakeClock()
	f := &limitFake{evidence: LimitEvidence{Limited: true}}
	r := newTestRateLimit(clk, f)
	defer r.Close()
	var burst []outcome
	for _, id := range []string{"r1", "r2", "r3", "r4"} {
		burst = append(burst, outcome{task: id, attempt: 1, kind: outcomeFailure, locality: remote})
	}
	r.observeOutcomes(burst, clk.Now())
	waitUntil(t, r.idle)
	r.mu.Lock()
	got := r.aimd.limit
	r.mu.Unlock()
	if got != 3 {
		t.Fatalf("four failures from one provider limit left %d of 6 slots, want one halving to 3", got)
	}
}

func TestAProviderResetThatHasAlreadyPassedIsNoCooldown(t *testing.T) {
	clk := newFakeClock()
	past := clk.Now().Add(-time.Second)
	f := &limitFake{evidence: LimitEvidence{Limited: true, ResetAt: &past}}
	r := newTestRateLimit(clk, f)
	defer r.Close()
	r.observeOutcomes([]outcome{{task: "r1", attempt: 1, kind: outcomeFailure, locality: remote}}, clk.Now())
	waitUntil(t, r.idle)
	got := r.sample(context.Background(), runSnapshot{})
	if got.maxRemoteSlots == 0 {
		t.Fatalf("a reset in the past still imposed a cooldown: %+v", got)
	}
	if got.maxRemoteSlots != 3 {
		t.Fatalf("the limit should still halve and recover: %+v", got)
	}
}

func TestAFailedStatusWriteIsRetriedAfterTheDebounce(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := statusWriter{path: filepath.Join(blocker, StatusFile), debounce: time.Second, preset: "balanced"}
	now := time.Now()
	env := open()
	env.MaxLocalSlots = 1
	w.observe(now, env, runSnapshot{}, 4)
	if w.Err == nil || w.wrote {
		t.Fatalf("the write should have failed and not be recorded as written: %v %v", w.Err, w.wrote)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	w.observe(now.Add(100*time.Millisecond), env, runSnapshot{}, 4) // inside the debounce
	if w.wrote {
		t.Fatal("retried inside the debounce")
	}
	w.observe(now.Add(2*time.Second), env, runSnapshot{}, 4)
	if w.Err != nil || !w.wrote {
		t.Fatalf("not retried after the debounce: %v", w.Err)
	}
	if _, err := os.Stat(filepath.Join(blocker, StatusFile)); err != nil {
		t.Fatal(err)
	}
}

func TestAControllerThatStartsClearsTheStatusOfADeadOne(t *testing.T) {
	for _, spec := range []string{"", "unbounded"} {
		root := t.TempDir()
		stale := filepath.Join(root, StatusFile)
		if err := os.WriteFile(stale, []byte(`{"schema":"irrigator.admission-status/v1"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		e, err := LoadRun(root, spec, 4, Deps{})
		if err != nil || e != nil {
			t.Fatalf("spec %q: %v %v", spec, e, err)
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("spec %q: the dead controller's status file is still there", spec)
		}
	}
}

func TestAConfigFileThatLimitsNothingIsRejected(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, body := range map[string]string{
		"preset-only.json":   `{"schema":"irrigator.admission/v1","preset":"balanced"}`,
		"empty-disk.json":    `{"schema":"irrigator.admission/v1","disk":{}}`,
		"zero-active.json":   `{"schema":"irrigator.admission/v1","presence":{"active_local_slots":1}}`,
		"only-dwell.json":    `{"schema":"irrigator.admission/v1","exit_dwell_seconds":5}`,
		"disk-no-limit.json": `{"schema":"irrigator.admission/v1","disk":{"low_local_slots":1}}`,
	} {
		if _, err := resolve(write(name, body)); err == nil {
			t.Errorf("%s: a config that limits nothing was accepted", name)
		}
	}
	if _, err := resolve(write("unbounded.json", `{"schema":"irrigator.admission/v1","preset":"unbounded"}`)); err != nil {
		t.Errorf("an explicit unbounded file must be accepted: %v", err)
	}
	if _, err := resolve(write("ok.json", `{"schema":"irrigator.admission/v1","disk":{"critical_free_mb":512,"low_free_mb":2048}}`)); err != nil {
		t.Errorf("a real disk limit was rejected: %v", err)
	}
}

func TestValidateRejectsNonFiniteNumbers(t *testing.T) {
	for name, cfg := range map[string]config{
		"dwell nan":       {Schema: configSchema, ExitDwellSeconds: math.NaN()},
		"throttle inf":    {Schema: configSchema, RampThrottleSeconds: math.Inf(1)},
		"alpha nan":       {Schema: configSchema, Breaker: &breakerConfig{Alpha: math.NaN(), Threshold: .5, MinOutcomes: 1, IntervalSeconds: 1, OpenSeconds: 1, HalfLifeSeconds: 1}},
		"presence active": {Schema: configSchema, Presence: &presenceConfig{ActiveWithinSeconds: math.NaN()}},
	} {
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "finite") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestThermalCriticalWithoutAHoldKeepsTheTierCap(t *testing.T) {
	cfg := powerConfig{ThermalCriticalHold: false, ThermalSeriousMaxTier: "minimal", ThermalSeriousLocalSlots: 1, BatteryFloorPercent: 10, OnBatteryLocalSlots: -1, LowPowerLocalSlots: -1, ThermalFairLocalSlots: -1}
	sample := func(limit int) Reading {
		return newPowerThermal(cfg, testPowerProbe(powerInfo{onAC: true, batteryPct: 100, thermalLimitPct: limit}, nil)).sample(context.Background(), runSnapshot{})
	}
	serious, critical := sample(60), sample(40)
	if serious.modelTier >= tierFull {
		t.Fatalf("setup: the serious level should cap the tier: %+v", serious)
	}
	if critical.modelTier != serious.modelTier {
		t.Fatalf("critical is looser than serious: %v against %v", critical.modelTier, serious.modelTier)
	}
	if critical.MaxLocalSlots != 0 {
		t.Fatalf("critical must still stop local work: %+v", critical)
	}
}

func TestParsersRejectMalformedOutputInsteadOfGuessing(t *testing.T) {
	if got := parseSwapUsage("total = 1.2.3M  used = 5.0M"); got != -1 {
		t.Errorf("a malformed swap total gave %d, want unknown (-1)", got)
	}
	if got := parseSwapUsage("total = 1024.00M  used = 512.00M  free = 512.00M"); got != 50 {
		t.Errorf("a valid swap line gave %d", got)
	}
	iface, gw := parseDefaultRoute("   gateway:\n  interface: en0\n")
	if gw != "" || iface != "en0" {
		t.Errorf("an empty gateway field took the next line: iface %q gateway %q", iface, gw)
	}
}

func TestAnEvaluatorStartsNothingAfterClose(t *testing.T) {
	clk := newFakeClock()
	s := newFake("late")
	e := newEvaluator(options{clock: clk, workers: 4, exitDwell: time.Second, rampThrottle: time.Second}, registration{sensor: s, policy: failOpen, ttl: time.Second})
	e.Close()
	e.Close() // closing twice is harmless
	e.evaluate(context.Background(), runSnapshot{})
	clk.Advance(5 * time.Second)
	e.evaluate(context.Background(), runSnapshot{})
	if n := s.sampleCount(); n != 0 {
		t.Fatalf("a closed evaluator sampled its sensor %d times", n)
	}
}

// A refresh stores its reading and ends its flight in one step. With two steps, a
// watcher of the reading could see it while the entry still counted as in flight,
// so the next due refresh was skipped and the old reading kept standing.
func TestARefreshEndsItsFlightWhenItStoresTheReading(t *testing.T) {
	clk := newFakeClock()
	s := newFake("net")
	e := newEvaluator(options{clock: clk, workers: 4, exitDwell: time.Second, rampThrottle: time.Second}, registration{sensor: s, policy: failOpen, ttl: 10 * time.Second})
	defer e.Close()
	en := e.entries[0]
	e.mu.Lock()
	en.inflight, en.started = true, clk.Now()
	e.mu.Unlock()
	e.refresh(context.Background(), en, runSnapshot{})
	e.mu.Lock()
	have, inflight := en.have, en.inflight
	e.mu.Unlock()
	if !have || inflight {
		t.Fatalf("after a refresh: have=%v inflight=%v, want a stored reading and no flight", have, inflight)
	}
}
