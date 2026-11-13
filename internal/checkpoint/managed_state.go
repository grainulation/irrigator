package checkpoint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func governedCheckpointVersion(v string) int {
	if v == launch.ArtifactPlanVersion {
		return 5
	}
	return 4
}

func ManagedCheckpointVersion(s *model.State) int {
	if model.IsRecoveryMigratedCheckpoint(s) {
		return 5
	}
	return governedCheckpointVersion(s.Plan.APIVersion)
}

func HasTurnSemantics(version string) bool {
	return version == TurnPlanVersion || launch.IsManagedPlanVersion(version)
}
func ManagedRun(s *model.State) bool { return launch.IsManagedPlanVersion(s.Plan.APIVersion) }

// Tracking is explicit on immutable managed plans. An absent policy keeps
// cap enforcement on; checkpoints never silently lose caps.
func PlanTracksCost(p model.Plan) bool {
	return p.Execution != nil && p.Execution.CostPolicy == "track"
}
func ManagedTracksCost(s *model.State) bool { return ManagedRun(s) && PlanTracksCost(s.Plan) }
func ManagedReservedAmount(s *model.State, b model.ExecutionBinding) int64 {
	if ManagedTracksCost(s) {
		return 0
	}
	return b.OperationBudgetMicros
}

// managedExpectedBinding reconstructs pinned metadata without reading executables.
// ExecutableVerified is an expected admission fact, not a new live verification.
func ManagedExpectedBinding(c model.ExecutionCatalog, name string) model.ExecutionBinding {
	p := c.Profiles[name]
	i := c.Instances[p.Instance]
	a := c.Adapters[i.Adapter]
	amount, _ := launch.ExecutionBudgetMicros(p.OperationBudgetUSD)
	b := model.ExecutionBinding{Schema: launch.ExecutionBindingSchema, CatalogPath: "inline", CatalogSHA256: launch.ExecutionDigest(c), ProfileName: name, Profile: p, InstanceName: p.Instance, Instance: i, AdapterName: i.Adapter, Adapter: a, SupportedCapabilities: launch.ExecutionProfileCapabilities(a.Kind, p), CapabilityEvidence: "declared_contract_not_live_probe", ExecutableVerified: a.Kind == "claude_cli" || a.Kind == "codex_cli" || a.Kind == "agy_cli", ModelResolution: "requested_only", OperationBudgetMicros: amount}
	b.Digest = launch.ExecutionDigest(b)
	return b
}

func ValidateManagedPlan(p model.Plan) error {
	if err := ValidateCampaignPlan(p); err != nil {
		return err
	}
	if !launch.IsManagedPlanVersion(p.APIVersion) {
		if p.Execution != nil {
			return errors.New("execution catalog requires swarm/v3 or swarm/v4")
		}
		for _, t := range p.Tasks {
			if t.Profile != "" {
				return errors.New("task profile requires swarm/v3 or swarm/v4")
			}
		}
		return nil
	}
	if p.Execution == nil || len(p.Providers) != 0 {
		return errors.New("managed plan requires execution and forbids legacy providers")
	}
	e := p.Execution
	if launch.SupportsUnknownCost(p.APIVersion) {
		if e.CostPolicy != "retain_unknown" && e.CostPolicy != "track" {
			return errors.New("swarm/v4 and later require cost_policy retain_unknown or track")
		}
	} else {
		if e.CostPolicy != "" {
			return errors.New("cost_policy requires swarm/v4")
		}
		for _, a := range e.Catalog.Adapters {
			if a.Kind == "codex_cli" || a.Kind == "agy_cli" {
				return errors.New(a.Kind + " requires swarm/v4") // reports tokens only: needs unknown-cost support
			}
		}
	}
	for _, adapter := range e.Catalog.Adapters {
		if adapter.Kind == "ollama_http" && p.APIVersion != launch.ArtifactPlanVersion {
			return errors.New("ollama_http requires swarm/v6")
		}
	}
	if err := launch.ValidateExecutionCatalog(e.Catalog); err != nil {
		return err
	}
	if _, err := launch.ExecutionBudgetMicros(e.RunBudgetUSD); err != nil {
		return err
	}
	allowed := map[string]bool{}
	if len(e.AllowedProfiles) == 0 {
		return errors.New("managed allowed_profiles must be explicit")
	}
	for _, name := range e.AllowedProfiles {
		if _, ok := e.Catalog.Profiles[name]; !ok || allowed[name] {
			return errors.New("invalid or duplicate allowed profile")
		}
		if launch.ManagedHistoryEnabled(e.Catalog.Profiles[name]) && !launch.SupportsGovernedPlan(p.APIVersion) {
			return errors.New("managed explicit_history requires swarm/v5")
		}
		if err := ValidateManagedBinding(ManagedExpectedBinding(e.Catalog, name)); err != nil {
			return err
		}
		allowed[name] = true
	}
	if !allowed[e.Catalog.DefaultProfile] {
		return errors.New("default profile is not allowed")
	}
	for _, t := range p.Tasks {
		if t.Provider != "" || t.Effort != "" {
			return errors.New("managed tasks use profiles, not provider/effort overrides")
		}
		if !allowed[model.ManagedProfile(p, t)] {
			return fmt.Errorf("task %s profile is not allowed", t.ID)
		}
		for _, c := range t.Checks {
			if c.Kind == "command" {
				return errors.New("managed command validators require a separately budgeted execution contract")
			}
		}
	}
	return nil
}
func InitializeManagedExecution(p model.Plan) (*model.ManagedExecutionState, error) {
	if !launch.IsManagedPlanVersion(p.APIVersion) {
		return nil, nil
	}
	ceiling, _ := launch.ExecutionBudgetMicros(p.Execution.RunBudgetUSD)
	e := &model.ManagedExecutionState{Bindings: map[string]model.ExecutionBinding{}, CeilingMicros: ceiling, Reservations: map[string]model.ManagedReservation{}}
	for _, name := range p.Execution.AllowedProfiles {
		b, err := launch.ResolveExecutionCatalog(p.Execution.Catalog, "inline", name)
		if err != nil {
			return nil, err
		}
		if err := ValidateManagedBinding(b); err != nil {
			return nil, err
		}
		e.Bindings[name] = b
	}
	return e, nil
}
func ManagedBinding(s *model.State, row *model.TaskState) (model.ExecutionBinding, error) {
	if !ManagedRun(s) || s.Execution == nil || row == nil {
		return model.ExecutionBinding{}, errors.New("missing managed execution state")
	}
	b, lookupErr := ManagedBindingByDigest(s, model.ManagedProfile(s.Plan, row.Spec), row.BindingDigest)
	if lookupErr != nil {
		return model.ExecutionBinding{}, errors.New("cell execution binding changed")
	}
	if err := ValidateManagedBinding(b); err != nil {
		return model.ExecutionBinding{}, err
	}
	return b, nil
}
func ValidateManagedState(s *model.State) error {
	if err := ValidateProviderMigrations(s); err != nil {
		return err
	}
	if err := ValidateBillingReceipts(s); err != nil {
		return err
	}
	if !ManagedRun(s) {
		if s.Version != 1 || s.Execution != nil {
			return errors.New("legacy run cannot carry managed execution state")
		}
		for _, r := range s.Tasks {
			if r.BindingDigest != "" || r.CostUnknown {
				return errors.New("legacy cell cannot carry managed binding")
			}
		}
		return nil
	}
	expectedVersion := 2
	if launch.SupportsUnknownCost(s.Plan.APIVersion) {
		expectedVersion = 3
	}
	if launch.SupportsGovernedPlan(s.Plan.APIVersion) {
		expectedVersion = ManagedCheckpointVersion(s)
	}
	if s.Version != expectedVersion || s.Backend != "managed" || s.Execution == nil {
		return errors.New("managed execution checkpoint version or backend mismatch")
	}
	if err := ValidateManagedPlan(s.Plan); err != nil {
		return err
	}
	e := s.Execution
	ceiling, _ := launch.ExecutionBudgetMicros(s.Plan.Execution.RunBudgetUSD)
	if e.CeilingMicros != ceiling || len(e.Bindings) != len(s.Plan.Execution.AllowedProfiles) || e.Reservations == nil {
		return errors.New("managed budget/catalog checkpoint changed")
	}
	for _, name := range s.Plan.Execution.AllowedProfiles {
		b, ok := e.Bindings[name]
		if !ok {
			return errors.New("missing pinned profile")
		}
		expected := ManagedExpectedBinding(s.Plan.Execution.Catalog, name)
		if !reflect.DeepEqual(b, expected) {
			return errors.New("pinned binding differs from immutable catalog")
		}
		if err := ValidateManagedBinding(b); err != nil {
			return err
		}
	}
	for _, r := range s.Tasks {
		if r.CostUnknown && s.Version != 3 && s.Version != 4 && s.Version != 5 {
			return errors.New("unknown cost requires checkpoint3")
		}
		b, err := ManagedBinding(s, r)
		if err != nil {
			return err
		}
		if r.Attempt > 0 && (r.Status == "running" || r.Status == "produced" || r.Status == "verified" || r.Status == "uncertain" || r.Status == "failed") {
			receipt, ok := e.Reservations[r.OperationID]
			if !ok || receipt.Attempt != r.Attempt {
				return errors.New("attempt missing its managed reservation")
			}
		}
		owner := ""
		if r.Spec.Parent != "" {
			owner = r.Spec.Parent
		} else if r.Spec.Turn != nil {
			owner = r.Spec.Turn.PreviousTask
		}
		if owner != "" {
			o := s.Tasks[owner]
			if o == nil || model.ManagedProfile(s.Plan, o.Spec) != b.ProfileName {
				return errors.New("managed generated task changed owner profile")
			}
		}
	}
	for op, r := range e.Reservations {
		if op != r.OperationID || r.TaskID == "" || r.Attempt < 1 || r.ReservedMicros < 0 || r.ReportedMicros < 0 {
			return errors.New("invalid managed reservation identity or cost")
		}
		row := s.Tasks[r.TaskID]
		if row == nil {
			return errors.New("reservation has no admitted cell")
		}
		found := false
		for generation := 1; generation <= row.Generation; generation++ {
			if model.OperationID(s.RunID, r.TaskID, generation) == op {
				found = true
				break
			}
		}
		b, err := ManagedBindingByDigest(s, model.ManagedProfile(s.Plan, row.Spec), r.BindingDigest)
		if err != nil {
			return err
		}
		if !found || r.BindingDigest != b.Digest || r.ReservedMicros != ManagedReservedAmount(s, b) || r.Attempt > row.Attempt {
			return errors.New("reservation lineage or binding mismatch")
		}
		if r.Status != "held" && r.Status != "uncertain" && r.Status != "settled" && r.Status != "unknown_cost" {
			return errors.New("invalid managed reservation status")
		}
		if r.Status == "unknown_cost" && (s.Version != 3 && s.Version != 4 && s.Version != 5 || r.ReportedMicros != 0) {
			return errors.New("invalid unknown-cost reservation")
		}
		if op == row.OperationID {
			if r.BindingDigest != row.BindingDigest {
				return errors.New("current reservation binding differs from cell")
			}
			if row.CostUnknown && r.Status != "unknown_cost" && r.Status != "uncertain" {
				return errors.New("unknown cell cost must retain reservation")
			}
			if r.Status == "unknown_cost" && (!row.CostUnknown || row.Status == "pending" || row.Status == "running") {
				return errors.New("unknown-cost reservation disagrees with cell")
			}
			// Output reconciliation can accept a result without establishing cost:
			// produced/verified/failed/cancelled may retain an uncertain hold.
			// The reverse is forbidden: uncertainty never becomes settled by a
			// standalone ledger status edit, and running work must retain admission.
			if row.Status == "uncertain" && r.Status != "uncertain" {
				return errors.New("uncertain managed cell requires an uncertain reservation")
			}
			if row.Status == "running" && r.Status != "held" {
				return errors.New("running managed cell requires a held reservation")
			}
			if r.Status == "held" && row.Status != "running" && !(row.Status == "pending" && b.Adapter.Kind == "mock" && r.ReservedMicros == 0 && r.ReportedMicros == 0) {
				return errors.New("managed held reservation disagrees with cell lifecycle")
			}
			if r.Status == "uncertain" && row.Status == "pending" {
				return errors.New("uncertain managed operation cannot be pending for replay")
			}
		}
		if !ManagedTracksCost(s) && r.ReportedMicros > r.ReservedMicros && !e.Frozen {
			return errors.New("managed cost overrun must freeze admission")
		}
	}
	if used, err := ManagedBudgetUsed(e); err != nil {
		return err
	} else if !ManagedTracksCost(s) && used > e.CeilingMicros && !e.Frozen {
		return errors.New("managed overspend must freeze admission")
	}
	return nil
}
func ManagedBudgetUsed(e *model.ManagedExecutionState) (int64, error) {
	total := int64(0)
	for _, r := range e.Reservations {
		amount, settled := ManagedAccountedAmount(e, r)
		if !settled && r.ReservedMicros > amount {
			amount = r.ReservedMicros
		}
		if amount < 0 || total > math.MaxInt64-amount {
			return 0, errors.New("managed budget overflow")
		}
		total += amount
	}
	return total, nil
}

func validateManagedPlanJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	value, err := jsonio.ParseStrictJSON(decoder, 0)
	if err != nil {
		return errors.New("invalid or duplicate managed plan JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing managed plan JSON")
	}
	if err := model.ExactTurnJSONFields(value, reflect.TypeOf(model.Plan{})); err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("managed plan must be object")
	}
	return launch.ExecutionJSONShape(object["execution"], reflect.TypeOf(model.ManagedExecutionPlan{}))
}
func validateManagedCheckpointJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	value, err := jsonio.ParseStrictJSON(decoder, 0)
	if err != nil {
		return errors.New("invalid or duplicate managed checkpoint JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing managed checkpoint JSON")
	}
	return model.ExactTurnJSONFields(value, reflect.TypeOf(model.State{}))
}
