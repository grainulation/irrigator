package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/grainulation/irrigator/internal/model"
)

func HelperSpecHash(t model.Task) string {
	x := *t.Turn
	x.Async = nil
	t.Turn = &x
	raw, _ := json.Marshal(t)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// An explicit owner retry creates a new operation, but grants already admitted
// by an earlier operation remain immutable and continue consuming allowance.
// Owner task identity/specification cannot be revised after helpers are admitted.
// Accept only this owner's deterministic lineage, never another owner's grant.
func HelperAdmissionOwnerMatches(s *model.State, owner *model.TaskState, a *model.HelperAdmission) bool {
	if owner == nil || a == nil {
		return false
	}
	if a.OperationID == owner.OperationID {
		return true
	}
	for generation := 1; generation < owner.Generation; generation++ {
		if a.OperationID == model.OperationID(s.RunID, owner.Spec.ID, generation) {
			return true
		}
	}
	return false
}

func ValidateHelperAdmission(s *model.State, t model.Task) error {
	a := t.Turn.Async
	owner := s.Tasks[t.Parent]
	if a == nil || owner == nil || !ProposalIDRe.MatchString(a.ID) || !HelperAdmissionOwnerMatches(s, owner, a) || a.Attempt < 1 || a.Attempt > owner.Attempt || a.Count < 1 || a.Count > s.Plan.TurnShape.HelpersPerTurn || len(a.RequestHash) != 64 || a.SpecHash != HelperSpecHash(t) {
		return errors.New("invalid asynchronous helper admission")
	}
	count := 0
	for _, id := range owner.Children {
		child := s.Tasks[id]
		if child == nil || child.Spec.Turn == nil {
			return errors.New("missing asynchronous helper")
		}
		b := child.Spec.Turn.Async
		if b != nil && b.ID == a.ID {
			if b.RequestHash != a.RequestHash || b.OperationID != a.OperationID || b.Attempt != a.Attempt || b.Count != a.Count {
				return errors.New("conflicting helper admission receipt")
			}
			count++
		}
	}
	if count != a.Count {
		return errors.New("incomplete helper admission batch")
	}
	return nil
}
