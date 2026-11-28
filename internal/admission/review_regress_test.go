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
		r := newTestRateLimit(clk, &limitFake{evidence: LimitEvidence{Limited: true}})
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < 20; k++ {
					r.observeOutcomes([]outcome{{task: "r", attempt: k + 1, kind: outcomeFailure, locality: remote}}, clk.Now())
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
	r := newRateLimit("/run", rateLimitConfig{DefaultCooldownSeconds: 60, MaxCooldownSeconds: 600, Headroom: 1}, 4, clk,
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
		r.observeOutcomes([]outcome{{task: "r", attempt: i, kind: outcomeFailure, locality: remote}}, clk.Now())
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
	r := newTestRateLimit(clk, f)
	defer r.Close()
	r.observeOutcomes([]outcome{{task: "r1", attempt: 1, kind: outcomeFailure, locality: remote}}, clk.Now())
	waitUntil(t, r.idle)
	for i := 0; i < 5; i++ { // five remote attempts that were already running finish fine
		r.observeOutcomes([]outcome{{task: "other", kind: outcomeSuccess, locality: remote}}, clk.Now())
	}
	clk.Advance(61 * time.Second)
	if got := r.sample(context.Background(), runSnapshot{}); got.maxRemoteSlots != 3 {
		t.Fatalf("after the cooldown the limit should still be the halved 3, got %d", got.maxRemoteSlots)
	}
}

func TestAIMDDoesNotGrowWhileRemoteAccessIsOffOrHeld(t *testing.T) {
	for name, env := range map[string]Envelope{
		"remote off": func() Envelope { e := open(); e.AllowRemote = false; return e }(),
		"hold":       func() Envelope { e := open(); e.Hold = HoldTransient; return e }(),
		"zero slots": func() Envelope { e := open(); e.MaxRemoteSlots = 0; return e }(),
	} {
		a := newAIMD(8, 1, 1)
		a.onLimit()
		before := a.limit
		a.observe(env, remote)
		for i := 0; i < 20; i++ {
			a.onSuccess()
		}
		if a.limit != before {
			t.Errorf("%s: the limit grew from %d to %d while nothing could be admitted", name, before, a.limit)
		}
		a.observe(open(), remote)
		a.onSuccess()
		if a.limit != before+1 {
			t.Errorf("%s: growth should resume once admission does, got %d", name, a.limit)
		}
	}
}

// A dip into the band between half the threshold and the threshold must stop the
// shed clock, so crossing again does not replay the steps missed meanwhile.
func TestBreakerDoesNotReplayMissedShedStepsAfterADipIntoTheBand(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(breakerConfig{Alpha: 0.5, Threshold: 0.5, MinOutcomes: 3, IntervalSeconds: 10, OpenSeconds: 60, HalfLifeSeconds: 100000}, 4, clk)
	b.observeOutcomes(fail(6), clk.Now())
	b.sample(context.Background(), runSnapshot{}) // arms the shed clock at t0
	clk.Advance(time.Second)
	b.observeOutcomes(ok(2), clk.Now()) // rate falls into the band
	if got := b.sample(context.Background(), runSnapshot{}); got.MaxLocalSlots != Unlimited {
		t.Fatalf("nothing should have been shed yet: %+v", got)
	}
	clk.Advance(40 * time.Second) // far past several intervals
	b.observeOutcomes(fail(3), clk.Now())
	got := b.sample(context.Background(), runSnapshot{})
	if got.hold != holdNone || got.MaxLocalSlots < 3 {
		t.Fatalf("crossing again after a long dip must not shed all slots at once: %+v", got)
	}
}

func TestBreakerReopensWhenTheHalfOpenProbeFails(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(breakerConfig{Alpha: 0.5, Threshold: 0.5, MinOutcomes: 3, IntervalSeconds: 5, OpenSeconds: 30, HalfLifeSeconds: 100000}, 2, clk)
	b.observeOutcomes(fail(8), clk.Now())
	var opened bool
	for i := 0; i < 40 && !opened; i++ {
		b.observeOutcomes(fail(1), clk.Now())
		opened = b.sample(context.Background(), runSnapshot{}).hold == HoldTransient
		clk.Advance(time.Second)
	}
	if !opened {
		t.Fatal("the breaker never opened")
	}
	clk.Advance(31 * time.Second) // the open period is over: one probe slot
	if got := b.sample(context.Background(), runSnapshot{}); got.hold != holdNone || got.MaxLocalSlots != 1 {
		t.Fatalf("expected a half-open probe slot: %+v", got)
	}
	b.observeOutcomes(fail(1), clk.Now()) // the probe fails
	if got := b.sample(context.Background(), runSnapshot{}); got.hold != HoldTransient {
		t.Fatalf("a failed probe must open the breaker again: %+v", got)
	}
}

func TestAFailedProbeIsRetriedSoonNotAfterAWholeTTL(t *testing.T) {
	clk := newFakeClock()
	s := newFake("disk")
	bad := OpenReading("disk")
	bad.probe, bad.probeErr = probeFailed, "transient"
	s.set(bad)
	e := syncEval(clk, 4, registration{sensor: s, policy: failClosed, ttl: time.Minute})
	if env := e.evaluate(context.Background(), runSnapshot{}); env.Hold != HoldTransient {
		t.Fatalf("a failed fail-closed probe must hold: %+v", env)
	}
	s.set(OpenReading("disk")) // the next probe succeeds
	clk.Advance(3 * time.Second)
	e.evaluate(context.Background(), runSnapshot{})
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
	e := newEvaluator(options{clock: clk, workers: 4, exitDwell: time.Second, rampThrottle: time.Second},
		registration{sensor: s, policy: failClosed, ttl: 10 * time.Second})
	defer e.Close()
	e.evaluate(context.Background(), runSnapshot{})
	waitUntil(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.entries[0].have && !e.entries[0].inflight })
	s.mu.Lock()
	s.block = make(chan struct{})
	release := s.block
	s.mu.Unlock()
	defer close(release)
	clk.Advance(10 * time.Minute) // the pass gap
	if env := e.evaluate(context.Background(), runSnapshot{}); env.Hold != holdNone {
		t.Fatalf("a healthy sensor was held because of an idle gap: %+v", env)
	}
}

func TestEvaluatorCloseCancelsInFlightSamplesAndWaitsForThem(t *testing.T) {
	clk := newFakeClock()
	s := newFake("slow")
	s.block = make(chan struct{}) // never released: only cancellation can end it
	e := newEvaluator(options{clock: clk, workers: 2, exitDwell: time.Second, rampThrottle: time.Second},
		registration{sensor: s, policy: failOpen, ttl: time.Minute})
	e.evaluate(context.Background(), runSnapshot{})
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
	e := newEvaluator(options{clock: clk, workers: 2, exitDwell: time.Second, rampThrottle: time.Second},
		registration{sensor: s, policy: failClosed, ttl: time.Minute})
	defer e.Close()
	start := time.Now()
	env := e.evaluate(context.Background(), runSnapshot{})
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
	e := newEvaluator(options{clock: never{newFakeClock()}, workers: 2, exitDwell: time.Second, rampThrottle: time.Second},
		registration{sensor: s, policy: failOpen, ttl: time.Minute})
	defer e.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.evaluate(ctx, runSnapshot{}); close(done) }()
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
	e := newEvaluator(options{clock: newFakeClock(), workers: 2, sync: true}, registration{sensor: newFake("a"), policy: failOpen})
	if e.entries[0].reg.ttl <= 0 {
		t.Fatal("a background sensor registered with TTL 0 would never go stale")
	}
}

func TestTrackerIsSafeForConcurrentObservation(t *testing.T) {
	tr := newTracker()
	s := rows(map[string]model.TaskState{"a": {Status: "running", Attempt: 1}, "b": {Status: "running", Attempt: 1}})
	loc := func(string) locality { return local }
	tr.observe(s, time.Now(), loc)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tr.observe(s, time.Now(), loc)
			}
		}()
	}
	wg.Wait()
}

func TestOutcomeTrackingCostsNothingWithoutObservers(t *testing.T) {
	clk := newFakeClock()
	e := syncEval(clk, 4, registration{sensor: newFake("a"), policy: failOpen, ttl: time.Hour})
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
	hot.maxRemoteSlots, hot.Reason = 1, "thermal serious"
	thermal.set(hot)
	f := &limitFake{evidence: LimitEvidence{Limited: true}}
	rl := newRateLimit("/run", rateLimitConfig{DefaultCooldownSeconds: 30, MaxCooldownSeconds: 600, Headroom: 1}, 6, clk, f.diagnose)
	e := newEvaluator(options{clock: clk, workers: 6, exitDwell: 5 * time.Second, rampThrottle: time.Second, sync: true},
		registration{sensor: thermal, policy: failOpen, ttl: time.Second},
		registration{sensor: rl, policy: failOpen, inline: true})
	defer e.Close()

	// A long stretch under the thermal limit with remote attempts succeeding.
	for i := 0; i < 60; i++ {
		rl.observeOutcomes([]outcome{{task: "r", kind: outcomeSuccess, locality: remote}}, clk.Now())
		e.evaluate(context.Background(), runSnapshot{})
		clk.Advance(time.Second)
	}
	rl.mu.Lock()
	wound := rl.aimd.limit
	rl.mu.Unlock()
	if wound > 2 {
		t.Fatalf("the limiter wound up to %d behind a thermal limit of 1 plus headroom 1", wound)
	}

	// A provider limit starts a cooldown just as the thermal limit clears.
	rl.observeOutcomes([]outcome{{task: "r1", attempt: 1, kind: outcomeFailure, locality: remote}}, clk.Now())
	waitUntil(t, rl.idle)
	thermal.set(OpenReading("thermal"))
	clk.Advance(35 * time.Second) // both constraints are over
	var after []int
	for i := 0; i < 25; i++ {
		env := e.evaluate(context.Background(), runSnapshot{})
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
