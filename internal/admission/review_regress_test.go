package admission

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

// Regressions for defects found in review. Each test names the failure it pins.

func TestRateLimitObserveAndCloseDoNotRaceOrPanic(t *testing.T) {
	for i := 0; i < 50; i++ {
		clk := newFakeClock()
		r := newRateLimit(clk, &limitFake{evidence: LimitEvidence{Limited: true}})
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < 20; k++ {
					r.ObserveOutcomes([]Outcome{{Task: "r", Attempt: k + 1, Kind: OutcomeFailure, Locality: Remote}}, clk.Now())
				}
			}()
		}
		r.Close() // concurrently with the senders: must neither panic nor race
		wg.Wait()
	}
}

func TestRateLimitCloseWaitsForTheDrainAndStartsNoDiagnosisAfterwards(t *testing.T) {
	clk := newFakeClock()
	started, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	calls := 0
	r := NewRateLimit("/run", RateLimitConfig{DefaultCooldownSeconds: 60, MaxCooldownSeconds: 600, Headroom: 1}, 4, clk,
		func(string, string, int, time.Time) (LimitEvidence, error) {
			mu.Lock()
			calls++
			first := calls == 1
			mu.Unlock()
			if first {
				close(started)
				<-release
			}
			return LimitEvidence{}, nil
		})
	for i := 1; i <= 5; i++ {
		r.ObserveOutcomes([]Outcome{{Task: "r", Attempt: i, Kind: OutcomeFailure, Locality: Remote}}, clk.Now())
	}
	<-started
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a diagnosis was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the running diagnosis finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("queued attempts were still diagnosed after Close: %d calls", calls)
	}
}

// Successes that finish during a cooldown must not grow the limit back, or the
// multiplicative decrease is undone before the cooldown ends.
func TestRateLimitInFlightSuccessesDuringCooldownDoNotWindTheLimitBackUp(t *testing.T) {
	clk := newFakeClock()
	f := &limitFake{evidence: LimitEvidence{Limited: true}}
	r := newRateLimit(clk, f)
	defer r.Close()
	r.ObserveOutcomes([]Outcome{{Task: "r1", Attempt: 1, Kind: OutcomeFailure, Locality: Remote}}, clk.Now())
	waitUntil(t, r.idle)
	for i := 0; i < 5; i++ { // five remote attempts that were already running finish fine
		r.ObserveOutcomes([]Outcome{{Task: "other", Kind: OutcomeSuccess, Locality: Remote}}, clk.Now())
	}
	clk.Advance(61 * time.Second)
	if got := r.Sample(context.Background(), RunSnapshot{}); got.MaxRemoteSlots != 3 {
		t.Fatalf("after the cooldown the limit should still be the halved 3, got %d", got.MaxRemoteSlots)
	}
}

func TestAIMDDoesNotGrowWhileRemoteAccessIsOffOrHeld(t *testing.T) {
	for name, env := range map[string]Envelope{
		"remote off": func() Envelope { e := Open(); e.AllowRemote = false; return e }(),
		"hold":       func() Envelope { e := Open(); e.Hold = HoldTransient; return e }(),
		"zero slots": func() Envelope { e := Open(); e.MaxRemoteSlots = 0; return e }(),
	} {
		a := NewAIMD(8, 1, 1)
		a.OnLimit()
		before := a.Limit
		a.Observe(env, Remote)
		for i := 0; i < 20; i++ {
			a.OnSuccess()
		}
		if a.Limit != before {
			t.Errorf("%s: the limit grew from %d to %d while nothing could be admitted", name, before, a.Limit)
		}
		a.Observe(Open(), Remote)
		a.OnSuccess()
		if a.Limit != before+1 {
			t.Errorf("%s: growth should resume once admission does, got %d", name, a.Limit)
		}
	}
}

// A dip into the band between half the threshold and the threshold must stop the
// shed clock, so crossing again does not replay the steps missed meanwhile.
func TestBreakerDoesNotReplayMissedShedStepsAfterADipIntoTheBand(t *testing.T) {
	clk := newFakeClock()
	b := NewBreaker(BreakerConfig{Alpha: 0.5, Threshold: 0.5, MinOutcomes: 3, IntervalSeconds: 10, OpenSeconds: 60, HalfLifeSeconds: 100000}, 4, clk)
	b.ObserveOutcomes(fail(6), clk.Now())
	b.Sample(context.Background(), RunSnapshot{}) // arms the shed clock at t0
	clk.Advance(time.Second)
	b.ObserveOutcomes(ok(2), clk.Now()) // rate falls into the band
	if got := b.Sample(context.Background(), RunSnapshot{}); got.MaxLocalSlots != Unlimited {
		t.Fatalf("nothing should have been shed yet: %+v", got)
	}
	clk.Advance(40 * time.Second) // far past several intervals
	b.ObserveOutcomes(fail(3), clk.Now())
	got := b.Sample(context.Background(), RunSnapshot{})
	if got.Hold != HoldNone || got.MaxLocalSlots < 3 {
		t.Fatalf("crossing again after a long dip must not shed all slots at once: %+v", got)
	}
}

func TestBreakerReopensWhenTheHalfOpenProbeFails(t *testing.T) {
	clk := newFakeClock()
	b := NewBreaker(BreakerConfig{Alpha: 0.5, Threshold: 0.5, MinOutcomes: 3, IntervalSeconds: 5, OpenSeconds: 30, HalfLifeSeconds: 100000}, 2, clk)
	b.ObserveOutcomes(fail(8), clk.Now())
	var opened bool
	for i := 0; i < 40 && !opened; i++ {
		b.ObserveOutcomes(fail(1), clk.Now())
		opened = b.Sample(context.Background(), RunSnapshot{}).Hold == HoldTransient
		clk.Advance(time.Second)
	}
	if !opened {
		t.Fatal("the breaker never opened")
	}
	clk.Advance(31 * time.Second) // the open period is over: one probe slot
	if got := b.Sample(context.Background(), RunSnapshot{}); got.Hold != HoldNone || got.MaxLocalSlots != 1 {
		t.Fatalf("expected a half-open probe slot: %+v", got)
	}
	b.ObserveOutcomes(fail(1), clk.Now()) // the probe fails
	if got := b.Sample(context.Background(), RunSnapshot{}); got.Hold != HoldTransient {
		t.Fatalf("a failed probe must open the breaker again: %+v", got)
	}
}

func TestAFailedProbeIsRetriedSoonNotAfterAWholeTTL(t *testing.T) {
	clk := newFakeClock()
	s := newFake("disk")
	bad := OpenReading("disk")
	bad.Probe, bad.ProbeErr = ProbeFailed, "transient"
	s.set(bad)
	e := syncEval(clk, 4, Registration{Sensor: s, Policy: FailClosed, TTL: time.Minute})
	if env := e.Evaluate(context.Background(), RunSnapshot{}); env.Hold != HoldTransient {
		t.Fatalf("a failed fail-closed probe must hold: %+v", env)
	}
	s.set(OpenReading("disk")) // the next probe succeeds
	clk.Advance(3 * time.Second)
	e.Evaluate(context.Background(), RunSnapshot{})
	if got := s.sampleCount(); got != 2 {
		t.Fatalf("the failed probe was not retried within seconds (samples: %d)", got)
	}
}

// A machine that slept, or a pass that came late, makes readings old without
// making them wrong. A healthy fail-closed sensor must not hold admission just
// because its refresh is still in flight.
func TestAnIdleGapDoesNotHoldAHealthyFailClosedSensor(t *testing.T) {
	clk := newFakeClock()
	s := newFake("disk")
	e := NewEvaluator(Options{Clock: clk, Workers: 4, ExitDwell: time.Second, RampThrottle: time.Second},
		Registration{Sensor: s, Policy: FailClosed, TTL: 10 * time.Second})
	defer e.Close()
	e.Evaluate(context.Background(), RunSnapshot{})
	waitUntil(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.entries[0].have && !e.entries[0].inflight })
	s.mu.Lock()
	s.block = make(chan struct{})
	release := s.block
	s.mu.Unlock()
	defer close(release)
	clk.Advance(10 * time.Minute) // the pass gap
	if env := e.Evaluate(context.Background(), RunSnapshot{}); env.Hold != HoldNone {
		t.Fatalf("a healthy sensor was held because of an idle gap: %+v", env)
	}
}

func TestEvaluatorCloseCancelsInFlightSamplesAndWaitsForThem(t *testing.T) {
	clk := newFakeClock()
	s := newFake("slow")
	s.block = make(chan struct{}) // never released: only cancellation can end it
	e := NewEvaluator(Options{Clock: clk, Workers: 2, ExitDwell: time.Second, RampThrottle: time.Second},
		Registration{Sensor: s, Policy: FailOpen, TTL: time.Minute})
	e.Evaluate(context.Background(), RunSnapshot{})
	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel and wait for the in-flight sample")
	}
}

// The warm-up wait comes from the injected clock, not the wall clock.
func TestWarmUpUsesTheInjectedClockAndReturnsAtOnceWhenItFires(t *testing.T) {
	clk := newFakeClock() // its After fires immediately
	s := newFake("slow")
	s.block = make(chan struct{})
	defer close(s.block)
	e := NewEvaluator(Options{Clock: clk, Workers: 2, ExitDwell: time.Second, RampThrottle: time.Second},
		Registration{Sensor: s, Policy: FailClosed, TTL: time.Minute})
	defer e.Close()
	start := time.Now()
	env := e.Evaluate(context.Background(), RunSnapshot{})
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("warm-up waited on the wall clock for %v", time.Since(start))
	}
	if env.Hold != HoldTransient {
		t.Fatalf("a fail-closed sensor with no reading yet must hold: %+v", env)
	}
}

func TestWarmUpStopsWhenTheContextIsCancelled(t *testing.T) {
	s := newFake("slow")
	s.block = make(chan struct{})
	defer close(s.block)
	// A clock whose After never fires: only the context can end the wait.
	e := NewEvaluator(Options{Clock: never{newFakeClock()}, Workers: 2, ExitDwell: time.Second, RampThrottle: time.Second},
		Registration{Sensor: s, Policy: FailOpen, TTL: time.Minute})
	defer e.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Evaluate(ctx, RunSnapshot{}); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Evaluate ignored its cancelled context during warm-up")
	}
}

type never struct{ *fakeClock }

func (never) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

func TestABackgroundSensorWithNoTTLStillGetsAStalenessBound(t *testing.T) {
	e := NewEvaluator(Options{Clock: newFakeClock(), Workers: 2, Sync: true}, Registration{Sensor: newFake("a"), Policy: FailOpen})
	if e.entries[0].reg.TTL <= 0 {
		t.Fatal("a background sensor registered with TTL 0 would never go stale")
	}
}

func TestTrackerIsSafeForConcurrentObservation(t *testing.T) {
	tr := NewTracker()
	s := rows(map[string]model.TaskState{"a": {Status: "running", Attempt: 1}, "b": {Status: "running", Attempt: 1}})
	loc := func(string) Locality { return Local }
	tr.Observe(s, time.Now(), loc)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tr.Observe(s, time.Now(), loc)
			}
		}()
	}
	wg.Wait()
}

func TestOutcomeTrackingCostsNothingWithoutObservers(t *testing.T) {
	clk := newFakeClock()
	e := syncEval(clk, 4, Registration{Sensor: newFake("a"), Policy: FailOpen, TTL: time.Hour})
	s := rows(map[string]model.TaskState{"a": {Status: "running", Attempt: 1}})
	s.Tasks["a"].Status = "failed"
	e.Admit(s, nil, map[string]bool{}, 1)
	if len(e.tracker.seen) != 0 {
		t.Fatal("the tracker scanned the run although no sensor learns from outcomes")
	}
}

// Anti-windup handoff, end to end through the evaluator: while a tight thermal
// limit binds remote work, the rate-limit sensor's successes must not wind its
// own limit up, so that when the thermal limit and a cooldown release together
// admission recovers from what was last known to be safe, not from the maximum.
func TestAntiWindupHandoffWhenAThermalLimitAndACooldownReleaseTogether(t *testing.T) {
	clk := newFakeClock()
	thermal := newFake("thermal")
	hot := OpenReading("thermal")
	hot.MaxRemoteSlots, hot.Reason = 1, "thermal serious"
	thermal.set(hot)
	f := &limitFake{evidence: LimitEvidence{Limited: true}}
	rl := NewRateLimit("/run", RateLimitConfig{DefaultCooldownSeconds: 30, MaxCooldownSeconds: 600, Headroom: 1}, 6, clk, f.diagnose)
	e := NewEvaluator(Options{Clock: clk, Workers: 6, ExitDwell: 5 * time.Second, RampThrottle: time.Second, Sync: true},
		Registration{Sensor: thermal, Policy: FailOpen, TTL: time.Second},
		Registration{Sensor: rl, Policy: FailOpen, Inline: true})
	defer e.Close()

	// A long stretch under the thermal limit with remote attempts succeeding.
	for i := 0; i < 60; i++ {
		rl.ObserveOutcomes([]Outcome{{Task: "r", Kind: OutcomeSuccess, Locality: Remote}}, clk.Now())
		e.Evaluate(context.Background(), RunSnapshot{})
		clk.Advance(time.Second)
	}
	rl.mu.Lock()
	wound := rl.aimd.Limit
	rl.mu.Unlock()
	if wound > 2 {
		t.Fatalf("the limiter wound up to %d behind a thermal limit of 1 plus headroom 1", wound)
	}

	// A provider limit starts a cooldown just as the thermal limit clears.
	rl.ObserveOutcomes([]Outcome{{Task: "r1", Attempt: 1, Kind: OutcomeFailure, Locality: Remote}}, clk.Now())
	waitUntil(t, rl.idle)
	thermal.set(OpenReading("thermal"))
	clk.Advance(35 * time.Second) // both constraints are over
	var after []int
	for i := 0; i < 25; i++ {
		env := e.Evaluate(context.Background(), RunSnapshot{})
		after = append(after, env.MaxRemoteSlots)
		clk.Advance(time.Second)
	}
	for _, slots := range after {
		if slots > 3 && slots != Unlimited {
			t.Fatalf("remote admission jumped to %d after the release; it must recover from the limiter's own safe limit: %v", slots, after)
		}
	}
	if after[0] == Unlimited {
		t.Fatalf("remote admission was fully open the moment both constraints released: %v", after)
	}
}
