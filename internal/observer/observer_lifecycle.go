package observer

import (
	"github.com/grainulation/irrigator/internal/model"
)

const

// Authoritative lifecycle projection from the full State. Read-only and bounded.
// dependency_gate=satisfied means ONLY that every dependency is verified: it is
// not scheduler admission (admission is always "not_evaluated"). No lease or
// busy state is inferred. Generation 0 means unknown.

(
	lifecycleAdmission = "not_evaluated"
)

type PanelLifecycle struct {
	OperationID                 string   `json:"operation_id,omitempty"`
	Generation                  int      `json:"generation"`
	ExecutionStatus             string   `json:"execution_status"`
	ValidationLevel             string   `json:"validation_level"`
	DependencyGate              string   `json:"dependency_gate"`
	BlockingDependencyIDs       []string `json:"blocking_dependency_ids"`
	BlockingDependenciesTotal   int      `json:"blocking_dependencies_total"`
	BlockingDependenciesOmitted int      `json:"blocking_dependencies_omitted"`
	MissingDependenciesTotal    int      `json:"missing_dependencies_total"`
	Join                        string   `json:"join"`
	UnfinishedChildIDs          []string `json:"unfinished_child_ids"`
	UnfinishedChildrenTotal     int      `json:"unfinished_children_total"`
	UnfinishedChildrenOmitted   int      `json:"unfinished_children_omitted"`
	CancelRequested             bool     `json:"cancel_requested"`
	Admission                   string   `json:"admission"`
	// Recovery is additive and present only on waiting owners with terminal descendants.
	Recovery *model.LifecycleRecovery `json:"recovery,omitempty"`
}

func unknownLifecycle() *PanelLifecycle {
	return &PanelLifecycle{ExecutionStatus: "unknown", ValidationLevel: "unknown", DependencyGate: "unknown", Join: "unknown",
		Admission: lifecycleAdmission, BlockingDependencyIDs: []string{}, UnfinishedChildIDs: []string{}}
}

func lifecycleValidation(row *model.TaskState) string {
	switch row.Validation {
	case "structural", "task-checks":
		return row.Validation
	case "":
		if row.Status != "verified" && row.Status != "waiting" {
			return "none"
		}
	}
	return "unknown"
}

func lifecycleFor(s *model.State, t model.Task, row *model.TaskState) *PanelLifecycle {
	lc := unknownLifecycle()
	lc.OperationID, lc.Generation, lc.CancelRequested = row.OperationID, row.Generation, row.CancelRequested
	lc.ValidationLevel = lifecycleValidation(row)
	if ObsKnownStatus[row.Status] {
		lc.ExecutionStatus = row.Status
	}
	seen := map[string]bool{}
	blocking, missing := []string{}, 0
	for _, d := range t.DependsOn {
		if seen[d] {
			continue
		}
		seen[d] = true
		dr := s.Tasks[d]
		if dr == nil {
			missing++
		} else if dr.Status != "verified" {
			blocking = append(blocking, d)
		}
	}
	switch {
	case missing > 0:
		lc.DependencyGate = "unknown"
	case len(blocking) > 0:
		lc.DependencyGate = "blocked"
	default:
		lc.DependencyGate = "satisfied"
	}
	lc.MissingDependenciesTotal = missing
	lc.BlockingDependencyIDs, lc.BlockingDependenciesTotal, lc.BlockingDependenciesOmitted = model.LifecycleCap(blocking)

	seenChild := map[string]bool{}
	unfinished, unknownChildren, children := []string{}, 0, 0
	for _, c := range row.Children {
		if seenChild[c] {
			continue
		}
		seenChild[c] = true
		children++
		cr := s.Tasks[c]
		if cr == nil {
			unknownChildren++
		} else if cr.Status != "verified" {
			unfinished = append(unfinished, c)
		}
	}
	switch {
	case children == 0:
		lc.Join = "not_applicable"
	case unknownChildren > 0:
		lc.Join = "unknown"
	case len(unfinished) > 0:
		lc.Join = "waiting_children"
	default:
		lc.Join = "satisfied"
	}
	lc.UnfinishedChildIDs, lc.UnfinishedChildrenTotal, lc.UnfinishedChildrenOmitted = model.LifecycleCap(unfinished)
	lc.Recovery = model.LifecycleRecoveryFor(s, row)
	return lc
}

// projectLifecycles returns one entry per ACTIVE task. A task without a state
// row fails closed to all-unknown.
func ProjectLifecycles(s *model.State) map[string]*PanelLifecycle {
	out := map[string]*PanelLifecycle{}
	if s == nil {
		return out
	}
	for _, t := range s.AllTasks() {
		row := s.Tasks[t.ID]
		if row == nil {
			out[t.ID] = unknownLifecycle()
			continue
		}
		out[t.ID] = lifecycleFor(s, t, row)
	}
	return out
}
