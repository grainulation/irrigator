package admission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func syncEval(clk *fakeClock, workers int, regs ...Registration) *Evaluator {
	return NewEvaluator(Options{Clock: clk, Workers: workers, ExitDwell: 10 * time.Second, RampThrottle: time.Second, Sync: true}, regs...)
}

func TestEvaluatorCachesAReadingForItsTTL(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	e := syncEval(clk, 4, Registration{Sensor: s, Policy: FailOpen, TTL: 15 * time.Second})
	for i := 0; i < 5; i++ {
		e.Evaluate(context.Background(), RunSnapshot{})
		clk.Advance(2 * time.Second)
	}
	if got := s.sampleCount(); got != 1 {
		t.Fatalf("sampled %d times inside the TTL", got)
	}
	clk.Advance(10 * time.Second)
	e.Evaluate(context.Background(), RunSnapshot{})
	if got := s.sampleCount(); got != 2 {
		t.Fatalf("did not resample after the TTL: %d", got)
	}
}

func TestEvaluatorFailOpenSensorsSayNothingWhenTheProbeFailsAndAreShownDegraded(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.Probe, r.ProbeErr = ProbeFailed, "pmset: not found"
	s.set(r)
	env := syncEval(clk, 4, Registration{Sensor: s, Policy: FailOpen, TTL: time.Second}).Evaluate(context.Background(), RunSnapshot{})
	if env.Restricted() || env.Hold != HoldNone {
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
	r.Probe, r.ProbeErr = ProbeFailed, "statfs failed"
	s.set(r)
	env := syncEval(clk, 4, Registration{Sensor: s, Policy: FailClosed, TTL: time.Second}).Evaluate(context.Background(), RunSnapshot{})
	if env.Hold != HoldTransient || env.MaxLocalSlots != 0 {
		t.Fatalf("a failed hard safety probe must hold admission: %+v", env)
	}
	if len(env.Bindings) == 0 || env.Bindings[0].Sensor != "disk" || env.Bindings[0].Severity != Hard {
		t.Fatalf("the hold should name the failed sensor as a hard constraint: %+v", env.Bindings)
	}
}

func TestEvaluatorNeverBlocksOnASlowSensorAndAppliesItsPolicyUntilItAnswers(t *testing.T) {
	clk := newFakeClock()
	slow, hard := newFake("slow"), newFake("hard")
	slow.block, hard.block = make(chan struct{}), make(chan struct{})
	e := NewEvaluator(Options{Clock: clk, Workers: 4, ExitDwell: time.Second, RampThrottle: time.Second},
		Registration{Sensor: slow, Policy: FailOpen, TTL: time.Minute},
		Registration{Sensor: hard, Policy: FailClosed, TTL: time.Minute})
	done := make(chan Envelope, 1)
	go func() { done <- e.Evaluate(context.Background(), RunSnapshot{}) }()
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
	offline.AllowRemote, offline.Reason = false, "offline"
	s.set(offline)
	e := NewEvaluator(Options{Clock: clk, Workers: 4, ExitDwell: time.Second, RampThrottle: time.Second}, Registration{Sensor: s, Policy: FailOpen, TTL: 10 * time.Second})
	// The first call starts a background sample; wait until its reading lands.
	e.Evaluate(context.Background(), RunSnapshot{})
	waitUntil(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.entries[0].have })
	if env := e.Evaluate(context.Background(), RunSnapshot{}); env.AllowRemote {
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
	if during := e.Evaluate(context.Background(), RunSnapshot{}); during.AllowRemote {
		t.Fatalf("a reading was dropped while its refresh was still within its timeout: %+v", during)
	}
	clk.Advance(2 * sampleTimeout) // the probe is hung well past its timeout
	got := e.Evaluate(context.Background(), RunSnapshot{})
	if len(got.Degraded) != 1 || got.Degraded[0] != "net" {
		t.Fatalf("a stale reading should be shown degraded: %+v", got)
	}
	// The stale sensor now says nothing, but remote access reopens only after the
	// exit dwell, like any other relaxation.
	clk.Advance(2 * time.Second)
	if got = e.Evaluate(context.Background(), RunSnapshot{}); !got.AllowRemote {
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
	e := syncEval(clk, 4, Registration{Sensor: panicSensor{}, Policy: FailOpen, TTL: time.Second})
	env := e.Evaluate(context.Background(), RunSnapshot{})
	if env.Restricted() || len(env.Degraded) != 1 {
		t.Fatalf("a panicking sensor must degrade, not crash or restrict: %+v", env)
	}
}

type panicSensor struct{}

func (panicSensor) Name() string                                { return "boom" }
func (panicSensor) Sample(context.Context, RunSnapshot) Reading { panic("probe exploded") }

func TestEvaluatorTellsStatefulSensorsTheEffectiveEnvelope(t *testing.T) {
	clk := newFakeClock()
	thermal, limiter := newFake("thermal"), newFake("limiter")
	r := OpenReading("thermal")
	r.MaxLocalSlots, r.Reason = 1, "serious"
	thermal.set(r)
	e := syncEval(clk, 6, Registration{Sensor: thermal, Policy: FailOpen, TTL: time.Minute}, Registration{Sensor: limiter, Policy: FailOpen, TTL: time.Minute})
	e.Evaluate(context.Background(), RunSnapshot{})
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if len(limiter.observed) != 1 || limiter.observed[0].MaxLocalSlots != 1 {
		t.Fatalf("the limiter was not told the envelope that applies: %+v", limiter.observed)
	}
}

// Anti-windup: a limiter that is not the binding constraint must not wind up and
// then overshoot when the binding constraint is released.
func TestAIMDDoesNotWindUpUnderAnotherSensorsConstraint(t *testing.T) {
	a := NewAIMD(6, 1, 1)
	binding := Open()
	binding.MaxLocalSlots = 2
	for i := 0; i < 50; i++ {
		a.OnSuccess()
		a.Observe(binding, Local)
	}
	if a.Limit > 3 {
		t.Fatalf("the limiter wound up to %d under a limit of 2 plus headroom 1", a.Limit)
	}
	// The binding constraint is released: it may not jump to the maximum.
	a.OnSuccess()
	a.Observe(Open(), Local)
	if a.Limit > 4 {
		t.Fatalf("limit %d jumped after the binding constraint was released", a.Limit)
	}
}

func TestAIMDDoesNotChangeWhileTheEnvelopeAdmitsNothing(t *testing.T) {
	zero := Open()
	zero.MaxLocalSlots = 0
	held := Open()
	held.Hold = HoldTransient
	for name, env := range map[string]Envelope{"zero slots": zero, "hold": held} {
		a := NewAIMD(6, 1, 1)
		a.Limit = 4
		for i := 0; i < 20; i++ {
			a.Observe(env, Local)
		}
		if a.Limit != 4 {
			t.Fatalf("%s: the limit moved to %d while nothing could be admitted", name, a.Limit)
		}
	}
	a := NewAIMD(6, 1, 1)
	if a.OnLimit(); a.Limit != 3 {
		t.Fatalf("multiplicative decrease: %d", a.Limit)
	}
	if a.OnSuccess(); a.Limit != 4 {
		t.Fatalf("additive increase: %d", a.Limit)
	}
}

func TestStatusIsWrittenOnTransitionsAndDebounced(t *testing.T) {
	clk := newFakeClock()
	s := newFake("power")
	path := filepath.Join(t.TempDir(), StatusFile)
	e := NewEvaluator(Options{Clock: clk, Workers: 4, ExitDwell: time.Second, RampThrottle: time.Second, Sync: true, StatusPath: path, StatusDebounce: 10 * time.Second, Preset: "balanced"},
		Registration{Sensor: s, Policy: FailOpen, TTL: time.Second})
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
	e.Evaluate(context.Background(), RunSnapshot{})
	first := read()
	if first.Preset != "balanced" || first.ControllerPID != os.Getpid() || first.Summary != "open" {
		t.Fatalf("first status wrong: %+v", first)
	}
	// An unchanged picture writes nothing, however often it is evaluated.
	os.Remove(path)
	for i := 0; i < 5; i++ {
		clk.Advance(time.Second)
		e.Evaluate(context.Background(), RunSnapshot{})
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("an unchanged envelope rewrote the status file")
	}
	// A restriction inside the debounce window is held back...
	r := OpenReading("power")
	r.MaxLocalSlots, r.Reason = 2, "thermal"
	s.set(r)
	clk.Advance(time.Second)
	e.Evaluate(context.Background(), RunSnapshot{})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a change inside the debounce window was written at once")
	}
	// ...and written when the window passes.
	clk.Advance(10 * time.Second)
	e.Evaluate(context.Background(), RunSnapshot{})
	if st := read(); st.Envelope.MaxLocalSlots != 2 {
		t.Fatalf("status after the debounce: %+v", st)
	}
	// A hold is never delayed.
	h := OpenReading("power")
	h.Hold, h.Reason = HoldTransient, "battery low"
	s.set(h)
	clk.Advance(time.Second)
	e.Evaluate(context.Background(), RunSnapshot{})
	if st := read(); st.Envelope.Hold != HoldTransient {
		t.Fatalf("a hold must be written at once: %+v", st)
	}
}
