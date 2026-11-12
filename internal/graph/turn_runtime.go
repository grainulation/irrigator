package graph

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

func turnExpansion(s *model.State, row *model.TaskState) ([]model.Task, string, error) {
	source := row.Result[artifactstore.TurnRequestField]
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
	if row.ExpansionHash != "" {
		if row.ExpansionHash != hash {
			return nil, "", errors.New("turn request changed after commit")
		}
		return nil, hash, nil
	}
	var request checkpoint.TurnRequest
	if strings.TrimSpace(source) != "" {
		if len(source) > 1024*1024 {
			return nil, "", errors.New("turn request exceeds 1 MiB")
		}
		strict := json.NewDecoder(strings.NewReader(source))
		value, err := jsonio.ParseStrictJSON(strict, 0)
		if err != nil {
			return nil, "", fmt.Errorf("invalid turn request: %w", err)
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, "", errors.New("turn request must be an object")
		}
		if err := model.ExactTurnJSONFields(value, reflect.TypeOf(checkpoint.TurnRequest{})); err != nil {
			return nil, "", err
		}
		if err := strict.Decode(new(any)); err != io.EOF {
			return nil, "", errors.New("turn request must be one object")
		}
		d := json.NewDecoder(strings.NewReader(source))
		d.DisallowUnknownFields()
		if err := d.Decode(&request); err != nil {
			return nil, "", fmt.Errorf("invalid turn request: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return nil, "", errors.New("turn request must be one object")
		}
	}
	identity := row.Spec.Turn
	if identity.Role == "helper" {
		if request.Continue != nil || len(request.Helpers) > 0 || request.WaitForHelpers {
			return nil, "", errors.New("helpers cannot continue or spawn")
		}
		return nil, hash, nil
	}
	if len(row.Children)+len(request.Helpers) > s.Plan.TurnShape.HelpersPerTurn {
		return nil, "", errors.New("per-turn helper allowance exceeded")
	}
	if request.Continue != nil && identity.Index >= s.Plan.TurnShape.MainTurns {
		return nil, "", errors.New("main turn limit reached")
	}
	if request.WaitForHelpers && request.Continue == nil {
		return nil, "", errors.New("wait_for_helpers requires continuation")
	}
	children := []model.Task{}
	helperIDs := append([]string{}, row.Children...)
	localIDs := map[string]bool{}
	safe := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	prepare := func(t model.Task, id string) (model.Task, error) {
		if t.Turn != nil || t.Parent != "" || t.Spawn != nil || len(t.DependsOn) > 0 || len(t.InputFields) > 0 {
			return t, errors.New("generated tasks cannot set turn identity, parent, spawn or dependencies")
		}
		if checkpoint.ManagedRun(s) {
			profile := model.ManagedProfile(s.Plan, row.Spec)
			if (t.Profile != "" && t.Profile != profile) || t.Provider != "" || t.Effort != "" {
				return t, errors.New("managed child must retain owner profile")
			}
			t.Profile = profile
		} else {
			effective := checkpoint.ProviderName(s, row.Spec)
			if t.Provider != "" && t.Provider != effective {
				return t, errors.New("generated task cannot change provider")
			}
			t.Provider = effective
			effort, err := childEffort(row.Spec.Effort, t.Effort)
			if err != nil {
				return t, err
			}
			t.Effort = effort
		}
		for _, c := range t.Context {
			if !artifactstore.TurnContains(row.Spec.Context, c) {
				return t, errors.New("generated task context not granted")
			}
		}
		if len(t.Context) == 0 {
			t.Context = append([]string{}, row.Spec.Context...)
		}
		for _, c := range t.Checks {
			if c.Kind == "command" {
				return t, errors.New("generated task cannot introduce executable validator")
			}
		}
		for field, path := range t.Outputs {
			if !filepath.IsLocal(path) || path == "." {
				return t, errors.New("unsafe generated output")
			}
			t.Outputs[field] = filepath.Join(id, path)
		}
		t.ID = id
		t.DependsOn = []string{row.Spec.ID}
		return t, nil
	}
	for _, proposal := range request.Helpers {
		if !safe.MatchString(proposal.ID) || localIDs[proposal.ID] {
			return nil, "", errors.New("helper local IDs must be valid and unique")
		}
		localIDs[proposal.ID] = true
		id := fmt.Sprintf("%s__turn%d__helper_%s", identity.AgentID, identity.Index, proposal.ID)
		t, err := prepare(proposal, id)
		if err != nil {
			return nil, "", err
		}
		t.Parent = row.Spec.ID
		t.Turn = &model.TurnIdentity{AgentID: id, Role: "helper", Index: 1, OwnerAgentID: identity.AgentID, OwnerTurn: identity.Index}
		children = append(children, t)
		helperIDs = append(helperIDs, id)
	}
	if request.Continue != nil {
		id := fmt.Sprintf("%s__turn%d", identity.AgentID, identity.Index+1)
		t, err := prepare(*request.Continue, id)
		if err != nil {
			return nil, "", err
		}
		t.Turn = &model.TurnIdentity{AgentID: identity.AgentID, Role: "main", Index: identity.Index + 1, PreviousTask: row.Spec.ID}
		if request.WaitForHelpers {
			t.DependsOn = append(t.DependsOn, helperIDs...)
		}
		children = append(children, t)
	}
	p := s.Plan
	p.Tasks = append(s.AllTasks(), children...)
	if err := checkpoint.Validate(p); err != nil {
		return nil, "", err
	}
	return children, hash, nil
}

// The acceptance path supplies its original validation fence; a nil fence
// captures one here. Both paths check it again inside the atomic graph commit.
func CommitTurnExpansionFenced(root, id, level string, expected *managed.CellFence) error {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	if s.Tasks[id] == nil {
		return errors.New("unknown expansion cell")
	}
	fence := FenceForCell(s, s.Tasks[id])
	if expected != nil {
		if _, err := expected.Current(s); err != nil {
			return err
		}
		fence = *expected
	}
	row := s.Tasks[id]
	if row == nil {
		return errors.New("unknown turn")
	}
	op := row.OperationID
	tasks, hash, err := turnExpansion(s, row)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if err := os.MkdirAll(filepath.Join(root, "cells", t.ID), 0700); err != nil {
			return err
		}
	}
	return checkpoint.Update(root, func(s *model.State) error {
		if _, err := fence.Current(s); err != nil {
			return err
		}
		row := s.Tasks[id]
		if row == nil || row.OperationID != op {
			return errors.New("stale turn operation")
		}
		if row.Status == "produced" && (row.CancelRequested || s.Control == "down") {
			return errors.New("turn admission cancelled")
		}
		if row.ExpansionHash != "" {
			if row.ExpansionHash != hash {
				return errors.New("turn request changed")
			}
			return nil
		}
		if row.Status != "produced" {
			return errors.New("stale turn admission")
		}
		if row.CancelRequested || s.Control == "down" {
			return errors.New("turn admission cancelled")
		}
		tasks, current, err := turnExpansion(s, row)
		if err != nil {
			return err
		}
		if current != hash {
			return errors.New("turn request changed during commit")
		}
		for _, t := range tasks {
			if s.Tasks[t.ID] != nil {
				return errors.New("duplicate turn admission")
			}
		}
		row.ExpansionHash = hash
		row.Status = "verified"
		row.Validation = level
		row.Error = ""
		for _, t := range tasks {
			if s.ActiveTasks != nil {
				s.ActiveTasks = append(s.ActiveTasks, t)
			} else {
				s.Dynamic = append(s.Dynamic, t)
			}
			s.Tasks[t.ID] = s.NewTask(t)
			if t.Parent == id {
				row.Children = append(row.Children, t.ID)
			}
		}
		s.Event("turn_verified", id, map[string]any{"agent_id": row.Spec.Turn.AgentID, "turn_index": row.Spec.Turn.Index, "helpers": row.Children, "admitted": len(tasks), "operation_id": op})
		return nil
	})
}
