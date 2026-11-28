package admission

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/grainulation/irrigator/internal/clock"
)

// runCap reports the concurrency cap that the plan sizing gate imposes on this
// pass. The sizing gate stays authoritative for the slot count and for its
// path-conflict filtering; this sensor only makes its cap visible to the
// envelope, so one status line shows every constraint and a stateful sensor
// clamps itself against the cap that really applies.
type runCap struct {
	mu     sync.Mutex
	cap    int
	reason string
}

// newRunCap returns a sensor with no cap.
func newRunCap() *runCap { return &runCap{cap: Unlimited} }

func (r *runCap) sensorName() string { return "sizing" }

// Set records the total concurrency the sizing gate allows this pass.
func (r *runCap) Set(totalCap int, reason string) {
	r.mu.Lock()
	r.cap, r.reason = totalCap, reason
	r.mu.Unlock()
}

func (r *runCap) sample(context.Context, runSnapshot) Reading {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := OpenReading(r.sensorName())
	if r.cap < Unlimited {
		out.MaxLocalSlots, out.maxRemoteSlots, out.severity, out.Reason = r.cap, r.cap, hard, r.reason
	}
	return out
}

// LimitEvidence is what a limit diagnoser found in one attempt's captured output.
type LimitEvidence struct {
	Limited bool
	ResetAt *time.Time
}

// limitDiagnoser inspects one finished attempt's captured provider output. The
// controller supplies provider.DiagnoseLimit; it decodes the whole checkpoint,
// so the sensor only ever calls it off the scheduling pass.
type limitDiagnoser func(root, task string, attempt int, now time.Time) (LimitEvidence, error)

// rateLimitConfig tunes the rate-limit cooldown.
type rateLimitConfig struct {
	DefaultCooldownSeconds float64 `json:"default_cooldown_seconds"`
	MaxCooldownSeconds     float64 `json:"max_cooldown_seconds"`
	Headroom               int     `json:"headroom"`
}

// rateLimit is a cooldown for provider rate limits (HTTP 429, quota). When a
// remote attempt fails with limit evidence in its captured output, new remote
// admissions stop until the provider's stated reset (or a default), then resume
// under an additive-increase multiplicative-decrease limit.
//
// It only ever lowers how many pending remote tasks may start. It never retries,
// resets or touches a failed cell, and it never reads anything but state.
type rateLimit struct {
	root     string
	cfg      rateLimitConfig
	diagnose limitDiagnoser
	workers  int
	clk      clock.Clock

	mu        sync.Mutex
	aimd      *aimd
	until     time.Time
	resetAt   time.Time
	queue     chan outcome
	stop      chan struct{}
	done      chan struct{}
	pending   int
	diagnosed int
}

// newRateLimit returns the sensor. clk supplies the time.
func newRateLimit(root string, cfg rateLimitConfig, workers int, clk clock.Clock, diagnose limitDiagnoser) *rateLimit {
	r := &rateLimit{root: root, cfg: cfg, diagnose: diagnose, workers: workers, clk: clk, aimd: newAIMD(workers, 1, cfg.Headroom), queue: make(chan outcome, 256), stop: make(chan struct{}), done: make(chan struct{})}
	go r.drain(r.queue, r.stop, r.done)
	return r
}

func (r *rateLimit) sensorName() string { return "provider-limit" }

// Close stops the background diagnosis goroutine and waits for it: queued
// attempts that have not been diagnosed yet are dropped, and no diagnosis starts
// after Close returns.
func (r *rateLimit) Close() {
	r.mu.Lock()
	if r.queue != nil {
		close(r.stop)
		close(r.queue)
		r.queue = nil
	}
	r.mu.Unlock()
	<-r.done
}

func (r *rateLimit) idle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending == 0
}

// observeOutcomes feeds finished remote attempts: successes grow the limit, and a
// failure is queued for diagnosis in the background.
func (r *rateLimit) observeOutcomes(outs []outcome, _ time.Time) {
	for _, o := range outs {
		if o.locality != remote {
			continue
		}
		r.mu.Lock()
		switch o.kind {
		case outcomeSuccess:
			// Successes that finish during a cooldown are attempts started before
			// the limit; they say nothing about whether the limit has lifted.
			if !r.clk.Now().Before(r.until) {
				r.aimd.onSuccess()
			}
		case outcomeFailure, outcomeUncertain:
			// The send never blocks (it has a default) and happens under the lock,
			// so it cannot race Close, which closes the channel under the same lock.
			if r.queue != nil {
				select {
				case r.queue <- o:
					r.pending++
				default: // a flood of failures: drop the overflow rather than block the pass
				}
			}
		}
		r.mu.Unlock()
	}
}

// drain owns its channel: Close closes it and clears the field, and the loop must
// not read the field.
func (r *rateLimit) drain(queue <-chan outcome, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for o := range queue {
		select {
		case <-stop:
			r.mu.Lock()
			r.pending--
			r.mu.Unlock()
			continue
		default:
		}
		now := r.clk.Now()
		ev, err := r.diagnose(r.root, o.task, o.attempt, now)
		r.mu.Lock()
		r.pending--
		r.diagnosed++
		if err == nil && ev.Limited {
			// Several in-flight attempts fail on one provider limit. While a cooldown
			// is already running they are the same signal, not new ones, so they
			// halve the limit once rather than once per attempt.
			if !now.Before(r.until) {
				r.aimd.onLimit()
			}
			cooldown := seconds(r.cfg.DefaultCooldownSeconds)
			if ev.ResetAt != nil {
				// The provider said when it resets; a reset that has already passed
				// leaves nothing to wait out.
				cooldown = max(ev.ResetAt.Sub(now), 0)
				r.resetAt = *ev.ResetAt
			}
			if limit := seconds(r.cfg.MaxCooldownSeconds); limit > 0 && cooldown > limit {
				cooldown = limit
			}
			if until := now.Add(cooldown); cooldown > 0 && until.After(r.until) {
				r.until = until
			}
		}
		r.mu.Unlock()
	}
}

// observeEffective keeps the limit inside the envelope that really applies, so a
// limiter that is not the binding constraint does not wind up.
func (r *rateLimit) observeEffective(env Envelope) {
	r.mu.Lock()
	r.aimd.observe(env, remote)
	r.mu.Unlock()
}

func (r *rateLimit) sample(context.Context, runSnapshot) Reading {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := OpenReading(r.sensorName())
	now := r.clk.Now()
	switch {
	case now.Before(r.until):
		out.maxRemoteSlots, out.severity = 0, soft
		out.Reason = fmt.Sprintf("provider limit: cooling down for %s more", r.until.Sub(now).Round(time.Second))
	case r.aimd.limit < r.aimd.max:
		out.maxRemoteSlots, out.severity = r.aimd.limit, soft
		out.Reason = fmt.Sprintf("recovering from a provider limit (%d of %d remote slots)", r.aimd.limit, r.aimd.max)
	}
	return out
}

// breakerConfig tunes the failure-rate circuit breaker.
type breakerConfig struct {
	Alpha           float64 `json:"alpha"`
	Threshold       float64 `json:"threshold"`
	MinOutcomes     int     `json:"min_outcomes"`
	IntervalSeconds float64 `json:"interval_seconds"`
	OpenSeconds     float64 `json:"open_seconds"`
	// HalfLifeSeconds is how fast the rate decays while no outcomes arrive: no
	// news is not bad news forever, and a tripped breaker admits nothing that
	// could produce news.
	HalfLifeSeconds float64 `json:"half_life_seconds"`
}

// breaker watches the failure rate of recent attempts as an exponentially
// weighted moving average. While the rate stays above the threshold it sheds one
// slot per interval, with the interval shrinking as 1/sqrt(steps) in the manner
// of CoDel, so a brief bad patch costs nothing and a sustained one is reduced
// quickly. Only when every slot has been shed does it trip a transient hold, for
// OpenSeconds, after which it lets one probe slot through. It recovers when the
// rate falls below half the threshold.
type breaker struct {
	cfg     breakerConfig
	workers int
	clk     clock.Clock

	mu        sync.Mutex
	ewma      float64
	lastAt    time.Time
	n         int
	above     time.Time
	lastBadAt time.Time // when the last failed or uncertain attempt finished
	nextShed  time.Time
	shed      int
	openUntil time.Time
}

// newBreaker returns the breaker for a run of the given worker count.
func newBreaker(cfg breakerConfig, workers int, clk clock.Clock) *breaker {
	return &breaker{cfg: cfg, workers: workers, clk: clk}
}

func (b *breaker) sensorName() string { return "failure-rate" }

// rate is the failure rate as of now: the moving average, decayed by the time
// since the last outcome.
func (b *breaker) rate(now time.Time) float64 {
	if b.lastAt.IsZero() || b.cfg.HalfLifeSeconds <= 0 {
		return b.ewma
	}
	return b.ewma * math.Pow(0.5, now.Sub(b.lastAt).Seconds()/b.cfg.HalfLifeSeconds)
}

func (b *breaker) observeOutcomes(outs []outcome, _ time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clk.Now()
	for _, o := range outs {
		x := 0.0
		if o.kind != outcomeSuccess {
			x = 1
			b.lastBadAt = now
		}
		if b.n == 0 {
			b.ewma = x
		} else {
			b.ewma = b.cfg.Alpha*x + (1-b.cfg.Alpha)*b.rate(now)
		}
		b.lastAt = now
		b.n++
	}
}

func (b *breaker) sample(context.Context, runSnapshot) Reading {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := OpenReading(b.sensorName())
	now := b.clk.Now()
	interval := seconds(b.cfg.IntervalSeconds)
	rate := b.rate(now)
	switch {
	case b.n >= b.cfg.MinOutcomes && rate > b.cfg.Threshold:
		if b.above.IsZero() {
			// The shed clock starts when the rate first goes bad, and restarts
			// from here each time it comes back over the threshold.
			b.above = now
			b.nextShed = now.Add(time.Duration(float64(interval) / math.Sqrt(float64(b.shed+1))))
		}
		for b.shed < b.workers && !now.Before(b.nextShed) {
			b.shed++
			b.nextShed = b.nextShed.Add(time.Duration(float64(interval) / math.Sqrt(float64(b.shed+1))))
		}
		if b.shed >= b.workers {
			switch {
			case b.openUntil.IsZero():
				b.openUntil = now.Add(seconds(b.cfg.OpenSeconds))
			case !now.Before(b.openUntil) && b.lastBadAt.After(b.openUntil):
				// A half-open probe failed after the open period and the rate is
				// still bad: open again rather than staying half-open forever. A
				// probe that succeeded does not re-open it; the rate is falling.
				b.openUntil = now.Add(seconds(b.cfg.OpenSeconds))
			}
		}
	case rate < b.cfg.Threshold/2 || b.n < b.cfg.MinOutcomes:
		b.above, b.shed, b.openUntil = time.Time{}, 0, time.Time{}
	default:
		// In the band between half the threshold and the threshold: keep what has
		// been shed, but stop the shed clock so a later crossing does not replay
		// the steps that were missed while the rate was acceptable.
		b.above = time.Time{}
	}
	why := fmt.Sprintf("failure rate %.0f%% over recent attempts", rate*100)
	switch {
	case !b.openUntil.IsZero() && now.Before(b.openUntil):
		out.hold, out.severity, out.Reason = HoldTransient, hard, why+": breaker open"
	case b.shed > 0:
		keep := b.workers - b.shed
		if keep < 1 {
			keep = 1 // half-open: one probe slot
		}
		out.MaxLocalSlots, out.maxRemoteSlots, out.severity, out.Reason = keep, keep, soft, why+fmt.Sprintf(": shedding to %d slots", keep)
	}
	return out
}
