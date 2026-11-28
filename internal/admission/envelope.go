package admission

import (
	"context"
	"time"
)

// Unlimited is the slot count of a signal that has no opinion.
const Unlimited = 1 << 30

// Locality separates work that uses this machine's compute (an Ollama or mock
// binding) from work that uses a remote service (a hosted CLI binding), so that a
// hot local GPU does not starve remote tasks and a metered or offline link does
// not starve local ones.
type Locality int

const (
	Local Locality = iota
	Remote
)

func (l Locality) String() string {
	if l == Local {
		return "local"
	}
	return "remote"
}

// HoldKind says whether new admission is stopped, and whether that can end.
type HoldKind int

const (
	HoldNone HoldKind = iota
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

// Severity separates constraints that must never be overridden (offline, critical
// battery) from ones a future deadline or expedite option may relax (mild
// thermal pressure, a metered link).
type Severity int

const (
	Soft Severity = iota
	Hard
)

func (s Severity) String() string {
	if s == Hard {
		return "hard"
	}
	return "soft"
}

// Tier orders model quality for eligibility filtering. Higher is less restricted.
type Tier int

const (
	TierMinimal Tier = iota
	TierReduced
	TierFull
)

// BindingReason names the sensor that set a field of the composed envelope.
type BindingReason struct {
	Field    string   `json:"field"`
	Sensor   string   `json:"sensor"`
	Reason   string   `json:"reason"`
	Severity Severity `json:"severity"`
}

// ProbeStatus reports how trustworthy a reading is.
type ProbeStatus int

const (
	ProbeOK ProbeStatus = iota
	ProbePending
	ProbeStale
	ProbeFailed
)

func (p ProbeStatus) String() string {
	switch p {
	case ProbePending:
		return "pending"
	case ProbeStale:
		return "stale"
	case ProbeFailed:
		return "failed"
	}
	return "ok"
}

// Reading is one sensor's opinion over the envelope fields. The zero Reading is
// not neutral; use OpenReading for "no opinion".
type Reading struct {
	Sensor         string
	MaxLocalSlots  int
	MaxRemoteSlots int
	AllowRemote    bool
	ModelTier      Tier
	MinPoll        time.Duration
	Hold           HoldKind
	Severity       Severity
	Reason         string
	Probe          ProbeStatus
	ProbeErr       string
}

// OpenReading is a reading with no opinion on any field.
func OpenReading(sensor string) Reading {
	return Reading{Sensor: sensor, MaxLocalSlots: Unlimited, MaxRemoteSlots: Unlimited, AllowRemote: true, ModelTier: TierFull}
}

// Envelope is the composed decision for one evaluation.
type Envelope struct {
	MaxLocalSlots  int             `json:"max_local_slots"`
	MaxRemoteSlots int             `json:"max_remote_slots"`
	AllowRemote    bool            `json:"allow_remote"`
	ModelTier      Tier            `json:"model_tier"`
	MinPoll        time.Duration   `json:"min_poll_ns"`
	Hold           HoldKind        `json:"hold"`
	Bindings       []BindingReason `json:"bindings,omitempty"`
	// Degraded lists sensors whose probe failed or went stale and were treated
	// by their failure policy, so a status display can show that the picture is
	// incomplete.
	Degraded []string `json:"degraded,omitempty"`
}

// Open is the envelope that restricts nothing.
func Open() Envelope {
	return Envelope{MaxLocalSlots: Unlimited, MaxRemoteSlots: Unlimited, AllowRemote: true, ModelTier: TierFull}
}

// Restricted reports whether the envelope limits admission at all.
func (e Envelope) Restricted() bool {
	return e.Hold != HoldNone || !e.AllowRemote || e.MaxLocalSlots < Unlimited || e.MaxRemoteSlots < Unlimited || e.ModelTier < TierFull
}

// RunSnapshot is what a sensor may know about the run: how much work of each
// locality is already in flight, and the worker limit.
type RunSnapshot struct {
	Workers    int
	LocalBusy  int
	RemoteBusy int
}

// Sensor reports one signal. Sample must be cheap or bounded by ctx; the
// evaluator never calls it on the controller's critical path.
type Sensor interface {
	Name() string
	Sample(ctx context.Context, snap RunSnapshot) Reading
}

// StatefulSensor is a sensor with internal state, such as an additive-increase
// multiplicative-decrease limiter. It is told the envelope that actually applies
// after composition and smoothing so it can keep its own state within it
// (anti-windup): a sensor that is not the binding one must not keep growing, and
// then overshoot when the binding constraint is released.
type StatefulSensor interface {
	Sensor
	ObserveEffective(Envelope)
}

// FailurePolicy says what a sensor's failure means. Local operating-system
// probes fail open, so a broken probe never stops work; hard safety probes fail
// closed.
type FailurePolicy int

const (
	FailOpen FailurePolicy = iota
	FailClosed
)
