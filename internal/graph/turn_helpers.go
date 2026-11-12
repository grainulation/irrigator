package graph

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

type TurnHelperRequest struct {
	ID      string       `json:"id"`
	Helpers []model.Task `json:"helpers"`
}

func AdmitTurnHelpers(root, id, operation string, attempt int, request TurnHelperRequest) error {
	if !checkpoint.ProposalIDRe.MatchString(request.ID) || len(request.Helpers) == 0 || len(request.Helpers) > 64 || operation == "" || attempt < 1 {
		return errors.New("helper request requires an ID, exact operation/attempt and 1..64 helpers")
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > model.MaxInputBytes {
		return errors.New("helper request exceeds size limit")
	}
	requestHash := fmt.Sprintf("%x", sha256.Sum256(raw))
	return checkpoint.Update(root, func(s *model.State) error {
		row := s.Tasks[id]
		if !checkpoint.HasTurnSemantics(s.Plan.APIVersion) || row == nil || row.Spec.Turn == nil || row.Spec.Turn.Role != "main" {
			return errors.New("asynchronous helpers require a v2 main turn")
		}
		// Durable identical retries return the original admission without spending
		// allowance again, even after the owner has settled. Conflicts stay rejected.
		for _, childID := range row.Children {
			a := s.Tasks[childID].Spec.Turn.Async
			if a != nil && a.ID == request.ID {
				if a.RequestHash != requestHash || a.OperationID != operation || a.Attempt != attempt {
					return errors.New("helper admission ID payload conflict")
				}
				return nil
			}
		}
		if row.Status != "running" || row.OperationID != operation || row.Attempt != attempt || row.CancelRequested || s.Control != "run" {
			return errors.New("helper admission requires the exact live owner attempt")
		}
		// Reuse the boundary admission capability validator and generated-ID rules.
		// The owner result is deliberately absent, and the original row is untouched.
		encoded, _ := json.Marshal(checkpoint.TurnRequest{Helpers: request.Helpers})
		pending := *row
		pending.ExpansionHash = ""
		pending.Result = map[string]string{artifactstore.TurnRequestField: string(encoded)}
		tasks, _, err := turnExpansion(s, &pending)
		if err != nil {
			return err
		}
		for i := range tasks {
			t := &tasks[i]
			t.DependsOn = append([]string(nil), row.Spec.DependsOn...)
			// Preserve nil/empty map representation through the JSON checkpoint.
			t.InputFields = row.Spec.InputFields
			t.Turn.Async = &model.HelperAdmission{ID: request.ID, OperationID: operation, Attempt: attempt, Count: len(tasks), RequestHash: requestHash, SpecHash: checkpoint.HelperSpecHash(*t)}
		}
		plan := s.Plan
		plan.Tasks = append(s.AllTasks(), tasks...)
		if err := checkpoint.Validate(plan); err != nil {
			return err
		}
		for _, t := range tasks {
			if err := os.MkdirAll(filepath.Join(root, "cells", t.ID), 0700); err != nil {
				return err
			}
			if s.ActiveTasks != nil {
				s.ActiveTasks = append(s.ActiveTasks, t)
			} else {
				s.Dynamic = append(s.Dynamic, t)
			}
			s.Tasks[t.ID] = s.NewTask(t)
			row.Children = append(row.Children, t.ID)
		}
		if err := checkpoint.ValidateTurnState(s); err != nil {
			return err
		}
		s.Event("helpers_admitted", id, map[string]any{"admission_id": request.ID, "operation_id": operation, "attempt": attempt, "helpers": len(tasks)})
		return nil
	})
}

func TurnHelpersCLI(args []string) (int, error) {
	if len(args) < 2 {
		return 2, errors.New("helpers expects DIR NODE --operation OP --attempt N --request FILE")
	}
	fs := flag.NewFlagSet("helpers", flag.ContinueOnError)
	operation := fs.String("operation", "", "exact running owner operation ID")
	attempt := fs.Int("attempt", 0, "exact running owner attempt")
	path := fs.String("request", "", "bounded JSON {id,helpers} request")
	if err := commandflags.ParseCommandFlags(fs, args[2:]); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 || *path == "" {
		return 2, errors.New("helper request file is required")
	}
	f, err := os.Open(*path)
	if err != nil {
		return 2, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, model.MaxInputBytes+1))
	if err != nil {
		return 2, err
	}
	if len(raw) > model.MaxInputBytes {
		return 2, errors.New("helper request exceeds 1 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	value, err := jsonio.ParseStrictJSON(decoder, 0)
	if err != nil {
		return 2, err
	}
	if _, ok := value.(map[string]any); !ok {
		return 2, errors.New("helper request must be an object")
	}
	if err = model.ExactTurnJSONFields(value, reflect.TypeOf(TurnHelperRequest{})); err != nil {
		return 2, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return 2, errors.New("helper request must be one object")
	}
	var request TurnHelperRequest
	if err = json.Unmarshal(raw, &request); err != nil {
		return 2, err
	}
	if err = AdmitTurnHelpers(args[0], args[1], *operation, *attempt, request); err != nil {
		return 1, err
	}
	return 0, nil
}
