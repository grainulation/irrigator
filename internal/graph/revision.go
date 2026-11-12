package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

var

// Old checkpoints may retain a decision but no longer its original payload.
// Reserve that ID permanently rather than guessing whether a retry is identical.

(
	errRevisionRetry = errors.New("revision: affected set changed, retry")
	errRevisionSkip  = errors.New("revision: nothing to write")

	digitsRe = regexp.MustCompile(`[0-9.]+`)
)

func taskJSON(t model.Task) string { b, _ := json.Marshal(t); return string(b) }

func CloneState(s *model.State) (*model.State, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var c model.State
	if err = json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func samePayload(a, b model.RevisionProposal) bool {
	a.SubmittedAt, b.SubmittedAt = time.Time{}, time.Time{}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TerminalDecision(s *model.State, id string) *model.AdaptiveDecision {
	for i := len(s.Decisions) - 1; i >= 0; i-- {
		if d := &s.Decisions[i]; d.ProposalID == id && (d.Action == "apply" || d.Action == "reject") {
			return d
		}
	}
	return nil
}

func checkProposalShape(p model.RevisionProposal) error {
	if !checkpoint.ProposalIDRe.MatchString(p.ID) {
		return errors.New("proposal id must be 1..128 simple characters")
	}
	if strings.TrimSpace(p.Reason) == "" || len(p.Reason) > 1024 {
		return errors.New("proposal needs a reason of at most 1024 bytes")
	}
	if !checkpoint.FiniteNonNeg(p.ExpectedSavingsMS) || !checkpoint.FiniteNonNeg(p.SwitchCostMS) || !checkpoint.FiniteNonNeg(p.ExtraCostUSD) ||
		!checkpoint.FiniteNonNeg(p.Confidence) || p.Confidence > 1 {
		return errors.New("estimates must be finite and non-negative; confidence must be within 0..1")
	}
	if p.BaseRevision < 0 {
		return errors.New("base revision cannot be negative")
	}
	if len(p.Tasks) == 0 || len(p.Tasks) > checkpoint.MaxRevTasks {
		return fmt.Errorf("proposal must contain 1..%d tasks", checkpoint.MaxRevTasks)
	}
	return nil
}

func QueueRevision(root string, p model.RevisionProposal) error {
	if err := checkProposalShape(p); err != nil {
		return err
	}
	err := checkpoint.Update(root, func(s *model.State) error {
		digest := checkpoint.ProposalDigest(p)
		if old, ok := s.ProposalReceipts[p.ID]; ok {
			if old == digest {
				return nil
			}
			return fmt.Errorf("proposal %s is permanently reserved with a different or unavailable original payload", p.ID)
		}
		for _, old := range s.Proposals {
			if old.ID == p.ID {
				if samePayload(old, p) {
					return errRevisionSkip
				}
				return fmt.Errorf("proposal %s already exists with a different payload", p.ID)
			}
		}
		for _, d := range s.Decisions {
			if d.ProposalID == p.ID && (d.Action == "apply" || d.Action == "reject") {
				return fmt.Errorf("proposal %s was already decided", p.ID)
			}
		}
		if p.BaseRevision != s.GraphRevision {
			return fmt.Errorf("stale base revision %d: current graph revision is %d", p.BaseRevision, s.GraphRevision)
		}
		if len(s.ProposalReceipts) >= checkpoint.MaxProposalReceipts {
			return errors.New("proposal receipt capacity reached; use a new run instead of reusing proposal IDs")
		}
		if err := ValidateTurnRevision(s, p.Tasks); err != nil {
			return err
		}
		plan := s.Plan
		plan.Tasks = p.Tasks
		if err := checkpoint.Validate(plan); err != nil {
			return fmt.Errorf("invalid proposed graph: %w", err)
		}
		if len(s.Proposals) >= checkpoint.MaxProposals {
			PruneProposals(s)
			if len(s.Proposals) >= checkpoint.MaxProposals {
				return errors.New("proposal queue is full")
			}
		}
		if p.SubmittedAt.IsZero() {
			p.SubmittedAt = time.Now().UTC()
		}
		s.Proposals = append(s.Proposals, p)
		if s.ProposalReceipts == nil {
			s.ProposalReceipts = map[string]string{}
		}
		s.ProposalReceipts[p.ID] = digest
		s.Event("revision_queued", "", map[string]any{"proposal_id": p.ID, "base_revision": p.BaseRevision})
		return nil
	})
	if errors.Is(err, errRevisionSkip) {
		return nil
	}
	return err
}

// pruneProposals drops the oldest proposals that already have a terminal decision.
func PruneProposals(s *model.State) {
	kept := s.Proposals[:0:0]
	drop := len(s.Proposals) - checkpoint.MaxProposals/2
	for _, p := range s.Proposals {
		if drop > 0 && TerminalDecision(s, p.ID) != nil {
			drop--
			continue
		}
		kept = append(kept, p)
	}
	s.Proposals = kept
}

func AddDecision(s *model.State, d model.AdaptiveDecision) {
	s.Decisions = append(s.Decisions, d)
	if len(s.Decisions) > checkpoint.MaxDecisions {
		live := map[string]bool{}
		for _, p := range s.Proposals {
			live[p.ID] = true
		}
		last := map[string]int{}
		for i, x := range s.Decisions {
			last[x.ProposalID] = i
		}
		kept := make([]model.AdaptiveDecision, 0, checkpoint.MaxDecisions)
		excess := len(s.Decisions) - checkpoint.MaxDecisions
		for i, x := range s.Decisions {
			if excess > 0 && (!live[x.ProposalID] || last[x.ProposalID] != i) {
				excess--
				continue
			}
			kept = append(kept, x)
		}
		s.Decisions = kept
	}
	s.Event("adaptive_"+d.Action, d.ProposalID, d)
}

type evalResult struct {
	Kind     string // reject, defer, noop, ready
	reason   string
	decision model.AdaptiveDecision
	affected []string
	specs    []model.Task
	changed  map[string]bool
}

func hasCommand(cs []model.Check) bool {
	for _, c := range cs {
		if c.Kind == "command" {
			return true
		}
	}
	return false
}

// helperError bounds a brand-new node by its owner, or by the original plan.
func helperError(s *model.State, t model.Task, parent *model.TaskState) error {
	if hasCommand(t.Checks) {
		return errors.New("new nodes cannot introduce executable validators")
	}
	if t.Spawn != nil {
		return errors.New("new nodes cannot introduce spawn capability")
	}
	if parent != nil {
		if checkpoint.ProviderName(s, t) != checkpoint.ProviderName(s, parent.Spec) {
			return errors.New("new node cannot change provider capability")
		}
		if t.Effort != "" && t.Effort != parent.Spec.Effort {
			return errors.New("new node cannot change model effort")
		}
		if !checkpoint.Subset(t.Context, parent.Spec.Context) {
			return errors.New("new node context not granted by owner")
		}
		return nil
	}
	providers, ctx, rank := map[string]bool{s.Backend: true}, map[string]bool{}, 0
	for _, o := range s.Plan.Tasks {
		providers[checkpoint.ProviderName(s, o)] = true
		for _, c := range o.Context {
			ctx[c] = true
		}
		if r := checkpoint.EffortRank(o.Effort); r > rank {
			rank = r
		}
	}
	if !providers[checkpoint.ProviderName(s, t)] {
		return errors.New("new node provider is outside original plan capabilities")
	}
	if checkpoint.EffortRank(t.Effort) > rank {
		return errors.New("new node effort exceeds original plan")
	}
	for _, c := range t.Context {
		if !ctx[c] {
			return fmt.Errorf("new node context %s is outside original plan", c)
		}
	}
	return nil
}

func affectedClosure(s *model.State, seed []string) []string {
	set := map[string]bool{}
	add := func(id string) {
		for _, d := range managed.Descendants(s, id) {
			set[d] = true
		}
	}
	for _, id := range seed {
		if s.Tasks[id] != nil {
			add(id)
		}
	}
	for changed := true; changed; {
		changed = false
		for id, row := range s.Tasks {
			if set[id] {
				continue
			}
			deps := append([]string{}, row.Spec.DependsOn...)
			for src := range row.Spec.InputFields {
				deps = append(deps, src)
			}
			for _, d := range deps {
				if set[d] {
					add(id)
					changed = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func EvaluateRevision(root string, s *model.State, p model.RevisionProposal, now time.Time) *evalResult {
	res := &evalResult{changed: map[string]bool{}}
	rej := func(format string, a ...any) *evalResult {
		res.Kind, res.reason = "reject", fmt.Sprintf(format, a...)
		res.decision = model.AdaptiveDecision{ProposalID: p.ID, BaseRevision: p.BaseRevision, Time: now, Action: "reject", Reason: res.reason}
		return res
	}
	if err := ValidateTurnRevision(s, p.Tasks); err != nil {
		return rej("%v", err)
	}
	if p.BaseRevision != s.GraphRevision {
		return rej("stale base revision %d: current graph revision is %d", p.BaseRevision, s.GraphRevision)
	}
	if len(s.Revisions) >= checkpoint.MaxRevisions {
		return rej("revision history is full")
	}
	if err := checkProposalShape(p); err != nil {
		return rej("%v", err)
	}
	plan := s.Plan
	plan.Tasks = p.Tasks
	if err := checkpoint.Validate(plan); err != nil {
		return rej("invalid proposed graph: %v", err)
	}
	// Retired cells retain immutable evidence. New graphs must not alias a
	// current or archived cell even if the old spelling leaves the active graph.
	names := append([]model.Task{}, p.Tasks...)
	names = append(names, checkpoint.HistoricalCellTasks(s)...)
	if err := checkpoint.ValidateCellNames(names); err != nil {
		return rej("unsafe proposed cell names: %v", err)
	}
	retired := map[string]bool{}
	for _, a := range s.ArchivedTasks {
		retired[a.Spec.ID] = true
	}
	newByID := map[string]model.Task{}
	for _, t := range p.Tasks {
		newByID[t.ID] = t
	}
	// Original task IDs are stable goal/acceptance boundaries. Splitting their
	// implementation may add helpers, but cannot delete the obligation itself.
	for _, original := range s.Plan.Tasks {
		if _, ok := newByID[original.ID]; !ok {
			return rej("original task %s cannot be removed", original.ID)
		}
	}
	var seed []string
	var added []model.Task
	removed := false
	for id := range s.Tasks {
		if _, ok := newByID[id]; !ok {
			if parent := s.Tasks[s.Tasks[id].Spec.Parent]; parent != nil && parent.Status == "verified" {
				return rej("cannot change children of verified owner %s", parent.Spec.ID)
			}
			seed, removed = append(seed, id), true
		}
	}
	for _, t := range p.Tasks {
		row := s.Tasks[t.ID]
		if row == nil {
			if retired[t.ID] {
				return rej("task %s was retired and cannot be revived", t.ID)
			}
			added = append(added, t)
			continue
		}
		if taskJSON(row.Spec) != taskJSON(t) {
			res.changed[t.ID] = true
			seed = append(seed, t.ID)
			if err := checkpoint.CapabilityError(s, row.Spec, t); err != nil {
				return rej("%s: %v", t.ID, err)
			}
		}
	}
	res.affected = affectedClosure(s, seed)
	affected := map[string]bool{}
	for _, id := range res.affected {
		affected[id] = true
	}
	// Owners of changed or new nodes must be committed, unaffected expansions.
	check := append([]model.Task{}, added...)
	for _, t := range p.Tasks {
		if res.changed[t.ID] {
			check = append(check, t)
		}
	}
	for _, t := range check {
		var owner *model.TaskState
		if t.Parent != "" {
			if owner = s.Tasks[t.Parent]; owner == nil || affected[t.Parent] || owner.ExpansionHash == "" {
				return rej("%s: owner %s is not a committed, unaffected expansion", t.ID, t.Parent)
			}
			if s.Tasks[t.ID] == nil && owner.Status == "verified" {
				return rej("cannot change children of verified owner %s", owner.Spec.ID)
			}
		}
		if s.Tasks[t.ID] == nil {
			if err := helperError(s, t, owner); err != nil {
				return rej("%s: %v", t.ID, err)
			}
		}
	}
	for _, id := range res.affected {
		row := s.Tasks[id]
		switch row.Status {
		case "verified":
			return rej("affected node %s is already verified", id)
		case "uncertain":
			return rej("affected node %s has an uncertain outcome", id)
		case "running", "produced":
			res.Kind, res.reason = "defer", fmt.Sprintf("affected node %s is %s", id, row.Status)
			res.decision = model.AdaptiveDecision{ProposalID: p.ID, BaseRevision: p.BaseRevision, Time: now, Action: "defer", Reason: res.reason}
			return res
		case "pending", "failed", "cancelled":
		default:
			return rej("affected node %s is not at a replaceable boundary", id)
		}
		cell := filepath.Join(root, "cells", id)
		for a := 0; a <= row.Attempt; a++ {
			if bad, err := launch.UnresolvedProcess(cell, a); bad || err != nil {
				return rej("affected node %s has an unresolved provider process", id)
			}
		}
	}
	if len(res.affected) == 0 && len(added) == 0 && !removed {
		res.Kind, res.reason = "noop", "no-op revision: graph unchanged"
		res.decision = model.AdaptiveDecision{ProposalID: p.ID, BaseRevision: p.BaseRevision, Time: now, Action: "apply",
			Reason: res.reason, AppliedRevision: s.GraphRevision}
		return res
	}
	d := adaptiveDecision(s, p, now)
	d.ProposalID, d.BaseRevision = p.ID, p.BaseRevision
	if d.Time.IsZero() {
		d.Time = now
	}
	res.decision = d
	switch d.Action {
	case "apply":
	case "defer":
		res.Kind, res.reason = "defer", d.Reason
		return res
	default:
		res.Kind, res.reason = "reject", d.Reason
		res.decision.Action = "reject"
		return res
	}
	for _, id := range res.affected {
		if st := s.Tasks[id].Status; st == "running" || st == "produced" {
			res.Kind, res.reason = "defer", fmt.Sprintf("affected node %s is %s", id, st)
			res.decision.Action, res.decision.Reason = "defer", res.reason
			return res
		}
	}
	res.Kind = "ready"
	res.specs = p.Tasks
	return res
}

// commitRevision mutates c, a private clone, so a failed validation leaves the
// real checkpoint untouched.
func commitRevision(c *model.State, p model.RevisionProposal, res *evalResult, now time.Time) error {
	before := checkpoint.HashTasks(c.AllTasks())
	archived := map[string]bool{}
	for _, a := range c.ArchivedTasks {
		archived[a.OperationID] = true
	}
	var retiredOps []string
	old := map[string]*model.TaskState{}
	for _, id := range res.affected {
		row := c.Tasks[id]
		old[id] = row
		if !archived[row.OperationID] {
			c.ArchivedTasks = append(c.ArchivedTasks, *row)
		}
		retiredOps = append(retiredOps, row.OperationID)
		delete(c.Tasks, id)
	}
	active := make([]model.Task, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		if row := c.Tasks[t.ID]; row != nil {
			t = row.Spec
		} else {
			nr := c.NewTask(t)
			if prior := old[t.ID]; prior != nil {
				nr.Generation = prior.Generation + 1
				nr.OperationID = model.OperationID(c.RunID, t.ID, nr.Generation)
				nr.Attempt = prior.Attempt
			}
			c.Tasks[t.ID] = nr
		}
		active = append(active, t)
	}
	prev := map[string][]string{}
	for id, row := range c.Tasks {
		prev[id], row.Children = row.Children, nil
	}
	for id, row := range c.Tasks {
		for _, ch := range prev[id] {
			if r := c.Tasks[ch]; r != nil && r.Spec.Parent == id {
				row.Children = append(row.Children, ch)
			}
		}
	}
	for _, t := range active {
		if t.Parent == "" {
			continue
		}
		if owner := c.Tasks[t.Parent]; owner != nil {
			found := false
			for _, ch := range owner.Children {
				found = found || ch == t.ID
			}
			if !found {
				owner.Children = append(owner.Children, t.ID)
			}
		}
	}
	c.ActiveTasks, c.Dynamic = active, nil
	c.GraphRevision++
	d := res.decision
	d.AppliedRevision = c.GraphRevision
	c.Revisions = append(c.Revisions, model.GraphRevisionRecord{
		Revision: c.GraphRevision, ParentRevision: c.GraphRevision - 1, ProposalID: p.ID, At: now,
		BeforeHash: before, AfterHash: checkpoint.HashTasks(active), TaskCount: len(active), RetiredOperations: retiredOps,
		PredictedGainMS: d.NetBenefitMS, CostAtCommitUSD: model.AdaptiveTotalCost(c),
	})
	AddDecision(c, d)
	c.Event("revision_applied", "", map[string]any{"proposal_id": p.ID, "revision": c.GraphRevision, "retired_operations": retiredOps})
	if len(c.Tasks) != len(c.AllTasks()) {
		return errors.New("task set mismatch after revision")
	}
	for _, spec := range c.AllTasks() {
		if row := c.Tasks[spec.ID]; row == nil || !reflect.DeepEqual(row.Spec, spec) {
			return errors.New("task specification mismatch after revision")
		}
	}
	if err := checkpoint.ValidateOwnership(c); err != nil {
		return err
	}
	return checkpoint.ValidateRevisionState(c)
}

func deferNoSpam(s *model.State, d model.AdaptiveDecision) bool {
	for i := len(s.Decisions) - 1; i >= 0; i-- {
		x := s.Decisions[i]
		if x.ProposalID != d.ProposalID {
			continue
		}
		return x.Action == "defer" && x.BaseRevision == d.BaseRevision &&
			digitsRe.ReplaceAllString(x.Reason, "#") == digitsRe.ReplaceAllString(d.Reason, "#")
	}
	return false
}

func lockAffected(root string, ids []string) ([]*os.File, bool, error) {
	var files []*os.File
	release := func() {
		for _, f := range files {
			checkpoint.Unlock(f)
		}
	}
	for _, id := range ids {
		cell := filepath.Join(root, "cells", id)
		if err := os.MkdirAll(cell, 0700); err != nil {
			release()
			return nil, false, err
		}
		f, err := checkpoint.FileLock(filepath.Join(cell, "lease"), true)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			release()
			return nil, false, nil
		}
		if err != nil {
			release()
			return nil, false, err
		}
		files = append(files, f)
	}
	return files, true, nil
}

func ProcessRevisionProposals(root string) error {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	if s.Control != "run" {
		return nil
	}
	var ids []string
	for _, p := range s.Proposals {
		if TerminalDecision(s, p.ID) == nil {
			ids = append(ids, p.ID)
		}
	}
	for _, id := range ids {
		if err := processOne(root, id); err != nil {
			return err
		}
	}
	return nil
}

func processOne(root, id string) error {
	for try := 0; try < 4; try++ {
		s, err := checkpoint.ReadState(root)
		if err != nil {
			return err
		}
		var p *model.RevisionProposal
		for i := range s.Proposals {
			if s.Proposals[i].ID == id && TerminalDecision(s, id) == nil {
				p = &s.Proposals[i]
			}
		}
		if p == nil || s.Control != "run" {
			return nil
		}
		now := time.Now().UTC()
		res := EvaluateRevision(root, s, *p, now)
		var files []*os.File
		leased := map[string]bool{}
		force := ""
		if res.Kind == "ready" {
			// Empty directories are harmless after an interrupted transaction;
			// they become runnable only with the committed active snapshot.
			for _, task := range p.Tasks {
				if s.Tasks[task.ID] == nil {
					if err := os.MkdirAll(filepath.Join(root, "cells", task.ID), 0700); err != nil {
						return err
					}
				}
			}
			// Leases are only ever tried non-blocking, and never under the store lock.
			var ok bool
			if files, ok, err = lockAffected(root, res.affected); err != nil {
				return err
			}
			if !ok {
				force = "affected task has a live worker lease"
			}
			for _, a := range res.affected {
				leased[a] = true
			}
		}
		err = checkpoint.Update(root, func(cur *model.State) error {
			if cur.Control != "run" || TerminalDecision(cur, id) != nil {
				return errRevisionSkip
			}
			var pp *model.RevisionProposal
			for i := range cur.Proposals {
				if cur.Proposals[i].ID == id {
					pp = &cur.Proposals[i]
				}
			}
			if pp == nil {
				return errRevisionSkip
			}
			prop := *pp
			r := EvaluateRevision(root, cur, prop, time.Now().UTC())
			if r.Kind == "ready" {
				if force != "" {
					r.Kind, r.reason = "defer", force
					r.decision.Action, r.decision.Reason = "defer", force
				} else {
					for _, a := range r.affected {
						if !leased[a] {
							return errRevisionRetry
						}
					}
				}
			}
			switch r.Kind {
			case "ready":
				c, err := CloneState(cur)
				if err != nil {
					return err
				}
				if err := commitRevision(c, prop, r, r.decision.Time); err != nil {
					r = &evalResult{Kind: "reject", decision: model.AdaptiveDecision{ProposalID: id, BaseRevision: prop.BaseRevision,
						Time: r.decision.Time, Action: "reject", Reason: "revision failed validation: " + err.Error()}}
					AddDecision(cur, r.decision)
					return nil
				}
				*cur = *c
			case "defer":
				if deferNoSpam(cur, r.decision) {
					return errRevisionSkip
				}
				AddDecision(cur, r.decision)
			default: // reject, noop
				AddDecision(cur, r.decision)
			}
			return nil
		})
		for _, f := range files {
			checkpoint.Unlock(f)
		}
		if errors.Is(err, errRevisionSkip) {
			return nil
		}
		if errors.Is(err, errRevisionRetry) {
			continue
		}
		return err
	}
	return nil
}
