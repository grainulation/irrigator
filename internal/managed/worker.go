package managed

import (
	"errors"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// inputsForTask includes accepted planner artifacts without requiring an owning
// graph to finish: waiting parents have passed their artifact and expansion
// checks, but are not yet verified as a completed graph. Ownership is an input
// relationship, not a dependency on parent completion.
func InputsForTask(s *model.State, task model.Task) (map[string]map[string]string, error) {
	inputs := map[string]map[string]string{}
	for _, d := range task.DependsOn {
		row := s.Tasks[d]
		if row == nil || row.Status != "verified" {
			return nil, launch.ErrNoWork
		}
		if err := artifactstore.AcceptArtifactContracts(row.Spec, row.Result); err != nil {
			return nil, err
		}
		inputs[d] = row.Result
		for _, child := range Descendants(s, d)[1:] {
			if checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
				continue
			} // Helpers require an explicit verified dependency.
			if err := artifactstore.AcceptArtifactContracts(s.Tasks[child].Spec, s.Tasks[child].Result); err != nil {
				return nil, err
			}
			inputs[child] = s.Tasks[child].Result
		}
	}
	// A mid-call helper receives only the already accepted dependency snapshot;
	// its unfinished owner contributes no candidate/result/context implicitly.
	if task.Turn != nil && task.Turn.Async != nil {
		owner := s.Tasks[task.Parent]
		if owner == nil || owner.CancelRequested || !checkpoint.HelperAdmissionOwnerMatches(s, owner, task.Turn.Async) {
			return nil, launch.ErrNoWork
		}
		return launch.SelectInputFields(task, inputs)
	}
	seen := map[string]bool{task.ID: true}
	for parent := task.Parent; parent != ""; {
		row := s.Tasks[parent]
		if row == nil || seen[parent] {
			return nil, errors.New("invalid parent ownership chain")
		}
		seen[parent] = true
		if row.CancelRequested || row.ExpansionHash == "" || (row.Status != "waiting" && row.Status != "verified") {
			return nil, launch.ErrNoWork
		}
		if err := artifactstore.AcceptArtifactContracts(row.Spec, row.Result); err != nil {
			return nil, err
		}
		inputs[parent] = row.Result
		parent = row.Spec.Parent
	}
	return launch.SelectInputFields(task, inputs)
}
