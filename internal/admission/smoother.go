package admission

import (
	"time"

	"github.com/grainulation/irrigator/internal/clock"
)

// Smoother turns the composed target envelope into the one that is applied. It
// is asymmetric on purpose: a limit that tightens takes effect at once, to
// protect the hardware or the quota, while a limit that relaxes must stay relaxed
// for ExitDwell and then grows one slot at a time, no faster than one per
// RampThrottle, so a worker admitted into recovered capacity has time to claim
// its memory and heat before the next one starts. A one-second spike therefore
// cannot make admission flap. Timing comes from the injected clock.
type Smoother struct {
	clk          clock.Clock
	ExitDwell    time.Duration
	RampThrottle time.Duration

	local  slotRamp
	remote slotRamp
	allow  boolRamp
	tier   tierRamp
}

// NewSmoother returns a smoother reading time from clk.
func NewSmoother(clk clock.Clock, exitDwell, rampThrottle time.Duration) *Smoother {
	return &Smoother{clk: clk, ExitDwell: exitDwell, RampThrottle: rampThrottle}
}

// Apply returns the envelope to enforce for target. ceiling is the most slots
// that can ever run at once (the worker limit): a limit above it means "no
// limit", and recovery only needs to ramp up to it.
func (s *Smoother) Apply(target Envelope, ceiling int) Envelope {
	now := s.clk.Now()
	tl, tr := clampSlots(target.MaxLocalSlots, ceiling), clampSlots(target.MaxRemoteSlots, ceiling)
	if target.Hold != HoldNone {
		// A hold is zero slots for recovery purposes, so lifting it ramps the same way.
		tl, tr = 0, 0
	}
	out := target
	out.Bindings = append([]BindingReason(nil), target.Bindings...)
	out.MaxLocalSlots = s.local.step(now, tl, s.ExitDwell, s.RampThrottle)
	out.MaxRemoteSlots = s.remote.step(now, tr, s.ExitDwell, s.RampThrottle)
	out.AllowRemote = s.allow.step(now, target.AllowRemote, s.ExitDwell)
	out.ModelTier = s.tier.step(now, target.ModelTier, s.ExitDwell)
	if out.MaxLocalSlots < tl {
		out.Bindings = append(out.Bindings, BindingReason{Field: "max_local_slots", Sensor: "ramp", Reason: "recovering after a limit", Severity: Soft})
	}
	if out.MaxRemoteSlots < tr {
		out.Bindings = append(out.Bindings, BindingReason{Field: "max_remote_slots", Sensor: "ramp", Reason: "recovering after a limit", Severity: Soft})
	}
	if out.AllowRemote != target.AllowRemote {
		out.Bindings = append(out.Bindings, BindingReason{Field: "allow_remote", Sensor: "dwell", Reason: "waiting out the exit dwell", Severity: Soft})
	}
	// Report unlimited as unlimited once the ramp has reached the ceiling.
	if target.MaxLocalSlots >= Unlimited && out.MaxLocalSlots >= ceiling {
		out.MaxLocalSlots = Unlimited
	}
	if target.MaxRemoteSlots >= Unlimited && out.MaxRemoteSlots >= ceiling {
		out.MaxRemoteSlots = Unlimited
	}
	return out
}

func clampSlots(v, ceiling int) int {
	if v > ceiling {
		return ceiling
	}
	if v < 0 {
		return 0
	}
	return v
}

type slotRamp struct {
	init        bool
	cur         int
	looserSince time.Time
	lastStep    time.Time
}

func (r *slotRamp) step(now time.Time, target int, dwell, throttle time.Duration) int {
	if !r.init {
		r.init, r.cur = true, target
		return r.cur
	}
	if target <= r.cur {
		r.cur = target
		r.looserSince = time.Time{}
		return r.cur
	}
	if r.looserSince.IsZero() {
		r.looserSince = now
	}
	if now.Sub(r.looserSince) >= dwell && (r.lastStep.IsZero() || now.Sub(r.lastStep) >= throttle) {
		r.cur++
		if r.cur > target {
			r.cur = target
		}
		r.lastStep = now
	}
	return r.cur
}

type boolRamp struct {
	init       bool
	cur        bool
	openSince  time.Time
	hasOpening bool
}

func (r *boolRamp) step(now time.Time, target bool, dwell time.Duration) bool {
	if !r.init {
		r.init, r.cur = true, target
		return r.cur
	}
	if !target {
		r.cur, r.hasOpening = false, false
		return r.cur
	}
	if r.cur {
		return true
	}
	if !r.hasOpening {
		r.hasOpening, r.openSince = true, now
	}
	if now.Sub(r.openSince) >= dwell {
		r.cur, r.hasOpening = true, false
	}
	return r.cur
}

type tierRamp struct {
	init        bool
	cur         Tier
	looserSince time.Time
	hasLooser   bool
}

func (r *tierRamp) step(now time.Time, target Tier, dwell time.Duration) Tier {
	if !r.init {
		r.init, r.cur = true, target
		return r.cur
	}
	if target <= r.cur {
		r.cur, r.hasLooser = target, false
		return r.cur
	}
	if !r.hasLooser {
		r.hasLooser, r.looserSince = true, now
	}
	if now.Sub(r.looserSince) >= dwell {
		r.cur++
		r.looserSince = now
		if r.cur >= target {
			r.cur, r.hasLooser = target, false
		}
	}
	return r.cur
}
