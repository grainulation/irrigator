package admission

import (
	"fmt"
	"sort"
)

// Compose merges readings strictest-wins: the minimum of each slot limit and of
// the model tier, the AND of AllowRemote, the maximum poll interval and the
// strongest hold. Every field that ends up restricted records the sensors that
// bound it, so a status display can say why.
func Compose(readings []Reading) Envelope {
	env := Open()
	for _, r := range readings {
		if r.MaxLocalSlots < env.MaxLocalSlots {
			env.MaxLocalSlots = r.MaxLocalSlots
		}
		if r.MaxRemoteSlots < env.MaxRemoteSlots {
			env.MaxRemoteSlots = r.MaxRemoteSlots
		}
		env.AllowRemote = env.AllowRemote && r.AllowRemote
		if r.ModelTier < env.ModelTier {
			env.ModelTier = r.ModelTier
		}
		if r.MinPoll > env.MinPoll {
			env.MinPoll = r.MinPoll
		}
		if r.Hold > env.Hold {
			env.Hold = r.Hold
		}
		if r.Probe == ProbeFailed || r.Probe == ProbeStale {
			env.Degraded = append(env.Degraded, r.Sensor)
		}
	}
	bind := func(field string, r Reading) {
		env.Bindings = append(env.Bindings, BindingReason{Field: field, Sensor: r.Sensor, Reason: r.Reason, Severity: r.Severity})
	}
	for _, r := range readings {
		if r.MaxLocalSlots == env.MaxLocalSlots && r.MaxLocalSlots < Unlimited {
			bind("max_local_slots", r)
		}
		if r.MaxRemoteSlots == env.MaxRemoteSlots && r.MaxRemoteSlots < Unlimited {
			bind("max_remote_slots", r)
		}
		if !r.AllowRemote {
			bind("allow_remote", r)
		}
		if r.ModelTier == env.ModelTier && r.ModelTier < TierFull {
			bind("model_tier", r)
		}
		if r.MinPoll > 0 && r.MinPoll == env.MinPoll {
			bind("min_poll", r)
		}
		if r.Hold != HoldNone && r.Hold == env.Hold {
			bind("hold", r)
		}
	}
	sort.SliceStable(env.Bindings, func(i, j int) bool {
		if env.Bindings[i].Field != env.Bindings[j].Field {
			return env.Bindings[i].Field < env.Bindings[j].Field
		}
		return env.Bindings[i].Sensor < env.Bindings[j].Sensor
	})
	sort.Strings(env.Degraded)
	return env
}

// Summary is a one-line description of what binds the envelope, for status
// output, such as "local 2/6 (power: thermal serious); remote paused (network:
// offline)".
func (e Envelope) Summary(workers int) string {
	if !e.Restricted() {
		return "open"
	}
	reason := func(field string) string {
		for _, b := range e.Bindings {
			if b.Field == field {
				return b.Sensor + ": " + b.Reason
			}
		}
		return ""
	}
	if e.Hold != HoldNone {
		return fmt.Sprintf("hold %s (%s)", e.Hold, reason("hold"))
	}
	out := ""
	add := func(s string) {
		if out != "" {
			out += "; "
		}
		out += s
	}
	if e.MaxLocalSlots < Unlimited {
		add(fmt.Sprintf("local %d/%d (%s)", e.MaxLocalSlots, workers, reason("max_local_slots")))
	}
	if !e.AllowRemote {
		add(fmt.Sprintf("remote paused (%s)", reason("allow_remote")))
	} else if e.MaxRemoteSlots < Unlimited {
		add(fmt.Sprintf("remote %d/%d (%s)", e.MaxRemoteSlots, workers, reason("max_remote_slots")))
	}
	if e.ModelTier < TierFull {
		add(fmt.Sprintf("tier %d (%s)", e.ModelTier, reason("model_tier")))
	}
	if out == "" {
		out = "restricted"
	}
	return out
}
