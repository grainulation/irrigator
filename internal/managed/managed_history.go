package managed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// ManagedHistory is derived only from the admitted checkpoint snapshot. It is
// portable accepted task data, not a provider session or private reasoning trace.
type ManagedHistory struct {
	Schema        string                `json:"schema"`
	RunID         string                `json:"run_id"`
	PlanSHA256    string                `json:"plan_sha256"`
	AgentID       string                `json:"agent_id"`
	TaskID        string                `json:"task_id"`
	OperationID   string                `json:"operation_id"`
	Attempt       int                   `json:"attempt"`
	Index         int                   `json:"index"`
	BindingDigest string                `json:"binding_digest"`
	Boundary      string                `json:"boundary"`
	Entries       []ManagedHistoryEntry `json:"entries"`
	Digest        string                `json:"digest"`
}
type ManagedHistoryEntry struct {
	TaskID       string            `json:"task_id"`
	OperationID  string            `json:"operation_id"`
	Generation   int               `json:"generation"`
	Attempt      int               `json:"attempt"`
	Index        int               `json:"index"`
	Prompt       string            `json:"prompt"`
	Output       map[string]string `json:"output"`
	OutputSHA256 string            `json:"output_sha256"`
}

func ManagedBytesDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func ManagedHistoryDigest(h ManagedHistory) string {
	h.Digest = ""
	raw, _ := json.Marshal(h)
	return ManagedBytesDigest(raw)
}
func managedHistoryOutput(row *model.TaskState) (map[string]string, error) {
	output := map[string]string{}
	if row == nil || len(row.Spec.Requires) == 0 || row.Result == nil {
		return nil, errors.New("history source lacks accepted output contract")
	}
	for _, field := range row.Spec.Requires {
		if field == artifactstore.TurnRequestField {
			continue
		}
		v, ok := row.Result[field]
		if !ok {
			return nil, errors.New("history source lacks required accepted output")
		}
		output[field] = v
	}
	return output, nil
}
func historyFieldsAvailable(output, allowed map[string]string) bool {
	for k, v := range output {
		if actual, ok := allowed[k]; !ok || actual != v {
			return false
		}
	}
	return true
}
func BuildManagedHistory(s *model.State, req model.WorkRequest, b model.ExecutionBinding) (*ManagedHistory, error) {
	if !launch.ManagedHistoryEnabled(b.Profile) {
		return nil, nil
	}
	if s == nil || !launch.SupportsGovernedPlan(s.Plan.APIVersion) || s.Version != checkpoint.ManagedCheckpointVersion(s) || s.PlanHash != launch.ExecutionDigest(s.Plan) || req.RunID != s.RunID {
		return nil, errors.New("managed history requires pinned swarm/v5 snapshot")
	}
	row := s.Tasks[req.Task.ID]
	if row == nil || row.Status != "running" || row.Generation < 1 || row.OperationID != model.OperationID(s.RunID, row.Spec.ID, row.Generation) || row.OperationID != req.OperationID || row.Attempt != req.Attempt || row.BindingDigest != b.Digest || !reflect.DeepEqual(row.Spec, req.Task) {
		return nil, errors.New("managed history current execution fence mismatch")
	}
	expectedInputs, err := InputsForTask(s, row.Spec)
	if err != nil {
		return nil, err
	}
	if launch.WorkerContextInputDigest(TaskContext(s.Plan, row.Spec), expectedInputs) != launch.WorkerContextInputDigest(req.Context, req.Dependencies) {
		return nil, errors.New("managed history current input authority mismatch")
	}
	turn := row.Spec.Turn
	if turn == nil {
		return nil, errors.New("managed history requires turn identity")
	}
	if turn.Role == "helper" {
		return nil, nil
	}
	if turn.Role != "main" || turn.Index < 1 || turn.Index > model.MaxMainTurns {
		return nil, errors.New("invalid managed history main identity")
	}
	h := &ManagedHistory{Schema: "irrigator.managed-history/v1", RunID: s.RunID, PlanSHA256: s.PlanHash, AgentID: turn.AgentID, TaskID: req.Task.ID, OperationID: req.OperationID, Attempt: req.Attempt, Index: turn.Index, BindingDigest: b.Digest, Boundary: "start_of_agent", Entries: []ManagedHistoryEntry{}}
	// Validate the entire exact chain first. A privacy boundary cannot conceal a
	// missing, stale, unaccepted, or cross-agent source.
	chain := []*model.TaskState{}
	next := row
	for index := turn.Index - 1; index >= 1; index-- {
		prev := s.Tasks[next.Spec.Turn.PreviousTask]
		if prev == nil || prev.Spec.Turn == nil || prev.Spec.Turn.Role != "main" || prev.Spec.Turn.AgentID != turn.AgentID || prev.Spec.Turn.Index != index || prev.Status != "verified" || prev.Generation < 1 || prev.Attempt < 1 || prev.OperationID != model.OperationID(s.RunID, prev.Spec.ID, prev.Generation) {
			return nil, errors.New("history requires exact verified predecessor chain")
		}
		if _, err := managedHistoryOutput(prev); err != nil {
			return nil, err
		}
		chain = append(chain, prev)
		next = prev
	}
	if next.Spec.Turn.PreviousTask != "" {
		return nil, errors.New("history first turn has unexpected predecessor")
	}
	next = row
	for _, prev := range chain {
		if prev.BindingDigest != b.Digest {
			h.Boundary = "profile_boundary"
			break
		}
		if launch.WorkerContextInputDigest(TaskContext(s.Plan, prev.Spec), nil) != launch.WorkerContextInputDigest(req.Context, nil) {
			h.Boundary = "context_boundary"
			break
		}
		output, err := managedHistoryOutput(prev)
		if err != nil {
			return nil, err
		}
		nextInputs, err := InputsForTask(s, next.Spec)
		if err != nil {
			return nil, err
		}
		currentAllowed, sourceAllowed := req.Dependencies[prev.Spec.ID]
		if !sourceAllowed || !historyFieldsAvailable(output, nextInputs[prev.Spec.ID]) || !historyFieldsAvailable(output, currentAllowed) {
			h.Boundary = "output_selection_boundary"
			break
		}
		priorInputs, err := InputsForTask(s, prev.Spec)
		if err != nil {
			return nil, err
		}
		permitted := true
		for id, fields := range priorInputs {
			source := s.Tasks[id]
			if source == nil {
				return nil, errors.New("missing history input authority source")
			}
			allowed, exists := req.Dependencies[id]
			if !exists || !historyFieldsAvailable(fields, allowed) {
				permitted = false
				break
			}
		}
		if !permitted {
			h.Boundary = "external_input_boundary"
			break
		}
		h.Entries = append(h.Entries, ManagedHistoryEntry{TaskID: prev.Spec.ID, OperationID: prev.OperationID, Generation: prev.Generation, Attempt: prev.Attempt, Index: prev.Spec.Turn.Index, Prompt: prev.Spec.Prompt, Output: output, OutputSHA256: launch.ExecutionDigest(output)})
		next = prev
	}
	for i, j := 0, len(h.Entries)-1; i < j; i, j = i+1, j-1 {
		h.Entries[i], h.Entries[j] = h.Entries[j], h.Entries[i]
	}
	h.Digest = ManagedHistoryDigest(*h)
	if err := validateManagedHistory(req, b, h); err != nil {
		return nil, err
	}
	return h, nil
}
func validateManagedHistory(req model.WorkRequest, b model.ExecutionBinding, h *ManagedHistory) error {
	opted := launch.ManagedHistoryEnabled(b.Profile)
	if !opted {
		if h != nil {
			return errors.New("unsolicited managed history")
		}
		return nil
	}
	turn := req.Task.Turn
	if turn == nil {
		return errors.New("explicit_history requires turn identity")
	}
	if turn.Role == "helper" {
		if h != nil {
			return errors.New("helpers cannot inherit managed history")
		}
		return nil
	}
	if h == nil || h.Schema != "irrigator.managed-history/v1" || turn.Role != "main" || h.RunID != req.RunID || h.AgentID != turn.AgentID || h.TaskID != req.Task.ID || h.OperationID != req.OperationID || h.Attempt != req.Attempt || h.Index != turn.Index || h.BindingDigest != b.Digest || len(h.PlanSHA256) != 64 || h.Digest != ManagedHistoryDigest(*h) || h.Index < 1 || h.Index > model.MaxMainTurns || len(h.Entries) >= h.Index {
		return errors.New("managed history identity or digest mismatch")
	}
	switch h.Boundary {
	case "start_of_agent", "profile_boundary", "context_boundary", "output_selection_boundary", "external_input_boundary":
	default:
		return errors.New("invalid managed history boundary")
	}
	if h.Boundary == "start_of_agent" && len(h.Entries) != h.Index-1 {
		return errors.New("incomplete managed history without explicit boundary")
	}
	seen := map[string]bool{}
	for i, e := range h.Entries {
		if e.TaskID == "" || seen[e.TaskID] || e.Index != h.Index-len(h.Entries)+i || e.Generation < 1 || e.Attempt < 1 || e.OperationID != model.OperationID(h.RunID, e.TaskID, e.Generation) || e.Prompt == "" || e.Output == nil || e.OutputSHA256 != launch.ExecutionDigest(e.Output) {
			return errors.New("invalid managed history source identity/output")
		}
		seen[e.TaskID] = true
	}
	if len(h.Entries) > 0 && h.Entries[len(h.Entries)-1].TaskID != turn.PreviousTask {
		return errors.New("managed history predecessor mismatch")
	}
	raw, _ := json.Marshal(h)
	if len(raw) > model.MaxInputBytes {
		return errors.New("managed history exceeds global input bound")
	}
	return nil
}
func ManagedInput(req model.WorkRequest, b model.ExecutionBinding, h *ManagedHistory) ([]byte, error) {
	if err := validateManagedHistory(req, b, h); err != nil {
		return nil, err
	}
	var raw []byte
	var err error
	if h == nil {
		raw, err = json.Marshal(req)
	} else {
		raw, err = json.Marshal(struct {
			model.WorkRequest

			History *ManagedHistory `json:"history"`
		}{req, h})
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > model.MaxInputBytes || int64(len(raw)) > b.Profile.Limits.InputBytes {
		return nil, fmt.Errorf("managed request including history exceeds input limit")
	}
	return raw, nil
}
