package graph

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
)

func ReplaySafe(s *model.State, t model.Task) bool {
	if checkpoint.ManagedRun(s) {
		b, err := checkpoint.ManagedBinding(s, s.Tasks[t.ID])
		return err == nil && b.Adapter.Kind == "mock"
	}
	p := checkpoint.ProviderName(s, t)
	// A stopped paid call can still be billed. Only mock or an explicitly
	// declared named-provider policy permits automatic replay.
	return p == "mock" || (p != "haiku" && s.Plan.Providers[p].Replay == "safe")
}

// Expansion is data, never direct writes by agents. Validate the complete proposed
// graph before atomically registering any child, then use deterministic child IDs.
func Expansion(s *model.State, row *model.TaskState) ([]model.Task, string, error) {
	if row == nil {
		return nil, "", errors.New("unknown expansion node")
	}
	if row.Spec.Spawn == nil {
		return nil, "", nil
	}
	source := row.Result[row.Spec.Spawn.Field]
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
	if row.ExpansionHash != "" {
		if row.ExpansionHash != hash {
			return nil, "", errors.New("expansion changed after commit")
		}
		return nil, hash, nil
	}
	var proposal struct {
		Tasks []model.Task `json:"tasks"`
	}
	// Derived parse only: hash and stored result keep the original source bytes.
	normalized, err := normalizeChildProposal(source)
	if err != nil {
		return nil, "", fmt.Errorf("invalid subgraph: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(normalized))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return nil, "", fmt.Errorf("invalid subgraph: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, "", errors.New("subgraph must contain exactly one JSON object")
	}
	maxChildren := row.Spec.Spawn.MaxChildren
	if maxChildren == 0 {
		maxChildren = 8
	}
	if len(proposal.Tasks) > maxChildren {
		return nil, "", errors.New("subgraph exceeds max_children")
	}
	depth := 0
	seen := map[string]bool{row.Spec.ID: true}
	for p := row.Spec.Parent; p != ""; {
		if seen[p] || s.Tasks[p] == nil {
			return nil, "", errors.New("invalid parent ownership chain")
		}
		seen[p] = true
		depth++
		p = s.Tasks[p].Spec.Parent
	}
	maxDepth := s.Plan.MaxDepth
	if maxDepth == 0 {
		maxDepth = 4
	}
	if len(proposal.Tasks) > 0 && depth+1 > maxDepth {
		return nil, "", errors.New("subgraph exceeds max_depth")
	}
	names := map[string]string{}
	for _, t := range proposal.Tasks {
		if t.ID == "" || strings.Contains(t.ID, "__") || names[t.ID] != "" {
			return nil, "", errors.New("child IDs must be unique local names without __")
		}
		names[t.ID] = row.Spec.ID + "__" + t.ID
	}
	allowedContext := map[string]bool{}
	for _, k := range row.Spec.Context {
		allowedContext[k] = true
	}
	for i := range proposal.Tasks {
		t := &proposal.Tasks[i]
		local := t.ID
		t.ID = names[local]
		t.Parent = row.Spec.ID
		if s.Tasks[t.ID] != nil {
			return nil, "", fmt.Errorf("child %s already exists", t.ID)
		}
		effective := checkpoint.ProviderName(s, row.Spec)
		if t.Provider != "" && t.Provider != effective {
			return nil, "", errors.New("child cannot change provider capability")
		}
		t.Provider = effective
		effort, err := childEffort(row.Spec.Effort, t.Effort)
		if err != nil {
			return nil, "", err
		}
		t.Effort = effort
		for _, k := range t.Context {
			if !allowedContext[k] {
				return nil, "", fmt.Errorf("child context %s not granted to parent", k)
			}
		}
		if len(t.Context) == 0 {
			t.Context = append([]string{}, row.Spec.Context...)
		}
		for _, c := range t.Checks {
			if c.Kind == "command" {
				return nil, "", errors.New("generated children cannot introduce executable validators")
			}
		}
		if t.Spawn != nil {
			if t.Spawn.MaxChildren == 0 {
				t.Spawn.MaxChildren = maxChildren
			}
			if t.Spawn.MaxChildren > maxChildren {
				return nil, "", errors.New("child cannot raise spawn budget")
			}
		}
		for j, d := range t.DependsOn {
			if names[d] == "" {
				return nil, "", fmt.Errorf("unknown sibling dependency %s", d)
			}
			t.DependsOn[j] = names[d]
		}
		if len(t.DependsOn) == 0 {
			t.DependsOn = append([]string{}, row.Spec.DependsOn...)
		}
		if len(t.InputFields) > 0 {
			selected := make(map[string][]string, len(t.InputFields))
			for id, fields := range t.InputFields {
				if names[id] != "" {
					id = names[id]
				}
				if _, exists := selected[id]; exists {
					return nil, "", fmt.Errorf("duplicate input_fields source %s", id)
				}
				selected[id] = fields
			}
			t.InputFields = selected
		}
		for field, path := range t.Outputs {
			if !filepath.IsLocal(path) || path == "." {
				return nil, "", errors.New("unsafe child output path")
			}
			t.Outputs[field] = filepath.Join(row.Spec.ID, local, path)
		}
	}
	if err := checkpoint.ValidateCellNames(append(checkpoint.HistoricalCellTasks(s), proposal.Tasks...)); err != nil {
		return nil, "", err
	}
	combined := s.Plan
	combined.Tasks = append(s.AllTasks(), proposal.Tasks...)
	if err := checkpoint.Validate(combined); err != nil {
		return nil, "", err
	}
	return proposal.Tasks, hash, nil
}

// The acceptance path supplies its original validation fence; a nil fence
// captures one here. Both paths check it again inside the atomic graph commit.
func CommitExpansionFenced(root, id, level string, expected *managed.CellFence) error {
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
	children, hash, err := Expansion(s, s.Tasks[id])
	if err != nil {
		return err
	}
	operation := s.Tasks[id].OperationID
	// A crash here can leave only empty, unreferenced directories, never runnable work.
	for _, t := range children {
		if err := os.MkdirAll(filepath.Join(root, "cells", t.ID), 0700); err != nil {
			return err
		}
	}
	return checkpoint.Update(root, func(s *model.State) error {
		if _, err := fence.Current(s); err != nil {
			return err
		}
		row := s.Tasks[id]
		if row == nil || row.OperationID != operation {
			return errors.New("stale expansion operation")
		}
		// Revalidation of an already expanded producer still obeys cancellation.
		// Never let the idempotent expansion branch re-accept a cancelled attempt.
		if row.Status == "produced" && row.CancelRequested {
			row.Status = "cancelled"
			s.Event("cancelled", id, nil)
			return nil
		}
		if row.ExpansionHash != "" {
			if row.ExpansionHash != hash {
				return errors.New("expansion changed after commit")
			}
			if row.Status == "produced" {
				row.Status = "verified"
				for _, child := range row.Children {
					if s.Tasks[child].Status != "verified" {
						row.Status = "waiting"
					}
				}
			}
			return nil
		}
		if row.Status != "produced" {
			return errors.New("stale expansion commit")
		}
		if row.CancelRequested {
			row.Status = "cancelled"
			s.Event("cancelled", id, nil)
			return nil
		}
		current, currentHash, err := Expansion(s, row)
		if err != nil {
			return err
		}
		if currentHash != hash {
			return errors.New("expansion changed during commit")
		}
		children = current
		for _, t := range children {
			if s.Tasks[t.ID] != nil {
				return errors.New("duplicate child commit")
			}
		}
		row.ExpansionHash = hash
		row.Validation = level
		row.Error = ""
		for _, t := range children {
			if s.ActiveTasks != nil {
				s.ActiveTasks = append(s.ActiveTasks, t)
			} else {
				s.Dynamic = append(s.Dynamic, t)
			}
			s.Tasks[t.ID] = s.NewTask(t)
			row.Children = append(row.Children, t.ID)
		}
		row.Status = "verified"
		if len(children) > 0 {
			row.Status = "waiting"
		}
		s.Event("expanded", id, map[string]any{"children": row.Children, "source_hash": hash, "operation_id": row.OperationID})
		return nil
	})
}
func ReconcileParents(root string) error {
	s, err := checkpoint.ReadState(root)
	if err != nil {
		return err
	}
	// V2 helpers do not establish their owner's acceptance. Only that main
	// turn's own validated result can commit turn verification.
	if checkpoint.HasTurnSemantics(s.Plan.APIVersion) {
		return nil
	}
	change := false
	for _, row := range s.Tasks {
		if row.Status == "waiting" {
			all := true
			for _, id := range row.Children {
				all = all && s.Tasks[id].Status == "verified"
			}
			if all {
				change = true
			}
		}
	}
	if !change {
		return nil
	}
	return checkpoint.Update(root, func(s *model.State) error {
		// Bubble completion through all ownership levels in this transaction;
		// map iteration order must not strand an outer waiting graph.
		for {
			changed := false
			for id, row := range s.Tasks {
				if row.Status != "waiting" {
					continue
				}
				all := true
				for _, child := range row.Children {
					all = all && s.Tasks[child].Status == "verified"
				}
				if all {
					row.Status = "verified"
					row.Error = ""
					changed = true
					s.Event("graph_verified", id, map[string]any{"children": row.Children})
				}
			}
			if !changed {
				break
			}
		}
		return nil
	})
}
