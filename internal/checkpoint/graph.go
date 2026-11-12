package checkpoint

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/grainulation/irrigator/internal/model"
)

func ProviderName(s *model.State, t model.Task) string {
	if t.Provider != "" {
		return t.Provider
	}
	return s.Backend
}

// validateOwnership rejects malformed persisted ownership before traversals run.
func ValidateOwnership(s *model.State) error {
	if HasTurnSemantics(s.Plan.APIVersion) {
		return ValidateTurnState(s)
	}
	roots := map[string]bool{}
	rootTasks, dynamicTasks := s.Plan.Tasks, s.Dynamic
	if s.ActiveTasks != nil {
		rootTasks, dynamicTasks = nil, nil
		for _, t := range s.ActiveTasks {
			if t.Parent == "" {
				rootTasks = append(rootTasks, t)
			} else {
				dynamicTasks = append(dynamicTasks, t)
			}
		}
	}
	for _, t := range rootTasks {
		if roots[t.ID] {
			return errors.New("duplicate root")
		}
		roots[t.ID] = true
	}
	dynamic := map[string]bool{}
	for _, t := range dynamicTasks {
		if roots[t.ID] || dynamic[t.ID] {
			return errors.New("duplicate dynamic node")
		}
		dynamic[t.ID] = true
	}
	for id, row := range s.Tasks {
		if row == nil {
			return errors.New("nil graph node")
		}
		if roots[id] && row.Spec.Parent != "" {
			return errors.New("plan roots cannot have owners")
		}
		if !roots[id] && row.Spec.Parent == "" {
			return errors.New("dynamic node lacks owner")
		}
		seen := map[string]bool{}
		for _, child := range row.Children {
			if seen[child] || s.Tasks[child] == nil || s.Tasks[child].Spec.Parent != id {
				return errors.New("invalid child ownership reference")
			}
			seen[child] = true
		}
		if row.Spec.Parent != "" {
			parent := s.Tasks[row.Spec.Parent]
			if parent == nil {
				return errors.New("missing owner")
			}
			count := 0
			for _, child := range parent.Children {
				if child == id {
					count++
				}
			}
			if count != 1 {
				return errors.New("owner does not reference child exactly once")
			}
		}
		if len(row.Children) > 0 && row.ExpansionHash == "" {
			return errors.New("children lack committed expansion")
		}
		if row.Spec.Spawn != nil {
			limit := row.Spec.Spawn.MaxChildren
			if limit == 0 {
				limit = 8
			}
			if len(row.Children) > limit {
				return errors.New("ownership exceeds max_children")
			}
		}
		if row.ExpansionHash != "" {
			if row.Spec.Spawn == nil {
				return errors.New("expansion lacks spawn policy")
			}
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(row.Result[row.Spec.Spawn.Field])))
			if hash != row.ExpansionHash {
				return errors.New("committed expansion source changed")
			}
		}
		chain := map[string]bool{id: true}
		depth := 0
		for p := row.Spec.Parent; p != ""; {
			if chain[p] || s.Tasks[p] == nil {
				return errors.New("invalid ownership cycle")
			}
			chain[p] = true
			depth++
			p = s.Tasks[p].Spec.Parent
		}
		limit := s.Plan.MaxDepth
		if limit == 0 {
			limit = 4
		}
		if depth > limit {
			return errors.New("ownership exceeds max_depth")
		}
	}
	return nil
}
