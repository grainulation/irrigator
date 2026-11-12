package launch

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/grainulation/irrigator/internal/model"
)

const RuntimeGuardrailsSchema = "irrigator.guardrails/v1"

type runtimeContract struct {
	ID         string `json:"id"`
	Available  bool   `json:"available"`
	Scope      string `json:"scope"`
	Limitation string `json:"limitation"`
}

// This is a conservative implementation declaration, not a live backend probe.
// Unsupported guarantees cannot be made available by adapter-authored strings.
func RuntimeContracts(p model.Plan) []runtimeContract {
	managed := IsManagedPlanVersion(p.APIVersion) && p.Execution != nil
	holds := managed && p.Execution.CostPolicy != "track"
	return []runtimeContract{
		{"managed_binding_validation", managed, "Pinned adapter/profile/schema and capability validation before managed dispatch", "Does not prove filesystem isolation, complete tool mediation or authentication."},
		{"managed_cost_reservations", holds, "Atomic monetary admission reservations and trailing reconciliation", "No invoice or hard token cap; provider overruns remain possible."},
		{"unknown_cost_holds", holds && SupportsUnknownCost(p.APIVersion), "Unknown costs retain monetary reservations until reconciliation", "Usage can remain unknown; token tranches are not implemented."},
		{"attempt_fencing", true, "Checkpoint transitions bind run, operation, attempt and execution binding", "Does not make external effects exactly once."},
		{"process_group_cleanup", true, "Local launched-process group termination and unresolved-cleanup markers", "Detached descendants and remote inference are outside the POSIX group boundary; executor-specific hosting differs."},
		{"checkpoint_recovery", true, "Exclusive execution-authority recovery and uncertain-outcome preservation", "Does not snapshot dirty Git worktrees or roll back external effects."},
		{"decision_link_single_use", true, "Signed decision links; POST consumption is serialized, durable and checked for expiry", "Only requests routed to approvals are covered; legacy tokens lack attempt binding, and no exact tool-payload grant is established."},
		{"bounded_artifact_validation", p.APIVersion == ArtifactPlanVersion, "Bounded typed JSON artifact checks and fenced acceptance", "A JSON string naming a Git OID is not proof of tree contents or integrated tests."},
		{"evidence_receipt", true, "Read-only digest-bound projection of recorded state and unresolved holds", "A local digest is not a signature, live process attestation or provider invoice."},
		{"dirty_worktree_quarantine", true, "Explicit git quarantine command captures reachable recovery objects and preserves the disk worktree", "Operator must confirm writers stopped; arbitrary agent worktrees are not automatically captured."},
		{"recoverable_git_settlement", true, "Explicit prepared, verified, accepted candidate with durable intent and conditional Irrigator-owned ref advancement", "Only refs/irrigator/integration targets; no automatic developer checkout synchronization or external-effect rollback."},
		{"host_resource_limits", PlanHostResourceLimits(p), "Explicit per-provider container CPU/memory settings, checked against runtime support before launch", "Runtime-specific; requires named sandbox providers. No uncontained fallback or zero-overhead promise."},
		{"hard_token_limit", PlanHasHardOutputTokenLimit(p), "Explicit output-generation ceilings on trusted local Ollama profiles", "Not a prompt-token, aggregate-run, inference-time or monetary ceiling; depends on the configured server honoring num_predict."},
		{"automatic_worktree_quarantine", false, "No generic per-cell worktree discovery hook", "Use the explicit Git quarantine command after stopping writers."},
		{"automatic_git_settlement", false, "No implicit candidate acceptance or branch application", "Use explicit Git prepare/accept/settle/reconcile commands."},
		{"complete_tool_mediation", false, "No complete mediation contract", "Cooperative protocols and deny lists do not intercept arbitrary host effects."},
	}
}
func ValidateRuntimeGuardrails(p model.Plan) error {
	g := p.Guardrails
	if g == nil {
		return nil
	}
	if (p.APIVersion != ArtifactPlanVersion && p.APIVersion != "" && p.APIVersion != "swarm/v1") || g.Schema != RuntimeGuardrailsSchema || g.MaxInflight < 0 || g.MaxInflight > 1024 {
		return errors.New("guardrails require swarm/v1 or swarm/v6, irrigator.guardrails/v1 and max_inflight 0..1024")
	}
	available := map[string]bool{}
	for _, c := range RuntimeContracts(p) {
		available[c.ID] = c.Available
	}
	seen := map[string]bool{}
	for _, id := range g.Require {
		if seen[id] || !available[id] {
			return fmt.Errorf("guardrail requirement unavailable, unknown or duplicated: %s", id)
		}
		seen[id] = true
	}
	return nil
}

// Runs inside the existing checkpoint admission transaction, before reserving
// funds or incrementing attempts. Unknown operations keep their concurrency slot.
func AdmitRuntimeGuardrails(s *model.State, id string) error {
	effective := s.Plan
	effective.Tasks = s.AllTasks()
	if err := ValidateRuntimeGuardrails(effective); err != nil {
		return err
	}
	g := s.Plan.Guardrails
	if g == nil || g.MaxInflight == 0 {
		return nil
	}
	active := map[string]bool{}
	key := func(op, task string, attempt int) string { return fmt.Sprintf("%s/%s/%d", op, task, attempt) }
	for task, row := range s.Tasks {
		switch row.Status {
		case "running", "produced", "uncertain":
			active[key(row.OperationID, task, row.Attempt)] = true
		}
		if row.CostUnknown {
			active[key(row.OperationID, task, row.Attempt)] = true
		}
	}
	if s.Execution != nil {
		for _, r := range s.Execution.Reservations {
			if r.Status != "settled" {
				active[key(r.OperationID, r.TaskID, r.Attempt)] = true
			}
		}
	}
	// Only recovery of the same running attempt can reuse its existing slot.
	row := s.Tasks[id]
	reusing := row != nil && row.Status == "running" && active[key(row.OperationID, id, row.Attempt)]
	if len(active) > g.MaxInflight || (!reusing && len(active) >= g.MaxInflight) {
		return ErrNoWork
	}
	return nil
}

type contractTaskReceipt struct {
	ID             string   `json:"task_id"`
	OperationID    string   `json:"operation_id"`
	Generation     int      `json:"generation"`
	Attempt        int      `json:"attempt"`
	BindingDigest  string   `json:"binding_digest,omitempty"`
	Status         string   `json:"status"`
	Validation     string   `json:"validation,omitempty"`
	ReportedMicros int64    `json:"reported_micros"`
	CostUnknown    bool     `json:"cost_unknown"`
	Unresolved     []string `json:"unresolved"`
}
type runtimeEvidenceReceipt struct {
	Schema       string                     `json:"schema"`
	RunID        string                     `json:"run_id"`
	PlanHash     string                     `json:"plan_hash"`
	StateDigest  string                     `json:"state_sha256"`
	Control      string                     `json:"control"`
	Contracts    []runtimeContract          `json:"contracts"`
	Tasks        []contractTaskReceipt      `json:"tasks"`
	Reservations []model.ManagedReservation `json:"reservations"`
	Frozen       bool                       `json:"accounting_frozen"`
	Digest       string                     `json:"sha256"`
}

func BuildRuntimeEvidence(s *model.State) (runtimeEvidenceReceipt, error) {
	r := runtimeEvidenceReceipt{Schema: "irrigator.contract-evidence/v1", RunID: s.RunID, PlanHash: s.PlanHash, Control: s.Control, Contracts: RuntimeContracts(s.Plan), Tasks: []contractTaskReceipt{}, Reservations: []model.ManagedReservation{}}
	stateBytes, err := json.Marshal(s)
	if err != nil {
		return r, err
	}
	r.StateDigest = fmt.Sprintf("%x", sha256.Sum256(stateBytes))
	ids := []string{}
	for id := range s.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		row := s.Tasks[id]
		u := []string{}
		if row.Status == "uncertain" {
			u = append(u, "external_outcome_uncertain")
		}
		if row.Status == "running" || row.Status == "produced" {
			u = append(u, "attempt_not_final")
		}
		if row.CostUnknown {
			u = append(u, "usage_or_cost_unreconciled")
		}
		r.Tasks = append(r.Tasks, contractTaskReceipt{ID: id, OperationID: row.OperationID, Generation: row.Generation, Attempt: row.Attempt, BindingDigest: row.BindingDigest, Status: row.Status, Validation: row.Validation, ReportedMicros: row.CostMicros(), CostUnknown: row.CostUnknown, Unresolved: u})
	}
	if s.Execution != nil {
		r.Frozen = s.Execution.Frozen
		ops := []string{}
		for op := range s.Execution.Reservations {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			r.Reservations = append(r.Reservations, s.Execution.Reservations[op])
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	r.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
	return r, nil
}
