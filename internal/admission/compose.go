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
	env := open()
	for _, r := range readings {
		if r.MaxLocalSlots < env.MaxLocalSlots {
			env.MaxLocalSlots = r.MaxLocalSlots
		}
		if r.maxRemoteSlots < env.MaxRemoteSlots {
			env.MaxRemoteSlots = r.maxRemoteSlots
		}
		env.AllowRemote = env.AllowRemote && r.allowRemote
		if r.modelTier < env.ModelTier {
			env.ModelTier = r.modelTier
		}
		if r.minPoll > env.MinPoll {
			env.MinPoll = r.minPoll
		}
		if r.hold > env.Hold {
			env.Hold = r.hold
		}
		if r.probe == probeFailed || r.probe == probeStale {
			env.Degraded = append(env.Degraded, r.sensor)
		}
	}
	bind := func(field string, r Reading) {
		env.Bindings = append(env.Bindings, bindingReason{Field: field, Sensor: r.sensor, Reason: r.Reason, Severity: r.severity})
	}
	for _, r := range readings {
		if r.MaxLocalSlots == env.MaxLocalSlots && r.MaxLocalSlots < Unlimited {
			bind("max_local_slots", r)
		}
		if r.maxRemoteSlots == env.MaxRemoteSlots && r.maxRemoteSlots < Unlimited {
			bind("max_remote_slots", r)
		}
		if !r.allowRemote {
			bind("allow_remote", r)
		}
		if r.modelTier == env.ModelTier && r.modelTier < tierFull {
			bind("model_tier", r)
		}
		if r.minPoll > 0 && r.minPoll == env.MinPoll {
			bind("min_poll", r)
		}
		if r.hold != holdNone && r.hold == env.Hold {
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

// summary is a one-line description of what binds the envelope, for status
// output, such as "local 2/6 (power: thermal serious); remote paused (network:
// offline)".
func (e Envelope) summary(workers int) string {
	if !e.restricted() {
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
	if e.Hold != holdNone {
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
	if e.ModelTier < tierFull {
		add(fmt.Sprintf("tier %d (%s)", e.ModelTier, reason("model_tier")))
	}
	if out == "" {
		out = "restricted"
	}
	return out
}
