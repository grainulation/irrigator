package admission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func syncEval(clk *fakeClock, workers int, regs ...registration) *Evaluator {
	return newEvaluator(options{clock: clk, workers: workers, exitDwell: 10 * time.Second, rampThrottle: time.Second, sync: true}, regs...)
}

func TestEvaluatorCachesAReadingForItsTTL(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	e := syncEval(clk, 4, registration{sensor: s, policy: failOpen, ttl: 15 * time.Second})
	for i := 0; i < 5; i++ {
		e.evaluate(context.Background(), runSnapshot{})
		clk.Advance(2 * time.Second)
	}
	if got := s.sampleCount(); got != 1 {
		t.Fatalf("sampled %d times inside the TTL", got)
	}
	clk.Advance(10 * time.Second)
	e.evaluate(context.Background(), runSnapshot{})
	if got := s.sampleCount(); got != 2 {
		t.Fatalf("did not resample after the TTL: %d", got)
	}
}

func TestEvaluatorFailOpenSensorsSayNothingWhenTheProbeFailsAndAreShownDegraded(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.probe, r.probeErr = probeFailed, "pmset: not found"
	s.set(r)
	env := syncEval(clk, 4, registration{sensor: s, policy: failOpen, ttl: time.Second}).evaluate(context.Background(), runSnapshot{})
	if env.restricted() || env.Hold != holdNone {
		t.Fatalf("a failed local probe restricted the run: %+v", env)
	}
	if len(env.Degraded) != 1 || env.Degraded[0] != "power" {
		t.Fatalf("degraded = %v", env.Degraded)
	}
}

func TestEvaluatorFailClosedSensorsHoldWhenTheProbeFails(t *testing.T) {
	clk := newFakeClock()
	s := newFake("disk")
	r := OpenReading("disk")
	r.probe, r.probeErr = probeFailed, "statfs failed"
	s.set(r)
	env := syncEval(clk, 4, registration{sensor: s, policy: failClosed, ttl: time.Second}).evaluate(context.Background(), runSnapshot{})
	if env.Hold != HoldTransient || env.MaxLocalSlots != 0 {
		t.Fatalf("a failed hard safety probe must hold admission: %+v", env)
	}
	if len(env.Bindings) == 0 || env.Bindings[0].Sensor != "disk" || env.Bindings[0].Severity != hard {
		t.Fatalf("the hold should name the failed sensor as a hard constraint: %+v", env.Bindings)
	}
}

func TestEvaluatorNeverBlocksOnASlowSensorAndAppliesItsPolicyUntilItAnswers(t *testing.T) {
	clk := newFakeClock()
	slow, hard := newFake("slow"), newFake("hard")
	slow.block, hard.block = make(chan struct{}), make(chan struct{})
	e := newEvaluator(options{clock: clk, workers: 4, exitDwell: time.Second, rampThrottle: time.Second},
		registration{sensor: slow, policy: failOpen, ttl: time.Minute},
		registration{sensor: hard, policy: failClosed, ttl: time.Minute})
	done := make(chan Envelope, 1)
	go func() { done <- e.evaluate(context.Background(), runSnapshot{}) }()
	select {
	case env := <-done:
		if env.Hold != HoldTransient {
			t.Fatalf("a fail-closed sensor with no reading yet must hold: %+v", env)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Evaluate waited for a sensor probe")
	}
	close(slow.block)
	close(hard.block)
}

func TestEvaluatorTreatsAReadingOlderThanThreeTTLsAsStale(t *testing.T) {
	clk := newFakeClock()
	s := newFake("net")
	offline := OpenReading("net")
	offline.allowRemote, offline.Reason = false, "offline"
	s.set(offline)
	e := newEvaluator(options{clock: clk, workers: 4, exitDwell: time.Second, rampThrottle: time.Second}, registration{sensor: s, policy: failOpen, ttl: 10 * time.Second})
	// The first call starts a background sample; wait until its reading lands.
	e.evaluate(context.Background(), runSnapshot{})
	waitUntil(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.entries[0].have })
	if env := e.evaluate(context.Background(), runSnapshot{}); env.AllowRemote {
		t.Fatal("a fresh offline reading was ignored")
	}
	// The probe now hangs, so no new reading arrives while the old one ages.
	s.mu.Lock()
	s.block = make(chan struct{})
	release := s.block
	s.mu.Unlock()
	defer close(release)
	clk.Advance(40 * time.Second)
	// A refresh has only just started: the last reading stands until it lands.
	if during := e.evaluate(context.Background(), runSnapshot{}); during.AllowRemote {
		t.Fatalf("a reading was dropped while its refresh was still within its timeout: %+v", during)
	}
	clk.Advance(2 * sampleTimeout) // the probe is hung well past its timeout
	got := e.evaluate(context.Background(), runSnapshot{})
	if len(got.Degraded) != 1 || got.Degraded[0] != "net" {
		t.Fatalf("a stale reading should be shown degraded: %+v", got)
	}
	// The stale sensor now says nothing, but remote access reopens only after the
	// exit dwell, like any other relaxation.
	clk.Advance(2 * time.Second)
	if got = e.evaluate(context.Background(), runSnapshot{}); !got.AllowRemote {
		t.Fatalf("a stale fail-open reading should stop restricting once the dwell has passed: %+v", got)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEvaluatorRecoversFromAPanickingSensor(t *testing.T) {
	clk := newFakeClock()
	e := syncEval(clk, 4, registration{sensor: panicSensor{}, policy: failOpen, ttl: time.Second})
	env := e.evaluate(context.Background(), runSnapshot{})
	if env.restricted() || len(env.Degraded) != 1 {
		t.Fatalf("a panicking sensor must degrade, not crash or restrict: %+v", env)
	}
}

type panicSensor struct{}

func (panicSensor) sensorName() string                          { return "boom" }
func (panicSensor) sample(context.Context, runSnapshot) Reading { panic("probe exploded") }

func TestEvaluatorTellsStatefulSensorsTheEffectiveEnvelope(t *testing.T) {
	clk := newFakeClock()
	thermal, limiter := newFake("thermal"), newFake("limiter")
	r := OpenReading("thermal")
	r.MaxLocalSlots, r.Reason = 1, "serious"
	thermal.set(r)
	e := syncEval(clk, 6, registration{sensor: thermal, policy: failOpen, ttl: time.Minute}, registration{sensor: limiter, policy: failOpen, ttl: time.Minute})
	e.evaluate(context.Background(), runSnapshot{})
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.observed) != 1 || limiter.observed[0].MaxLocalSlots != 1 {
		t.Fatalf("the limiter was not told the envelope that applies: %+v", limiter.observed)
	}
}

// Anti-windup: a limiter that is not the binding constraint must not wind up and
// then overshoot when the binding constraint is released.
func TestAIMDDoesNotWindUpUnderAnotherSensorsConstraint(t *testing.T) {
	a := newAIMD(6, 1, 1)
	binding := open()
	binding.MaxLocalSlots = 2
	for i := 0; i < 50; i++ {
		a.onSuccess()
		a.observe(binding, local)
	}
	if a.limit > 3 {
		t.Fatalf("the limiter wound up to %d under a limit of 2 plus headroom 1", a.limit)
	}
	// The binding constraint is released: it may not jump to the maximum.
	a.onSuccess()
	a.observe(open(), local)
	if a.limit > 4 {
		t.Fatalf("limit %d jumped after the binding constraint was released", a.limit)
	}
}

func TestAIMDDoesNotChangeWhileTheEnvelopeAdmitsNothing(t *testing.T) {
	zero := open()
	zero.MaxLocalSlots = 0
	held := open()
	held.Hold = HoldTransient
	for name, env := range map[string]Envelope{"zero slots": zero, "hold": held} {
		a := newAIMD(6, 1, 1)
		a.limit = 4
		for i := 0; i < 20; i++ {
			a.observe(env, local)
		}
		if a.limit != 4 {
			t.Fatalf("%s: the limit moved to %d while nothing could be admitted", name, a.limit)
		}
	}
	a := newAIMD(6, 1, 1)
	if a.onLimit(); a.limit != 3 {
		t.Fatalf("multiplicative decrease: %d", a.limit)
	}
	if a.onSuccess(); a.limit != 4 {
		t.Fatalf("additive increase: %d", a.limit)
	}
}

func TestStatusIsWrittenOnTransitionsAndDebounced(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	path := filepath.Join(t.TempDir(), StatusFile)
	e := newEvaluator(options{clock: clk, workers: 4, exitDwell: time.Second, rampThrottle: time.Second, sync: true, statusPath: path, statusDebounce: 10 * time.Second, preset: "balanced"},
		registration{sensor: s, policy: failOpen, ttl: time.Second})
	read := func() Status {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var st Status
		if err = json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	e.evaluate(context.Background(), runSnapshot{})
	first := read()
	if first.Preset != "balanced" || first.ControllerPID != os.Getpid() || first.Summary != "open" {
		t.Fatalf("first status wrong: %+v", first)
	}
	// An unchanged picture writes nothing, however often it is evaluated.
	os.Remove(path)
	for i := 0; i < 5; i++ {
		clk.Advance(time.Second)
		e.evaluate(context.Background(), runSnapshot{})
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("an unchanged envelope rewrote the status file")
	}
	// A restriction inside the debounce window is held back...
	r := OpenReading("power")
	r.MaxLocalSlots, r.Reason = 2, "thermal"
	s.set(r)
	clk.Advance(time.Second)
	e.evaluate(context.Background(), runSnapshot{})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a change inside the debounce window was written at once")
	}
	// ...and written when the window passes.
	clk.Advance(10 * time.Second)
	e.evaluate(context.Background(), runSnapshot{})
	if st := read(); st.Envelope.MaxLocalSlots != 2 {
		t.Fatalf("status after the debounce: %+v", st)
	}
	// A hold is never delayed.
	h := OpenReading("power")
	h.hold, h.Reason = HoldTransient, "battery low"
	s.set(h)
	clk.Advance(time.Second)
	e.evaluate(context.Background(), runSnapshot{})
	if st := read(); st.Envelope.Hold != HoldTransient {
		t.Fatalf("a hold must be written at once: %+v", st)
	}
}
