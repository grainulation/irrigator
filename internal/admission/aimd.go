package admission

// AIMD is an additive-increase multiplicative-decrease limit, the shape of a
// rate-limit cooldown: it grows by one after a success and halves after a limit
// signal. It exists to show and test the anti-windup rule that every stateful
// sensor must follow.
//
// Anti-windup: when another sensor is the binding constraint, an AIMD sensor
// that keeps counting successes would wind its limit up to the worker count, and
// then the moment the binding constraint released it would admit a burst far
// above what was last known to be safe. So the limit is clamped to the envelope
// that actually applies plus Headroom, and it does not decay or grow while that
// envelope admits nothing.
type AIMD struct {
	Limit    int // current limit; starts at Max
	Max      int
	Min      int
	Headroom int
	// frozen is set while the effective envelope admits nothing for this
	// locality: successes that finish meanwhile must not grow the limit back.
	frozen bool
	// ceil is the clamp from the last observation (0 for none), so successes that
	// finish between two observations cannot wind the limit past it.
	ceil int
}

// NewAIMD starts at max.
func NewAIMD(max, min, headroom int) *AIMD {
	return &AIMD{Limit: max, Max: max, Min: min, Headroom: headroom}
}

// OnSuccess grows the limit by one, up to Max, unless the envelope currently
// admits nothing for this locality.
func (a *AIMD) OnSuccess() {
	if a.frozen {
		return
	}
	if a.Limit < a.Max && (a.ceil == 0 || a.Limit < a.ceil) {
		a.Limit++
	}
}

// OnLimit halves the limit, down to Min.
func (a *AIMD) OnLimit() {
	a.Limit /= 2
	if a.Limit < a.Min {
		a.Limit = a.Min
	}
}

// Observe clamps the limit to what the effective envelope allows for loc.
func (a *AIMD) Observe(env Envelope, loc Locality) {
	effective := env.MaxLocalSlots
	if loc == Remote {
		effective = env.MaxRemoteSlots
	}
	a.frozen = env.Hold != HoldNone || effective <= 0 || (loc == Remote && !env.AllowRemote)
	if a.frozen {
		return // admits nothing: freeze, neither decay nor growth
	}
	if effective >= Unlimited {
		a.ceil = 0
		return
	}
	ceiling := effective + a.Headroom
	a.ceil = ceiling
	if a.Limit > ceiling {
		a.Limit = ceiling
	}
	if a.Limit < a.Min {
		a.Limit = a.Min
	}
}
