package model

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// CurrentSchemaVersion is the storage schema version stamped on checkpoints.
const CurrentSchemaVersion = 2

type Task struct {
	ArtifactContracts map[string]ArtifactContract `json:"artifact_contracts,omitempty"`
	Profile           string                      `json:"profile,omitempty"`
	Turn              *TurnIdentity               `json:"turn,omitempty"`
	ID                string                      `json:"id"`
	Prompt            string                      `json:"prompt"`
	DependsOn         []string                    `json:"depends_on,omitempty"`
	InputFields       map[string][]string         `json:"input_fields,omitempty"`
	Requires          []string                    `json:"requires"`
	FileFields        []string                    `json:"file_fields,omitempty"`
	Equals            map[string]string           `json:"equals,omitempty"`
	MockDelay         float64                     `json:"mock_delay,omitempty"`
	MockResult        map[string]string           `json:"mock_result,omitempty"`
	Provider          string                      `json:"provider,omitempty"`
	Context           []string                    `json:"context,omitempty"`
	Checks            []Check                     `json:"checks,omitempty"`
	Outputs           map[string]string           `json:"outputs,omitempty"`
	Effort            string                      `json:"effort,omitempty"`
	Parent            string                      `json:"parent,omitempty"`
	Spawn             *SpawnPolicy                `json:"spawn,omitempty"`
	Display           *TaskDisplay                `json:"display,omitempty"`
}
type SpawnPolicy struct {
	Field       string `json:"field"`
	MaxChildren int    `json:"max_children,omitempty"`
}
type CommandSpec struct {
	Command []string `json:"command"`
	Env     []string `json:"env,omitempty"`
	Replay  string   `json:"replay,omitempty"`
	// Sandbox optionally runs Command in a container; nil keeps host execution.
	Sandbox *SandboxSpec `json:"sandbox,omitempty"`
}
type Check struct {
	Kind           string   `json:"kind"`
	Field          string   `json:"field,omitempty"`
	Value          string   `json:"value,omitempty"`
	Minimum        int      `json:"minimum,omitempty"`
	Command        []string `json:"command,omitempty"`
	Env            []string `json:"env,omitempty"`
	TimeoutSeconds float64  `json:"timeout_seconds,omitempty"`
}
type Plan struct {
	Guardrails *RuntimeGuardrails     `json:"guardrails,omitempty"`
	Campaign   *CampaignBinding       `json:"campaign,omitempty"`
	Execution  *ManagedExecutionPlan  `json:"execution,omitempty"`
	TurnShape  *TurnShape             `json:"turn_shape,omitempty"`
	APIVersion string                 `json:"api_version,omitempty"`
	Name       string                 `json:"name"`
	Tasks      []Task                 `json:"tasks"`
	Context    map[string]string      `json:"context,omitempty"`
	Providers  map[string]CommandSpec `json:"providers,omitempty"`
	MaxTasks   int                    `json:"max_tasks,omitempty"`
	MaxDepth   int                    `json:"max_depth,omitempty"`
	Display    *PlanDisplay           `json:"display,omitempty"`
	// ObserverShape is explicit, optional, read-only capacity metadata. Nil is
	// omitted so plan bytes and PlanHash are unchanged when absent.
	ObserverShape *ObserverShape `json:"observer_shape,omitempty"`
	// Experiments are opt-in and omitted when absent, preserving plan bytes.
	Experiments *PlanExperiments `json:"experiments,omitempty"`
}
type TaskState struct {
	BindingDigest string            `json:"binding_digest,omitempty"`
	Spec          Task              `json:"spec"`
	Status        string            `json:"status"`
	Attempt       int               `json:"attempt"`
	Result        map[string]string `json:"result,omitempty"`
	Error         string            `json:"error,omitempty"`
	CostUnknown   bool              `json:"cost_unknown,omitempty"`
	CostBasis     string            `json:"cost_basis,omitempty"`
	Cost          float64           `json:"reported_cost_usd"`
	// ReportedCostMicros is the exact reported cost. States written before it
	// existed derive it from Cost (see costMicros).
	ReportedCostMicros int64      `json:"reported_cost_micros"`
	Usage              *TaskUsage `json:"usage,omitempty"`
	Model              string     `json:"model,omitempty"`
	Validation         string     `json:"validation,omitempty"`
	OperationID        string     `json:"operation_id,omitempty"`
	Generation         int        `json:"generation,omitempty"`
	Children           []string   `json:"children,omitempty"`
	ExpansionHash      string     `json:"expansion_hash,omitempty"`
	CancelRequested    bool       `json:"cancel_requested,omitempty"`
	// Notes are frozen when an attempt begins (experiments.messaging) so a
	// recovered attempt receives exactly the same input.
	Notes map[string]string `json:"notes,omitempty"`
	// DiagnosticCode is the OPTIONAL stable typed failure code from the provider response (known codes only).
	// Old states without it stay valid; it is never inferred from cost or error text.
	DiagnosticCode string `json:"diagnostic_code,omitempty"`
}
type Event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Task   string    `json:"task,omitempty"`
	Detail any       `json:"detail,omitempty"`
}
type State struct {
	SchemaVersion int              `json:"schema_version,omitempty"`
	CallLimits    *SavedCallLimits `json:"call_limits,omitempty"`
	// UnbudgetedDecision is the durable, schema-independent record of how a run
	// without an explicit call budget was admitted. Storage schema is never
	// consent: ordinary checkpoint updates stamp SchemaVersion on old runs.
	// Values: "" (none), "allow" (operator passed --legacy-unbudgeted=allow) or
	// "initial" (a new run created by this code without call options, so no
	// legacy ambiguity exists). Anything else fails checkpoint validation.
	UnbudgetedDecision  string                 `json:"unbudgeted_decision,omitempty"`
	CheckpointMigration *CheckpointMigration   `json:"checkpoint_migration,omitempty"`
	Mailbox             *MailboxState          `json:"mailbox,omitempty"`
	Execution           *ManagedExecutionState `json:"execution,omitempty"`
	Version             int                    `json:"version"`
	RunID               string                 `json:"run_id,omitempty"`
	Plan                Plan                   `json:"plan"`
	PlanHash            string                 `json:"plan_hash"`
	Backend             string                 `json:"backend"`
	Control             string                 `json:"control"`
	Tasks               map[string]*TaskState  `json:"tasks"`
	Events              []Event                `json:"events"`
	Dynamic             []Task                 `json:"dynamic_tasks,omitempty"`
	// Plan and PlanHash are immutable provenance. ActiveTasks, once present,
	// is the full executable snapshot, including dynamically spawned children.
	ActiveTasks      []Task                `json:"active_tasks,omitempty"`
	GraphRevision    int                   `json:"graph_revision,omitempty"`
	Revisions        []GraphRevisionRecord `json:"revisions,omitempty"`
	Proposals        []RevisionProposal    `json:"proposals,omitempty"`
	ProposalReceipts map[string]string     `json:"proposal_receipts,omitempty"`
	Decisions        []AdaptiveDecision    `json:"decisions,omitempty"`
	ArchivedTasks    []TaskState           `json:"archived_tasks,omitempty"`
	Policy           *AdaptivePolicy       `json:"policy,omitempty"`
	// Scheduling is an explicit, opt-in ready-order policy ("breadth"). Absent
	// means the default dependency-depth order. Not part of Plan/PlanHash.
	Scheduling string `json:"scheduling,omitempty"`
}

func (s *State) AllTasks() []Task {
	if s.ActiveTasks != nil {
		return append([]Task{}, s.ActiveTasks...)
	}
	return append(append([]Task{}, s.Plan.Tasks...), s.Dynamic...)
}

func (s *State) ReportedCost() float64 {
	return AdaptiveTotalCost(s)
}
func OperationID(runID, id string, generation int) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%d", runID, id, generation))))
}
func (s *State) NewTask(t Task) *TaskState {
	generation, attempt := 1, 0
	// A revised owner can legitimately generate the same child ID again. Its
	// prior operation and attempt files remain retired evidence, never replay.
	for _, retired := range s.ArchivedTasks {
		if retired.Spec.ID != t.ID {
			continue
		}
		if retired.Generation >= generation {
			generation = retired.Generation + 1
		}
		if retired.Attempt > attempt {
			attempt = retired.Attempt
		}
	}
	row := &TaskState{Spec: t, Status: "pending", Generation: generation, Attempt: attempt, OperationID: OperationID(s.RunID, t.ID, generation)}
	if s.Execution != nil {
		row.BindingDigest = ManagedCurrentBinding(s, ManagedProfile(s.Plan, t)).Digest
	}
	return row
}

func (s *State) Event(kind, task string, detail any) {
	s.Events = append(s.Events, Event{time.Now().UTC(), kind, task, detail})
}
