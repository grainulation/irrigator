package checkpoint

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/grainulation/irrigator/internal/model"
)

var ChannelName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

func PlanMessaging(p model.Plan) *model.MessagingExperiment {
	if p.Experiments == nil || p.Experiments.Messaging == nil || !p.Experiments.Messaging.Enabled {
		return nil
	}
	return p.Experiments.Messaging
}

// Kept outside generic validation so already-admitted plans stay readable by
// revisions and recovery. New admission rejects ambiguous declarations; actual
// publication also fails closed for a plan that cannot publish all fields.
// Item names identify one output per (channel, task), not per field.
func ValidateChannelPublishAdmission(p model.Plan) error {
	m := PlanMessaging(p)
	if m == nil {
		return nil
	}
	seen := map[[2]string]string{}
	for _, pub := range m.Publish {
		key := [2]string{pub.Channel, pub.Task}
		if field, ok := seen[key]; ok && field != pub.Field {
			return fmt.Errorf("experiments.messaging: channel %q task %q declares multiple publications (%q and %q); use one output field per channel and task", pub.Channel, pub.Task, field, pub.Field)
		}
		seen[key] = pub.Field
	}
	return nil
}

// Campaign admission has not assigned real authority yet. It may defer only
// the binding requirement; channel names, task/field contracts and all other
// experiment checks still apply before any grant is allocated.
func validateExperimentsContext(p model.Plan, campaignAdmission bool) error {
	if p.Experiments == nil {
		return nil
	}
	m := p.Experiments.Messaging
	if m == nil {
		return errors.New("experiments block must name an experiment (messaging)")
	}
	if !m.Enabled {
		if len(m.Edges)+len(m.Publish)+len(m.Subscribe) > 0 {
			return errors.New("experiments.messaging declares edges or channels but is not enabled")
		}
		return nil
	}
	if m.MaxBytes < 0 || m.MaxBytes > 16384 || m.MaxPerTask < 0 || m.MaxPerTask > 32 || m.MaxPerRun < 0 || m.MaxPerRun > 1024 || m.MaxHops < 0 || m.MaxHops > 4 {
		return errors.New("experiments.messaging limits: max_bytes ≤ 16384, max_per_task ≤ 32, max_per_run ≤ 1024, max_hops ≤ 4")
	}
	if m.CrossSwarmGate != "" && m.CrossSwarmGate != "review" {
		return errors.New(`experiments.messaging.cross_swarm_gate must be "review" or omitted`)
	}
	if len(m.Edges) > 64 || len(m.Publish) > 16 || len(m.Subscribe) > 16 {
		return errors.New("experiments.messaging: at most 64 edges, 16 publishes and 16 subscriptions")
	}
	tasks := map[string]model.Task{}
	for _, t := range p.Tasks {
		tasks[t.ID] = t
	}
	declares := func(t model.Task, field string) bool {
		for _, r := range t.Requires {
			if r == field {
				return true
			}
		}
		_, ok := t.Outputs[field]
		return ok
	}
	next := map[string][]string{}
	for _, e := range m.Edges {
		from, ok := tasks[e.From]
		if _, okTo := tasks[e.To]; !ok || !okTo || e.From == e.To {
			return fmt.Errorf("experiments.messaging edge %s→%s must join two different plan tasks", e.From, e.To)
		}
		if !declares(from, e.Field) {
			return fmt.Errorf("experiments.messaging edge %s→%s: field %q is not a declared output of %s", e.From, e.To, e.Field, e.From)
		}
		next[e.From] = append(next[e.From], e.To)
	}
	_, _, _, maxHops := m.Limits()
	// Edges form a DAG whose longest chain is at most max_hops: no loops, no storms.
	depths := map[string]int{}
	var depth func(string, map[string]bool) (int, error)
	depth = func(id string, path map[string]bool) (int, error) {
		if d, ok := depths[id]; ok {
			return d, nil
		}
		if path[id] {
			return 0, fmt.Errorf("experiments.messaging edges form a cycle through %s", id)
		}
		path[id] = true
		defer delete(path, id)
		best := 0
		for _, to := range next[id] {
			d, err := depth(to, path)
			if err != nil {
				return 0, err
			}
			if d+1 > best {
				best = d + 1
			}
		}
		depths[id] = best
		return best, nil
	}
	for id := range next {
		d, err := depth(id, map[string]bool{})
		if err != nil {
			return err
		}
		if d > maxHops {
			return fmt.Errorf("experiments.messaging: a note chain from %s is %d hops; max_hops is %d", id, d, maxHops)
		}
	}
	if (len(m.Publish) > 0 || len(m.Subscribe) > 0) && p.Campaign == nil && !campaignAdmission {
		return errors.New("experiments.messaging channels connect runs of one campaign; admit this plan with irrigator campaign admit")
	}
	for _, c := range m.Publish {
		t, ok := tasks[c.Task]
		if !ChannelName.MatchString(c.Channel) || !ok || !declares(t, c.Field) {
			return fmt.Errorf("experiments.messaging publish %q: needs a channel name (a-z, 0-9, -) and a declared output of a plan task", c.Channel)
		}
	}
	for _, c := range m.Subscribe {
		if _, ok := tasks[c.Task]; !ChannelName.MatchString(c.Channel) || !ok {
			return fmt.Errorf("experiments.messaging subscribe %q: needs a channel name and a plan task", c.Channel)
		}
	}
	return nil
}
