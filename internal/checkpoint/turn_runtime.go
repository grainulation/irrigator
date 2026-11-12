package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/model"
)

const TurnPlanVersion = "swarm/v2"

type TurnRequest struct {
	Continue       *model.Task  `json:"continue,omitempty"`
	Helpers        []model.Task `json:"helpers,omitempty"`
	WaitForHelpers bool         `json:"wait_for_helpers,omitempty"`
}

func validateTurnPlan(p model.Plan) error {
	if !HasTurnSemantics(p.APIVersion) {
		if p.TurnShape != nil {
			return errors.New("turn_shape requires swarm/v2")
		}
		for _, t := range p.Tasks {
			if t.Turn != nil {
				return errors.New("turn identity requires swarm/v2")
			}
		}
		return nil
	}
	if p.TurnShape == nil {
		return errors.New("swarm/v2 requires turn_shape")
	}
	report, err := model.ReportTurnShape(*p.TurnShape)
	if err != nil {
		return err
	}
	if p.ObserverShape != nil {
		return errors.New("swarm/v2 cannot use legacy observer_shape")
	}
	if p.MaxTasks != report.MaxExecutions {
		return errors.New("swarm/v2 max_tasks must equal turn execution capacity")
	}
	byID := map[string]model.Task{}
	lanes := map[string]map[int]string{}
	roots := 0
	helperCounts := map[string]int{}
	mainAgents := map[string]bool{}
	for _, t := range p.Tasks {
		if t.Turn != nil && t.Turn.Role == "main" {
			mainAgents[t.Turn.AgentID] = true
		}
	}
	safe := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	for _, t := range p.Tasks {
		byID[t.ID] = t
	}
	for _, t := range p.Tasks {
		x := t.Turn
		if x == nil || !safe.MatchString(x.AgentID) || t.Spawn != nil {
			return errors.New("v2 tasks require valid turn identity and no legacy spawn")
		}
		switch x.Role {
		case "main":
			if x.Async != nil || x.Index < 1 || x.Index > p.TurnShape.MainTurns || t.Parent != "" || x.OwnerAgentID != "" || x.OwnerTurn != 0 {
				return errors.New("invalid main turn identity")
			}
			if lanes[x.AgentID] == nil {
				lanes[x.AgentID] = map[int]string{}
			}
			if lanes[x.AgentID][x.Index] != "" {
				return errors.New("duplicate turn in stable agent lane")
			}
			lanes[x.AgentID][x.Index] = t.ID
			if x.Index == 1 {
				roots++
				if x.PreviousTask != "" {
					return errors.New("first turn has predecessor")
				}
			} else {
				prev, ok := byID[x.PreviousTask]
				if !ok || prev.Turn == nil || prev.Turn.Role != "main" || prev.Turn.AgentID != x.AgentID || prev.Turn.Index != x.Index-1 || !artifactstore.TurnContains(t.DependsOn, prev.ID) {
					return errors.New("turn requires exact previous turn dependency")
				}
			}
		case "helper":
			owner, ok := byID[t.Parent]
			if !ok || owner.Turn == nil || owner.Turn.Role != "main" || x.Index != 1 || x.PreviousTask != "" || x.OwnerAgentID != owner.Turn.AgentID || x.OwnerTurn != owner.Turn.Index {
				return errors.New("invalid terminal helper identity")
			}
			if x.Async == nil {
				if !artifactstore.TurnContains(t.DependsOn, owner.ID) {
					return errors.New("boundary helper requires owner dependency")
				}
			} else if !reflect.DeepEqual(t.DependsOn, owner.DependsOn) || !reflect.DeepEqual(t.InputFields, owner.InputFields) {
				return errors.New("asynchronous helper must inherit only the owner's accepted dependencies")
			}
			if mainAgents[x.AgentID] {
				return errors.New("helper identity collides with main agent")
			}
			if x.AgentID != t.ID {
				return errors.New("helper stable identity must equal execution identity")
			}
			helperCounts[t.Parent]++
			if helperCounts[t.Parent] > p.TurnShape.HelpersPerTurn {
				return errors.New("per-turn helper allowance exceeded")
			}
		default:
			return errors.New("unknown turn role")
		}
	}
	if roots != p.TurnShape.Roots || len(lanes) != roots {
		return errors.New("turn root-agent count mismatch")
	}
	for _, lane := range lanes {
		for i := 1; i <= len(lane); i++ {
			if lane[i] == "" {
				return errors.New("turn prefix has a gap")
			}
		}
	}
	return nil
}
func ValidateInitialTurns(p model.Plan) error {
	if !HasTurnSemantics(p.APIVersion) {
		return nil
	}
	for _, t := range p.Tasks {
		if t.Turn == nil || t.Turn.Role != "main" || t.Turn.Index != 1 {
			return errors.New("only first main turns may appear in an initial v2 plan")
		}
		if err := validateKnownTurnWaits(t, p.TurnShape, 1, false, 0); err != nil {
			return fmt.Errorf("task %q: %w", t.ID, err)
		}
	}
	return nil
}

// Only initial admission checks known mock proposals. Existing checkpoints are not
// re-checked; dynamically generated proposals still pass turnExpansion.
func validateKnownTurnWaits(t model.Task, shape *model.TurnShape, index int, helper bool, depth int) error {
	if shape == nil || depth > 64 {
		return nil
	}
	source, present := t.MockResult[artifactstore.TurnRequestField]
	if !present || strings.TrimSpace(source) == "" {
		return nil
	}
	var proposal TurnRequest
	if len(source) > model.MaxOutputBytes || json.Unmarshal([]byte(source), &proposal) != nil {
		return nil
	}
	if proposal.WaitForHelpers && (helper || proposal.Continue == nil || index >= shape.MainTurns) {
		return errors.New("known turn_request wait_for_helpers requires a continuation before the final main turn; final turns and helpers must not wait")
	}
	if proposal.Continue != nil {
		if err := validateKnownTurnWaits(*proposal.Continue, shape, index+1, false, depth+1); err != nil {
			return err
		}
	}
	for _, child := range proposal.Helpers {
		if err := validateKnownTurnWaits(child, shape, 1, true, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func ValidateTurnState(s *model.State) error {
	p := s.Plan
	p.Tasks = s.AllTasks()
	if err := Validate(p); err != nil {
		return err
	}
	for _, t := range p.Tasks {
		r := s.Tasks[t.ID]
		if r == nil {
			return errors.New("missing turn execution")
		}
		if r.Status == "waiting" {
			return errors.New("v2 turn cannot enter legacy child-waiting acceptance")
		}
		if t.Turn.PreviousTask != "" && s.Tasks[t.Turn.PreviousTask].Status != "verified" {
			return errors.New("continuation admitted before predecessor verification")
		}
		if r.ExpansionHash != "" {
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(r.Result[artifactstore.TurnRequestField])))
			if hash != r.ExpansionHash {
				return errors.New("turn request changed after admission")
			}
		}
		for _, id := range r.Children {
			child := s.Tasks[id]
			if child == nil || child.Spec.Parent != t.ID || child.Spec.Turn.Role != "helper" {
				return errors.New("invalid terminal helper ownership")
			}
		}
		if t.Parent != "" {
			owner := s.Tasks[t.Parent]
			count := 0
			for _, id := range owner.Children {
				if id == t.ID {
					count++
				}
			}
			if count != 1 || (t.Turn.Async == nil && owner.ExpansionHash == "") {
				return errors.New("helper missing committed owner admission")
			}
			if t.Turn.Async != nil {
				if err := ValidateHelperAdmission(s, t); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
