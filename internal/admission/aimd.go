package admission

// aimd is an additive-increase multiplicative-decrease limit, the shape of a
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
type aimd struct {
	limit    int // current limit; starts at Max
	max      int
	min      int
	headroom int
	// frozen is set while the effective envelope admits nothing for this
	// locality: successes that finish meanwhile must not grow the limit back.
	frozen bool
	// ceil is the clamp from the last observation (0 for none), so successes that
	// finish between two observations cannot wind the limit past it.
	ceil int
}

// newAIMD starts at max.
func newAIMD(max, min, headroom int) *aimd {
	return &aimd{limit: max, max: max, min: min, headroom: headroom}
}

// onSuccess grows the limit by one, up to Max, unless the envelope currently
// admits nothing for this locality.
func (a *aimd) onSuccess() {
	if a.frozen {
		return
	}
	if a.limit < a.max && (a.ceil == 0 || a.limit < a.ceil) {
		a.limit++
	}
}

// onLimit halves the limit, down to Min.
func (a *aimd) onLimit() {
	a.limit /= 2
	if a.limit < a.min {
		a.limit = a.min
	}
}

// observe clamps the limit to what the effective envelope allows for loc.
func (a *aimd) observe(env Envelope, loc locality) {
	effective := env.MaxLocalSlots
	if loc == remote {
		effective = env.MaxRemoteSlots
	}
	a.frozen = env.Hold != holdNone || effective <= 0 || (loc == remote && !env.AllowRemote)
	if a.frozen {
		return // admits nothing: freeze, neither decay nor growth
	}
	if effective >= Unlimited {
		a.ceil = 0
		return
	}
	ceiling := effective + a.headroom
	a.ceil = ceiling
	if a.limit > ceiling {
		a.limit = ceiling
	}
	if a.limit < a.min {
		a.limit = a.min
	}
}
