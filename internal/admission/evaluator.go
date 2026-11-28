package admission

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/grainulation/irrigator/internal/clock"
	"github.com/grainulation/irrigator/internal/model"
)

// sampleTimeout bounds one probe, so a hung operating-system command cannot pin a
// sampling goroutine for long.
const sampleTimeout = 5 * time.Second

// Registration adds a sensor with its failure policy and how long a reading stays
// fresh. Slow sensors (battery, network) use a long TTL and fast ones a short
// one, so the controller loop is never what waits for a probe.
type Registration struct {
	Sensor Sensor
	Policy FailurePolicy
	TTL    time.Duration
	// Inline sensors only read memory (run-derived counters), so they are
	// sampled on every evaluation instead of in the background.
	Inline bool
}

// Options configures an Evaluator.
type Options struct {
	Clock        clock.Clock
	Workers      int
	ExitDwell    time.Duration
	RampThrottle time.Duration
	// Preset names the configuration in the status file.
	Preset string
	// StatusPath is where transitions are written; empty disables the file.
	StatusPath     string
	StatusDebounce time.Duration
	// Sync samples inline instead of in the background. Tests use it for
	// deterministic readings; the controller never does.
	Sync bool
	// Locality overrides how tasks are classified; nil uses the task's binding.
	Locality func(s *model.State, id string) Locality
	// Tier gives a task's declared model tier and whether it declares one. Tasks
	// that declare none are never filtered by tier.
	Tier func(s *model.State, id string) (Tier, bool)
}

type entry struct {
	reg      Registration
	reading  Reading
	at       time.Time
	have     bool
	inflight bool
	started  time.Time // when the in-flight sample began
}

// failedProbeRetry is how soon a probe that failed is tried again, so one
// transient failure does not hold or hide a sensor for a whole TTL.
const failedProbeRetry = 2 * time.Second

// defaultTTL is the freshness of a background sensor registered without one.
const defaultTTL = 10 * time.Second

// Evaluator turns sensors into the envelope that governs new admission.
type Evaluator struct {
	opts     Options
	mu       sync.Mutex
	entries  []*entry
	smoother *Smoother
	classify *Classifier
	last     Envelope
	status   statusWriter
	warm     sync.Once
	tracker  *Tracker
	runCap   *RunCap
	// observers are the sensors that learn from finished attempts, found once.
	observers []OutcomeObserver
	// life bounds the background sampling goroutines; Close cancels and waits.
	life   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// closed is set under mu by Close, and no goroutine starts after it: a start
	// is decided, and counted in wg, under the same lock.
	closed bool
}

// NewEvaluator builds an evaluator over regs.
func NewEvaluator(opts Options, regs ...Registration) *Evaluator {
	if opts.Clock == nil {
		opts.Clock = clock.ActiveClock
	}
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.StatusDebounce <= 0 {
		opts.StatusDebounce = 2 * time.Second
	}
	e := &Evaluator{opts: opts, smoother: NewSmoother(opts.Clock, opts.ExitDwell, opts.RampThrottle), classify: NewClassifier(), last: Open(), tracker: NewTracker()}
	e.life, e.cancel = context.WithCancel(context.Background())
	for i := range regs {
		if !regs[i].Inline && regs[i].TTL <= 0 {
			regs[i].TTL = defaultTTL // a background sensor must always have a staleness bound
		}
		if rc, ok := regs[i].Sensor.(*RunCap); ok {
			e.runCap = rc
		}
		if ob, ok := regs[i].Sensor.(OutcomeObserver); ok {
			e.observers = append(e.observers, ob)
		}
	}
	for _, r := range regs {
		e.entries = append(e.entries, &entry{reg: r})
	}
	e.status = statusWriter{path: opts.StatusPath, debounce: opts.StatusDebounce, preset: opts.Preset}
	return e
}

// Evaluate returns the envelope to enforce now. Readings older than their TTL
// are refreshed in the background and the previous reading is used meanwhile.
func (e *Evaluator) Evaluate(ctx context.Context, snap RunSnapshot) Envelope {
	e.warmUp(ctx, snap)
	e.mu.Lock()
	now := e.opts.Clock.Now()
	var due []*entry
	for _, en := range e.entries {
		fresh := en.reg.TTL
		if en.have && en.reading.Probe == ProbeFailed && failedProbeRetry < fresh {
			fresh = failedProbeRetry // retry a failed probe soon, not a whole TTL later
		}
		if !en.have || en.reg.Inline || now.Sub(en.at) >= fresh {
			due = append(due, en)
		}
	}
	e.mu.Unlock()

	for _, en := range due {
		if e.opts.Sync || en.reg.Inline {
			e.refresh(ctx, en, snap)
			continue
		}
		e.mu.Lock()
		start := !en.inflight && !e.closed
		if start {
			en.inflight, en.started = true, now
			e.wg.Add(1)
		}
		e.mu.Unlock()
		if start {
			go func(en *entry) {
				defer e.wg.Done()
				e.refresh(e.life, en, snap)
			}(en)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now = e.opts.Clock.Now()
	readings := make([]Reading, 0, len(e.entries))
	for _, en := range e.entries {
		readings = append(readings, effectiveReading(en, now))
	}
	target := Compose(readings)
	eff := e.smoother.Apply(target, e.opts.Workers)
	e.last = eff
	for _, en := range e.entries {
		if st, ok := en.reg.Sensor.(StatefulSensor); ok {
			st.ObserveEffective(eff)
		}
	}
	e.status.observe(now, eff, snap, e.opts.Workers)
	return eff
}

// warmUpWait bounds how long the very first evaluation waits for first readings,
// so that a fail-closed sensor does not hold the start of a run for a whole idle
// polling interval while its first probe is still in flight. It happens once;
// every later evaluation is entirely non-blocking.
const warmUpWait = 500 * time.Millisecond

func (e *Evaluator) warmUp(ctx context.Context, snap RunSnapshot) {
	if e.opts.Sync {
		return
	}
	e.warm.Do(func() {
		e.mu.Lock()
		var pending []*entry
		for _, en := range e.entries {
			if !en.reg.Inline && !e.closed { // inline sensors are sampled by Evaluate itself
				pending = append(pending, en)
				en.inflight, en.started = true, e.opts.Clock.Now()
				e.wg.Add(1)
			}
		}
		e.mu.Unlock()
		var wg sync.WaitGroup
		for _, en := range pending {
			wg.Add(1)
			go func(en *entry) {
				defer wg.Done()
				defer e.wg.Done()
				e.refresh(e.life, en, snap)
			}(en)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-e.opts.Clock.After(warmUpWait):
		case <-ctx.Done():
		}
	})
}

func (e *Evaluator) refresh(ctx context.Context, en *entry, snap RunSnapshot) {
	sctx, cancel := context.WithTimeout(ctx, sampleTimeout)
	defer cancel()
	r := sampleSafely(sctx, en.reg.Sensor, snap)
	e.mu.Lock()
	// The reading lands and the flight ends in one critical section: a reader that
	// sees the new reading never also sees the entry still in flight, which would
	// stop the next due refresh from starting.
	en.reading, en.at, en.have, en.inflight = r, e.opts.Clock.Now(), true, false
	e.mu.Unlock()
}

func sampleSafely(ctx context.Context, s Sensor, snap RunSnapshot) (r Reading) {
	defer func() {
		if p := recover(); p != nil {
			r = Reading{Sensor: s.Name(), Probe: ProbeFailed, ProbeErr: fmt.Sprint("sensor panicked: ", p)}
		}
	}()
	r = s.Sample(ctx, snap)
	if r.Sensor == "" {
		r.Sensor = s.Name()
	}
	return r
}

// staleAfter is how old a reading may get before it is distrusted: three TTLs,
// and never less than two probe timeouts, so a slow but healthy probe is not
// declared stale while it is still being waited for.
func staleAfter(en *entry) time.Duration {
	limit := 3 * en.reg.TTL
	if min := 2 * sampleTimeout; limit < min {
		limit = min
	}
	return limit
}

// effectiveReading applies the failure policy. A sensor that has not reported yet,
// has gone stale, or failed is not trusted: a fail-open sensor then says nothing
// and is shown as degraded, a fail-closed one holds new admission.
//
// A reading that has merely aged because the controller made no pass for a while
// (a machine that slept, say) is not stale while a refresh is in flight and still
// within its timeout: the last reading stands until the new one lands.
func effectiveReading(en *entry, now time.Time) Reading {
	name := en.reg.Sensor.Name()
	bad := func(status ProbeStatus, why string) Reading {
		if en.reg.Policy == FailClosed {
			return Reading{Sensor: name, MaxLocalSlots: 0, MaxRemoteSlots: 0, AllowRemote: false, ModelTier: TierMinimal, Hold: HoldTransient, Severity: Hard, Reason: why, Probe: status, ProbeErr: why}
		}
		r := OpenReading(name)
		r.Probe, r.ProbeErr = status, why
		return r
	}
	switch {
	case !en.have:
		return bad(ProbePending, "no reading yet")
	case en.reading.Probe == ProbeFailed:
		return bad(ProbeFailed, en.reading.ProbeErr)
	case !en.reg.Inline && now.Sub(en.at) > staleAfter(en):
		if en.inflight && now.Sub(en.started) <= sampleTimeout {
			return en.reading
		}
		return bad(ProbeStale, "reading is stale")
	}
	return en.reading
}

// Last returns the most recent envelope.
func (e *Evaluator) Last() Envelope {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last
}

// Admit evaluates the envelope and returns which of ready may start, in order,
// at most slots of them. busy holds the tasks already in flight; their locality
// is counted against the per-locality limits. It never reorders ready and never
// changes what a task is bound to: it only decides whether it may start now.
func (e *Evaluator) Admit(s *model.State, ready []string, busy map[string]bool, slots int) ([]string, Envelope) {
	var snap RunSnapshot
	snap.Workers = e.opts.Workers
	for id := range busy {
		if s.Tasks[id] == nil {
			continue
		}
		if e.locality(s, id) == Local {
			snap.LocalBusy++
		} else {
			snap.RemoteBusy++
		}
	}
	e.observeOutcomes(s)
	env := e.Evaluate(context.Background(), snap)
	return e.filter(s, ready, snap, slots, env), env
}

// observeOutcomes finds the attempts that finished since the last pass and tells
// each sensor that learns from them. With no such sensor it does no work at all.
func (e *Evaluator) observeOutcomes(s *model.State) {
	if len(e.observers) == 0 {
		return
	}
	now := e.opts.Clock.Now()
	outs := e.tracker.Observe(s, now, func(id string) Locality { return e.locality(s, id) })
	if len(outs) == 0 {
		return
	}
	for _, ob := range e.observers {
		ob.ObserveOutcomes(outs, now)
	}
}

// NoteSizing records the concurrency cap the plan sizing gate imposed on this
// pass (Unlimited when it imposed none), so the envelope can show it.
func (e *Evaluator) NoteSizing(totalCap int, reason string) {
	if e.runCap != nil {
		e.runCap.Set(totalCap, reason)
	}
}

// Close stops background sampling, waits for it to finish, and then releases
// sensors that run background work of their own.
func (e *Evaluator) Close() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.cancel()
	e.wg.Wait()
	for _, en := range e.entries {
		if c, ok := en.reg.Sensor.(interface{ Close() }); ok {
			c.Close()
		}
	}
}

func (e *Evaluator) locality(s *model.State, id string) Locality {
	if e.opts.Locality != nil {
		return e.opts.Locality(s, id)
	}
	return e.classify.Locality(s, id)
}

func (e *Evaluator) tier(s *model.State, id string) (Tier, bool) {
	if e.opts.Tier == nil {
		return 0, false
	}
	return e.opts.Tier(s, id)
}

func (e *Evaluator) filter(s *model.State, ready []string, snap RunSnapshot, slots int, env Envelope) []string {
	if env.Hold != HoldNone || slots <= 0 {
		return nil
	}
	localFree := env.MaxLocalSlots - snap.LocalBusy
	remoteFree := env.MaxRemoteSlots - snap.RemoteBusy
	if !env.AllowRemote {
		remoteFree = 0
	}
	var admitted []string
	for _, id := range ready {
		if len(admitted) >= slots || (localFree <= 0 && remoteFree <= 0) {
			break
		}
		if t, declared := e.tier(s, id); declared && t > env.ModelTier {
			continue // the environment only allows a smaller model for now
		}
		if e.locality(s, id) == Local {
			if localFree > 0 {
				localFree--
				admitted = append(admitted, id)
			}
		} else if remoteFree > 0 {
			remoteFree--
			admitted = append(admitted, id)
		}
	}
	return admitted
}
