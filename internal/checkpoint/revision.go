package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

const (
	MaxProposals        = 64
	MaxDecisions        = 256
	MaxRevisions        = 256
	MaxRevTasks         = 1024
	MaxProposalReceipts = 4096
)

// Old checkpoints may retain a decision but no longer its original payload.
// Reserve that ID permanently rather than guessing whether a retry is identical.
const legacyProposalReceipt = "legacy:payload-unavailable"

var (
	ProposalIDRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)
)

func FiniteNonNeg(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func HashTasks(ts []model.Task) string {
	b, _ := json.Marshal(ts)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func ProposalDigest(p model.RevisionProposal) string {
	p.SubmittedAt = time.Time{}
	b, _ := json.Marshal(p)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func backfillProposalReceipts(s *model.State) error {
	if s.ProposalReceipts == nil && len(s.Proposals) == 0 && len(s.Decisions) == 0 && len(s.Revisions) == 0 {
		return nil
	}
	if s.ProposalReceipts == nil {
		s.ProposalReceipts = map[string]string{}
	}
	reserve := func(id, digest string) error {
		old, exists := s.ProposalReceipts[id]
		if exists && old != legacyProposalReceipt {
			return nil
		}
		if !exists && len(s.ProposalReceipts) >= MaxProposalReceipts {
			return errors.New("proposal receipt capacity reached; use a new run instead of reusing proposal IDs")
		}
		s.ProposalReceipts[id] = digest
		return nil
	}
	for _, p := range s.Proposals {
		if err := reserve(p.ID, ProposalDigest(p)); err != nil {
			return err
		}
	}
	for _, d := range s.Decisions {
		if err := reserve(d.ProposalID, legacyProposalReceipt); err != nil {
			return err
		}
	}
	for _, r := range s.Revisions {
		if err := reserve(r.ProposalID, legacyProposalReceipt); err != nil {
			return err
		}
	}
	return nil
}

func EffortRank(e string) int {
	switch e {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	}
	return 0
}

func Subset(a, b []string) bool {
	have := map[string]bool{}
	for _, x := range b {
		have[x] = true
	}
	for _, x := range a {
		if !have[x] {
			return false
		}
	}
	return true
}

// capabilityError enforces non-weakening and non-expansion for an existing node.
func CapabilityError(s *model.State, old, nw model.Task) error {
	if ManagedRun(s) && model.ManagedProfile(s.Plan, old) != model.ManagedProfile(s.Plan, nw) {
		return errors.New("managed profile cannot change")
	}
	if old.Parent != nw.Parent {
		return errors.New("existing node ownership cannot change")
	}
	for field, path := range old.Outputs {
		if nw.Outputs[field] != path {
			return errors.New("existing output mappings cannot be removed or changed")
		}
	}
	if old.Spawn != nil && nw.Spawn == nil {
		return errors.New("existing spawn contract cannot be removed")
	}
	if ProviderName(s, old) != ProviderName(s, nw) {
		return errors.New("provider capability cannot change")
	}
	if EffortRank(nw.Effort) > EffortRank(old.Effort) {
		return errors.New("model effort cannot increase")
	}
	if !Subset(nw.Context, old.Context) {
		return errors.New("context cannot expand")
	}
	if !Subset(old.FileFields, nw.FileFields) {
		return errors.New("file fields cannot be weakened")
	}
	if !Subset(old.Requires, nw.Requires) {
		return errors.New("required fields cannot be weakened")
	}
	for k, v := range old.Equals {
		if got, ok := nw.Equals[k]; !ok || got != v {
			return errors.New("equality checks cannot be weakened")
		}
	}
	have := map[string]bool{}
	for _, c := range nw.Checks {
		b, _ := json.Marshal(c)
		have[string(b)] = true
	}
	for _, c := range old.Checks {
		b, _ := json.Marshal(c)
		if !have[string(b)] {
			return errors.New("validators cannot be removed or weakened")
		}
		delete(have, string(b))
	}
	for _, c := range nw.Checks {
		b, _ := json.Marshal(c)
		if c.Kind == "command" && have[string(b)] {
			return errors.New("cannot introduce executable validators")
		}
	}
	if nw.Spawn != nil {
		if old.Spawn == nil || nw.Spawn.Field != old.Spawn.Field {
			return errors.New("spawn capability cannot expand")
		}
		oldLimit, newLimit := old.Spawn.MaxChildren, nw.Spawn.MaxChildren
		if oldLimit == 0 {
			oldLimit = 8
		}
		if newLimit == 0 {
			newLimit = 8
		}
		if newLimit > oldLimit {
			return errors.New("spawn capability cannot expand")
		}
	}
	return nil
}

func ValidateRevisionState(s *model.State) error {
	if len(s.ProposalReceipts) > MaxProposalReceipts {
		return errors.New("proposal receipt capacity exceeded")
	}
	for id, digest := range s.ProposalReceipts {
		if !ProposalIDRe.MatchString(id) {
			return errors.New("invalid proposal receipt ID")
		}
		if digest != legacyProposalReceipt {
			if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
				return errors.New("invalid proposal receipt digest")
			}
		}
	}
	if s.ProposalReceipts != nil {
		for _, p := range s.Proposals {
			if s.ProposalReceipts[p.ID] != ProposalDigest(p) {
				return errors.New("proposal payload differs from durable receipt")
			}
		}
	}
	if s.Policy != nil {
		if err := ValidateAdaptivePolicy(*s.Policy); err != nil {
			return fmt.Errorf("invalid adaptive policy: %w", err)
		}
	}
	if len(s.Tasks) != len(s.AllTasks()) {
		return errors.New("active task set differs from snapshot")
	}
	for _, task := range s.AllTasks() {
		if row := s.Tasks[task.ID]; row == nil || !reflect.DeepEqual(row.Spec, task) {
			return errors.New("active task specification differs from snapshot")
		}
	}
	if len(s.Proposals) > MaxProposals || len(s.Decisions) > MaxDecisions || len(s.Revisions) > MaxRevisions {
		return errors.New("revision history exceeds bounds")
	}
	seen := map[string]bool{}
	for _, p := range s.Proposals {
		if !ProposalIDRe.MatchString(p.ID) || seen[p.ID] {
			return errors.New("invalid or duplicate revision proposal")
		}
		seen[p.ID] = true
		if !FiniteNonNeg(p.ExpectedSavingsMS) || !FiniteNonNeg(p.SwitchCostMS) || !FiniteNonNeg(p.ExtraCostUSD) || !FiniteNonNeg(p.Confidence) {
			return errors.New("revision proposal has non-finite estimates")
		}
	}
	for _, d := range s.Decisions {
		if d.Action != "apply" && d.Action != "reject" && d.Action != "defer" {
			return fmt.Errorf("invalid decision action %q", d.Action)
		}
	}
	if len(s.Revisions) == 0 {
		if s.GraphRevision != 0 || s.ActiveTasks != nil || len(s.ArchivedTasks) != 0 {
			return errors.New("graph revision has no history")
		}
	} else {
		if s.Revisions[0].Revision != 1 || s.Revisions[0].ParentRevision != 0 {
			return errors.New("revision chain must start at revision one")
		}
		for i, r := range s.Revisions {
			if r.TaskCount < 0 || r.TaskCount > MaxRevTasks {
				return errors.New("revision snapshot task count is invalid")
			}
			if r.ParentRevision != r.Revision-1 || (i > 0 && r.Revision != s.Revisions[i-1].Revision+1) {
				return errors.New("revision chain is not contiguous")
			}
		}
		if s.Revisions[len(s.Revisions)-1].Revision != s.GraphRevision {
			return errors.New("graph revision differs from revision chain")
		}
		if s.ActiveTasks == nil {
			return errors.New("revised graph lacks active snapshot")
		}
		// Expansion appends children; the committed prefix remains immutable.
		// Zero is supported only for checkpoints predating this snapshot pin.
		latest := s.Revisions[len(s.Revisions)-1]
		if latest.TaskCount > 0 && (latest.TaskCount > len(s.ActiveTasks) || HashTasks(s.ActiveTasks[:latest.TaskCount]) != latest.AfterHash) {
			return errors.New("active task snapshot differs from committed revision hash")
		}
		plan := s.Plan
		plan.Tasks = s.ActiveTasks
		if err := Validate(plan); err != nil {
			return fmt.Errorf("invalid active graph: %w", err)
		}
		for _, original := range s.Plan.Tasks {
			row := s.Tasks[original.ID]
			if row == nil {
				return fmt.Errorf("original task %s is missing", original.ID)
			}
			if err := CapabilityError(s, original, row.Spec); err != nil {
				return fmt.Errorf("original task %s: %w", original.ID, err)
			}
		}
	}
	retired := map[string]bool{}
	for _, r := range s.Revisions {
		for _, op := range r.RetiredOperations {
			retired[op] = true
		}
	}
	ops := map[string]bool{}
	for id, row := range s.Tasks {
		if row == nil {
			return errors.New("nil graph node")
		}
		if row.OperationID != "" {
			if ops[row.OperationID] || retired[row.OperationID] {
				return fmt.Errorf("operation of %s is duplicated or retired", id)
			}
			ops[row.OperationID] = true
		}
	}
	archived := map[string]bool{}
	for _, a := range s.ArchivedTasks {
		if archived[a.OperationID] || ops[a.OperationID] || !retired[a.OperationID] {
			return errors.New("archived operation is duplicated, live, or not retired")
		}
		archived[a.OperationID] = true
	}
	return nil
}
