package admission

import (
	"context"
	"time"
)

// Unlimited is the slot count of a signal that has no opinion.
const Unlimited = 1 << 30

// locality separates work that uses this machine's compute (an Ollama or mock
// binding) from work that uses a remote service (a hosted CLI binding), so that a
// hot local GPU does not starve remote tasks and a metered or offline link does
// not starve local ones.
type locality int

const (
	local locality = iota
	remote
)

func (l locality) String() string {
	if l == local {
		return "local"
	}
	return "remote"
}

// HoldKind says whether new admission is stopped, and whether that can end.
type HoldKind int

const (
	holdNone HoldKind = iota
	// HoldTransient stops new admission until the signal clears. The run stays
	// alive: it is not blocked, and it resumes by itself.
	HoldTransient
	// HoldPermanent stops new admission and does not clear by itself.
	HoldPermanent
)

func (h HoldKind) String() string {
	switch h {
	case HoldTransient:
		return "transient"
	case HoldPermanent:
		return "permanent"
	}
	return "none"
}

// severity separates constraints that must never be overridden (offline, critical
// battery) from ones a future deadline or expedite option may relax (mild
// thermal pressure, a metered link).
type severity int

const (
	soft severity = iota
	hard
)

func (s severity) String() string {
	if s == hard {
		return "hard"
	}
	return "soft"
}

// tier orders model quality for eligibility filtering. Higher is less restricted.
type tier int

const (
	tierMinimal tier = iota
	tierReduced
	tierFull
)

// bindingReason names the sensor that set a field of the composed envelope.
type bindingReason struct {
	Field    string   `json:"field"`
	Sensor   string   `json:"sensor"`
	Reason   string   `json:"reason"`
	Severity severity `json:"severity"`
}

// probeStatus reports how trustworthy a reading is.
type probeStatus int

const (
	probeOK probeStatus = iota
	probePending
	probeStale
	probeFailed
)

func (p probeStatus) String() string {
	switch p {
	case probePending:
		return "pending"
	case probeStale:
		return "stale"
	case probeFailed:
		return "failed"
	}
	return "ok"
}

// Reading is one sensor's opinion over the envelope fields. The zero Reading is
// not neutral; use OpenReading for "no opinion".
type Reading struct {
	sensor         string
	MaxLocalSlots  int
	maxRemoteSlots int
	allowRemote    bool
	modelTier      tier
	minPoll        time.Duration
	hold           HoldKind
	severity       severity
	Reason         string
	probe          probeStatus
	probeErr       string
}

// OpenReading is a reading with no opinion on any field.
func OpenReading(sensor string) Reading {
	return Reading{sensor: sensor, MaxLocalSlots: Unlimited, maxRemoteSlots: Unlimited, allowRemote: true, modelTier: tierFull}
}

// Envelope is the composed decision for one evaluation.
type Envelope struct {
	MaxLocalSlots  int             `json:"max_local_slots"`
	MaxRemoteSlots int             `json:"max_remote_slots"`
	AllowRemote    bool            `json:"allow_remote"`
	ModelTier      tier            `json:"model_tier"`
	MinPoll        time.Duration   `json:"min_poll_ns"`
	Hold           HoldKind        `json:"hold"`
	Bindings       []bindingReason `json:"bindings,omitempty"`
	// Degraded lists sensors whose probe failed or went stale and were treated
	// by their failure policy, so a status display can show that the picture is
	// incomplete.
	Degraded []string `json:"degraded,omitempty"`
}

// open is the envelope that restricts nothing.
func open() Envelope {
	return Envelope{MaxLocalSlots: Unlimited, MaxRemoteSlots: Unlimited, AllowRemote: true, ModelTier: tierFull}
}

// restricted reports whether the envelope limits admission at all.
func (e Envelope) restricted() bool {
	return e.Hold != holdNone || !e.AllowRemote || e.MaxLocalSlots < Unlimited || e.MaxRemoteSlots < Unlimited || e.ModelTier < tierFull
}

// runSnapshot is what a sensor may know about the run: how much work of each
// locality is already in flight, and the worker limit.
type runSnapshot struct {
	workers    int
	localBusy  int
	remoteBusy int
}

// sensor reports one signal. Sample must be cheap or bounded by ctx; the
// evaluator never calls it on the controller's critical path.
type sensor interface {
	sensorName() string
	sample(ctx context.Context, snap runSnapshot) Reading
}

// statefulSensor is a sensor with internal state, such as an additive-increase
// multiplicative-decrease limiter. It is told the envelope that actually applies
// after composition and smoothing so it can keep its own state within it
// (anti-windup): a sensor that is not the binding one must not keep growing, and
// then overshoot when the binding constraint is released.
type statefulSensor interface {
	sensor
	observeEffective(Envelope)
}

// failurePolicy says what a sensor's failure means. Local operating-system
// probes fail open, so a broken probe never stops work; hard safety probes fail
// closed.
type failurePolicy int

const (
	failOpen failurePolicy = iota
	failClosed
)
