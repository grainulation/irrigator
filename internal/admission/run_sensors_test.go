package admission

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func rows(spec map[string]model.TaskState) *model.State {
	s := &model.State{Tasks: map[string]*model.TaskState{}}
	for id, r := range spec {
		r := r
		s.Tasks[id] = &r
	}
	return s
}

func TestTrackerReportsEachFinishedAttemptOnceAndSkipsHistory(t *testing.T) {
	tr := NewTracker()
	loc := func(id string) Locality {
		if id[0] == 'r' {
			return Remote
		}
		return Local
	}
	s := rows(map[string]model.TaskState{"l1": {Status: "verified", Attempt: 1}, "r1": {Status: "running", Attempt: 1}})
	if outs := tr.Observe(s, time.Now(), loc); len(outs) != 0 {
		t.Fatalf("attempts finished before the controller started are history, got %v", outs)
	}
	s.Tasks["r1"].Status = "failed"
	s.Tasks["l1"].Status, s.Tasks["l1"].Attempt = "running", 2 // retried
	outs := tr.Observe(s, time.Now(), loc)
	if len(outs) != 1 || outs[0].Task != "r1" || outs[0].Kind != OutcomeFailure || outs[0].Locality != Remote || outs[0].Attempt != 1 {
		t.Fatalf("outcomes = %+v", outs)
	}
	if outs = tr.Observe(s, time.Now(), loc); len(outs) != 0 {
		t.Fatalf("an unchanged row was reported again: %+v", outs)
	}
	s.Tasks["l1"].Status = "verified"
	s.Tasks["r1"].Status, s.Tasks["r1"].Attempt = "uncertain", 2
	outs = tr.Observe(s, time.Now(), loc)
	if len(outs) != 2 {
		t.Fatalf("a retried attempt that finishes is a new outcome: %+v", outs)
	}
}

func TestRunCapMakesTheSizingCapVisibleAsABinding(t *testing.T) {
	clk := newFakeClock()
	rc := NewRunCap()
	e := NewEvaluator(Options{Clock: clk, Workers: 6, ExitDwell: time.Second, RampThrottle: time.Second, Sync: true, Locality: byName},
		Registration{Sensor: rc, Policy: FailOpen, Inline: true})
	if env := e.Evaluate(context.Background(), RunSnapshot{}); env.Restricted() {
		t.Fatalf("no cap yet: %+v", env)
	}
	e.NoteSizing(2, "plan sizing caps concurrency")
	env := e.Evaluate(context.Background(), RunSnapshot{})
	if env.MaxLocalSlots != 2 || env.MaxRemoteSlots != 2 {
		t.Fatalf("sizing cap not reflected: %+v", env)
	}
	found := false
	for _, b := range env.Bindings {
		found = found || (b.Sensor == "sizing" && b.Severity == Hard)
	}
	if !found {
		t.Fatalf("the sizing sensor should be named as the binding constraint: %+v", env.Bindings)
	}
}

type limitFake struct {
	mu       sync.Mutex
	calls    []string
	evidence LimitEvidence
	err      error
}

func (f *limitFake) diagnose(_, task string, attempt int, _ time.Time) (LimitEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, task)
	return f.evidence, f.err
}

func newRateLimit(clk *fakeClock, f *limitFake) *RateLimit {
	return NewRateLimit("/run", RateLimitConfig{DefaultCooldownSeconds: 60, MaxCooldownSeconds: 600, Headroom: 1}, 6, clk, f.diagnose)
}

func TestRateLimitCoolsDownRemoteAdmissionAndRecoversByAIMD(t *testing.T) {
	clk := newFakeClock()
	f := &limitFake{evidence: LimitEvidence{Limited: true}}
	r := newRateLimit(clk, f)
	defer r.Close()
	if got := r.Sample(context.Background(), RunSnapshot{}); got.MaxRemoteSlots != Unlimited {
		t.Fatalf("no signal yet: %+v", got)
	}
	r.ObserveOutcomes([]Outcome{{Task: "r1", Attempt: 1, Kind: OutcomeFailure, Locality: Remote}}, clk.Now())
	waitUntil(t, r.idle)
	got := r.Sample(context.Background(), RunSnapshot{})
	if got.MaxRemoteSlots != 0 || got.MaxLocalSlots != Unlimited {
		t.Fatalf("a rate limit must stop new remote admission only: %+v", got)
	}
	clk.Advance(61 * time.Second)
	got = r.Sample(context.Background(), RunSnapshot{})
	if got.MaxRemoteSlots != 3 {
		t.Fatalf("after the cooldown the limit should have halved from 6 to 3, got %d", got.MaxRemoteSlots)
	}
	for i := 0; i < 3; i++ {
		r.ObserveOutcomes([]Outcome{{Task: "r2", Kind: OutcomeSuccess, Locality: Remote}}, clk.Now())
	}
	if got = r.Sample(context.Background(), RunSnapshot{}); got.MaxRemoteSlots != Unlimited {
		t.Fatalf("successes should grow the limit back to unlimited, got %d", got.MaxRemoteSlots)
	}
}

func TestRateLimitUsesTheProvidersStatedResetAndCapsIt(t *testing.T) {
	clk := newFakeClock()
	reset := clk.Now().Add(5 * time.Minute)
	f := &limitFake{evidence: LimitEvidence{Limited: true, ResetAt: &reset}}
	r := newRateLimit(clk, f)
	defer r.Close()
	r.ObserveOutcomes([]Outcome{{Task: "r1", Attempt: 1, Kind: OutcomeFailure, Locality: Remote}}, clk.Now())
	waitUntil(t, r.idle)
	clk.Advance(4 * time.Minute)
	if got := r.Sample(context.Background(), RunSnapshot{}); got.MaxRemoteSlots != 0 {
		t.Fatalf("still before the stated reset: %+v", got)
	}
	clk.Advance(2 * time.Minute)
	if got := r.Sample(context.Background(), RunSnapshot{}); got.MaxRemoteSlots == 0 {
		t.Fatal("cooldown should end after the stated reset")
	}
	// A reset far in the future is capped by max_cooldown_seconds.
	far := clk.Now().Add(24 * time.Hour)
	f.mu.Lock()
	f.evidence = LimitEvidence{Limited: true, ResetAt: &far}
	f.mu.Unlock()
	r.ObserveOutcomes([]Outcome{{Task: "r2", Attempt: 1, Kind: OutcomeFailure, Locality: Remote}}, clk.Now())
	waitUntil(t, r.idle)
	clk.Advance(11 * time.Minute)
	if got := r.Sample(context.Background(), RunSnapshot{}); got.MaxRemoteSlots == 0 {
		t.Fatal("a cooldown must never exceed max_cooldown_seconds (10 minutes here)")
	}
}

func TestRateLimitIgnoresLocalFailuresAndFailuresWithoutLimitEvidence(t *testing.T) {
	clk := newFakeClock()
	f := &limitFake{evidence: LimitEvidence{Limited: false}}
	r := newRateLimit(clk, f)
	defer r.Close()
	r.ObserveOutcomes([]Outcome{{Task: "l1", Attempt: 1, Kind: OutcomeFailure, Locality: Local}, {Task: "r1", Attempt: 1, Kind: OutcomeFailure, Locality: Remote}}, clk.Now())
	waitUntil(t, r.idle)
	f.mu.Lock()
	calls := append([]string(nil), f.calls...)
	f.mu.Unlock()
	if len(calls) != 1 || calls[0] != "r1" {
		t.Fatalf("only the remote failure should be diagnosed, got %v", calls)
	}
	if got := r.Sample(context.Background(), RunSnapshot{}); got.MaxRemoteSlots != Unlimited {
		t.Fatalf("a failure with no limit evidence must not throttle: %+v", got)
	}
}

// The sensor reads state and nothing else: it can only lower how many pending
// remote tasks start, and it has no way to retry, reset or rewrite a failed cell.
func TestRateLimitNeverTouchesTheStateItObserves(t *testing.T) {
	clk := newFakeClock()
	f := &limitFake{evidence: LimitEvidence{Limited: true}}
	e := NewEvaluator(Options{Clock: clk, Workers: 4, ExitDwell: time.Second, RampThrottle: time.Second, Sync: true, Locality: func(*model.State, string) Locality { return Remote }},
		Registration{Sensor: newRateLimit(clk, f), Policy: FailOpen, Inline: true})
	defer e.Close()
	s := rows(map[string]model.TaskState{"f1": {Status: "running", Attempt: 1}, "p1": {Status: "pending"}})
	e.Admit(s, []string{"p1"}, map[string]bool{"f1": true}, 3) // seeds the tracker
	s.Tasks["f1"].Status = "failed"
	s.Tasks["f1"].Error = "429 too many requests"
	before := *s.Tasks["f1"]
	pending := *s.Tasks["p1"]
	e.Admit(s, []string{"p1"}, map[string]bool{}, 3) // the failure is read and queued for diagnosis
	rl := e.entries[0].reg.Sensor.(*RateLimit)
	waitUntil(t, rl.idle)
	got, env := e.Admit(s, []string{"p1"}, map[string]bool{}, 3)
	if len(got) != 0 || env.MaxRemoteSlots != 0 {
		t.Fatalf("a pending remote task must wait out the cooldown: admitted %v under %+v", got, env)
	}
	if !reflect.DeepEqual(*s.Tasks["f1"], before) || !reflect.DeepEqual(*s.Tasks["p1"], pending) {
		t.Fatal("the sensor modified the run state")
	}
	if s.Tasks["f1"].Status != "failed" {
		t.Fatal("a failed cell was changed; the sensor must never retry")
	}
}

func TestRateLimitKeepsItsLimitInsideTheEffectiveEnvelope(t *testing.T) {
	clk := newFakeClock()
	r := newRateLimit(clk, &limitFake{})
	defer r.Close()
	tight := Open()
	tight.MaxRemoteSlots = 1
	for i := 0; i < 20; i++ {
		r.ObserveOutcomes([]Outcome{{Task: "r", Kind: OutcomeSuccess, Locality: Remote}}, clk.Now())
		r.ObserveEffective(tight)
	}
	r.mu.Lock()
	limit := r.aimd.Limit
	r.mu.Unlock()
	if limit > 2 {
		t.Fatalf("the limiter wound up to %d under a remote cap of 1 plus headroom 1", limit)
	}
}

func fail(n int) []Outcome {
	var o []Outcome
	for i := 0; i < n; i++ {
		o = append(o, Outcome{Kind: OutcomeFailure})
	}
	return o
}

func ok(n int) []Outcome {
	var o []Outcome
	for i := 0; i < n; i++ {
		o = append(o, Outcome{Kind: OutcomeSuccess})
	}
	return o
}

func newBreaker(clk *fakeClock) *Breaker {
	return NewBreaker(BreakerConfig{Alpha: 0.3, Threshold: 0.5, MinOutcomes: 5, IntervalSeconds: 20, OpenSeconds: 60, HalfLifeSeconds: 60}, 4, clk)
}

func TestBreakerIgnoresABriefBadPatchAndSmallSamples(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(clk)
	b.ObserveOutcomes(fail(3), clk.Now()) // too few outcomes to judge
	if got := b.Sample(context.Background(), RunSnapshot{}); restricted(got) {
		t.Fatalf("tripped on a tiny sample: %+v", got)
	}
	b.ObserveOutcomes(ok(10), clk.Now())
	b.ObserveOutcomes(fail(2), clk.Now())
	for i := 0; i < 5; i++ {
		clk.Advance(10 * time.Second)
		if got := b.Sample(context.Background(), RunSnapshot{}); got.MaxLocalSlots != Unlimited || got.Hold != HoldNone {
			t.Fatalf("a brief bad patch shed capacity: %+v", got)
		}
	}
}

func TestBreakerShedsSlotsBeforeTrippingAndRecoversWhenTheRateFalls(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(clk)
	b.ObserveOutcomes(ok(6), clk.Now())
	b.ObserveOutcomes(fail(10), clk.Now())
	var slots []int
	held := false
	for i := 0; i < 120 && !held; i++ {
		b.ObserveOutcomes(fail(1), clk.Now()) // sustained: failures keep arriving
		got := b.Sample(context.Background(), RunSnapshot{})
		if got.Hold == HoldTransient {
			held = true
			break
		}
		if len(slots) == 0 || slots[len(slots)-1] != got.MaxLocalSlots {
			slots = append(slots, got.MaxLocalSlots)
		}
		clk.Advance(2 * time.Second)
	}
	if !held {
		t.Fatalf("a sustained failure rate never tripped the breaker (slots went %v)", slots)
	}
	if len(slots) < 3 || slots[0] != Unlimited {
		t.Fatalf("slots should be shed step by step before the hold, got %v", slots)
	}
	for i := 1; i < len(slots); i++ {
		if slots[i] >= slots[i-1] {
			t.Fatalf("shedding must only go down: %v", slots)
		}
	}
	// While open it holds, then half-opens to a single probe slot.
	clk.Advance(61 * time.Second)
	if got := b.Sample(context.Background(), RunSnapshot{}); got.Hold != HoldNone || got.MaxLocalSlots != 1 {
		t.Fatalf("after the open period one probe slot should be allowed: %+v", got)
	}
	// Successes bring the rate under half the threshold and the breaker resets.
	b.ObserveOutcomes(ok(30), clk.Now())
	if got := b.Sample(context.Background(), RunSnapshot{}); restricted(got) {
		t.Fatalf("the breaker should close once the failure rate is low: %+v", got)
	}
}

// With no new outcomes the rate decays, so an old bad patch cannot keep shedding
// capacity forever, and a tripped breaker (which admits nothing, so produces no
// outcomes) can reopen.
func TestBreakerRateDecaysWhenNoOutcomesArrive(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(clk)
	b.ObserveOutcomes(fail(12), clk.Now())
	b.Sample(context.Background(), RunSnapshot{})
	clk.Advance(21 * time.Second)
	if got := b.Sample(context.Background(), RunSnapshot{}); got.MaxLocalSlots >= Unlimited {
		t.Fatalf("a sustained failure rate should have shed a slot: %+v", got)
	}
	clk.Advance(5 * time.Minute)
	if got := b.Sample(context.Background(), RunSnapshot{}); restricted(got) {
		t.Fatalf("five minutes with no outcomes should have decayed the rate and closed the breaker: %+v", got)
	}
}

func TestSizeOfTheBreakerReadingNamesTheRate(t *testing.T) {
	clk := newFakeClock()
	b := newBreaker(clk)
	b.ObserveOutcomes(fail(12), clk.Now())
	b.Sample(context.Background(), RunSnapshot{})
	clk.Advance(21 * time.Second)
	got := b.Sample(context.Background(), RunSnapshot{})
	if got.Reason == "" || got.Sensor != "failure-rate" && b.Name() != "failure-rate" {
		t.Fatalf("a restriction must say why: %+v", got)
	}
}
