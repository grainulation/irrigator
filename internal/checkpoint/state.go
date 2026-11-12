package checkpoint

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"syscall"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/cost"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// All processes cooperate through flock; atomic replacement lets readers see
// either the previous complete checkpoint or the next complete checkpoint.
func FileLock(path string, nonblock bool) (*os.File, error) {
	if err := RejectRecoveryRun(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_EX
	if nonblock {
		mode |= syscall.LOCK_NB
	}
	if err = syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	if err = RejectRecoveryRun(filepath.Dir(path)); err != nil {
		Unlock(f)
		return nil, err
	}
	return f, nil
}
func Unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
func ReadState(root string) (*model.State, error) {
	return (FileCheckpointStore{root}).Read(context.Background())
}

func readFileCheckpoint(root string) (*model.State, error) {
	data, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		return nil, err
	}
	var s model.State
	if err = json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid checkpoint: %w", err)
	}
	if s.SchemaVersion > model.CurrentSchemaVersion {
		return nil, fmt.Errorf("unsupported schema_version %d (max supported %d)", s.SchemaVersion, model.CurrentSchemaVersion)
	}
	if s.Version == 2 || s.Version == 3 || s.Version == 4 || s.Version == 5 {
		if err := validateManagedCheckpointJSON(data); err != nil {
			return nil, err
		}
	}
	if s.Version != 1 && s.Version != 2 && s.Version != 3 && s.Version != 4 && s.Version != 5 {
		return nil, fmt.Errorf("unsupported checkpoint version %d", s.Version)
	}
	encoded, _ := json.Marshal(s.Plan)
	if fmt.Sprintf("%x", sha256.Sum256(encoded)) != s.PlanHash {
		return nil, errors.New("plan checkpoint changed: create a new run for a revised plan")
	}
	if len(s.Tasks) != len(s.AllTasks()) {
		return nil, errors.New("checkpoint task set differs from plan")
	}
	if err := ValidateCellNames(HistoricalCellTasks(&s)); err != nil {
		return nil, fmt.Errorf("unsafe checkpoint cell names: %w", err)
	}
	for _, spec := range s.AllTasks() {
		row := s.Tasks[spec.ID]
		if row == nil || !reflect.DeepEqual(row.Spec, spec) {
			return nil, errors.New("checkpoint task specification differs from plan")
		}
	}
	if s.RunID == "" {
		s.RunID = "legacy-" + s.PlanHash
	}
	// Older checkpoints carry only the float; derive the exact amount once.
	for i := range s.ArchivedTasks {
		m, err := cost.ResolveCost(s.ArchivedTasks[i].ReportedCostMicros, s.ArchivedTasks[i].Cost)
		if err != nil {
			return nil, fmt.Errorf("invalid reported cost in archived task: %w", err)
		}
		s.ArchivedTasks[i].ReportedCostMicros = m
		s.ArchivedTasks[i].Cost = cost.MicrosToFloat(m)
		if s.ArchivedTasks[i].CostBasis == "" && s.ArchivedTasks[i].Attempt > 0 {
			if s.ArchivedTasks[i].CostUnknown {
				s.ArchivedTasks[i].CostBasis = "unknown"
			} else {
				s.ArchivedTasks[i].CostBasis = "reported"
			}
		}
	}
	for id, row := range s.Tasks {
		m, err := cost.ResolveCost(row.ReportedCostMicros, row.Cost)
		if err != nil {
			return nil, fmt.Errorf("invalid reported cost in checkpoint for task %s: %w", id, err)
		}
		row.ReportedCostMicros = m
		row.Cost = cost.MicrosToFloat(m)
		if row.CostBasis == "" && row.Attempt > 0 {
			if row.CostUnknown {
				row.CostBasis = "unknown"
			} else if row.ReportedCostMicros > 0 || row.Cost > 0 {
				row.CostBasis = "reported"
			} else {
				isUnknown := false
				for _, ev := range s.Events {
					if ev.Kind == "legacy_cost_unknown" || ev.Kind == "cost_unknown" {
						if d, ok := ev.Detail.(map[string]any); ok && d["operation_id"] == row.OperationID {
							isUnknown = true
							break
						}
					}
				}
				if isUnknown {
					row.CostBasis = "unknown"
				} else {
					row.CostBasis = "reported"
				}
			}
		}
		if row.Generation == 0 {
			row.Generation = 1
		}
		if row.OperationID == "" {
			row.OperationID = model.OperationID(s.RunID, id, row.Generation)
		}
	}
	for _, row := range s.Tasks {
		if row.Spec.ArtifactContracts != nil && s.Plan.APIVersion != launch.ArtifactPlanVersion {
			return nil, errors.New("typed checkpoint requires swarm/v6")
		}
		if err := artifactstore.ValidateArtifactContracts(row.Spec); err != nil {
			return nil, err
		}
		if row.Status == "verified" {
			if err := artifactstore.AcceptArtifactContracts(row.Spec, row.Result); err != nil {
				return nil, err
			}
		}
	}
	if err := model.ValidateCallLimits(s.CallLimits); err != nil {
		return nil, err
	}
	if err := validateUnbudgetedDecision(s.UnbudgetedDecision); err != nil {
		return nil, err
	}
	if err := ValidateMailboxState(&s); err != nil {
		return nil, err
	}
	if err := validateRecoveryCheckpoint(root, &s); err != nil {
		return nil, err
	}
	if err := ValidateManagedState(&s); err != nil {
		return nil, err
	}
	if err := ValidateOwnership(&s); err != nil {
		return nil, err
	}
	if err := backfillProposalReceipts(&s); err != nil {
		return nil, err
	}
	if err := ValidateRevisionState(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func Update(root string, fn func(*model.State) error) error {
	return (FileCheckpointStore{root}).Update(context.Background(), CheckpointUpdateOptions{}, fn)
}

// Cells are directories. ASCII task names must remain distinct on both
// case-sensitive and case-insensitive filesystems, including restored runs.
func HistoricalCellTasks(s *model.State) []model.Task {
	tasks := append([]model.Task{}, s.AllTasks()...)
	for _, archived := range s.ArchivedTasks {
		tasks = append(tasks, archived.Spec)
	}
	return tasks
}

func ValidateCellNames(tasks []model.Task) error {
	seen := make(map[string]string, len(tasks))
	for _, task := range tasks {
		key := strings.ToLower(task.ID)
		if previous, ok := seen[key]; ok && previous != task.ID {
			return fmt.Errorf("task IDs %q and %q collide on case-insensitive filesystems", previous, task.ID)
		}
		seen[key] = task.ID
	}
	return nil
}

func Validate(p model.Plan) error {
	return ValidatePlanContext(p, false)
}

func ValidatePlanContext(p model.Plan, campaignAdmission bool) error {
	if err := ValidateCellNames(p.Tasks); err != nil {
		return err
	}
	if err := validateTurnPlan(p); err != nil {
		return err
	}
	if p.MaxTasks < 0 || p.MaxTasks > 1024 || p.MaxDepth < 0 || p.MaxDepth > 16 {
		return errors.New("max_tasks must be 1..1024 and max_depth 1..16 (or omitted)")
	}
	maxTasks := p.MaxTasks
	if maxTasks == 0 {
		maxTasks = 128
	}
	if len(p.Tasks) > maxTasks {
		return errors.New("plan exceeds max_tasks")
	}
	if p.APIVersion != "" && p.APIVersion != "swarm/v1" && p.APIVersion != TurnPlanVersion && !launch.IsManagedPlanVersion(p.APIVersion) {
		return fmt.Errorf("unsupported plan api_version %q", p.APIVersion)
	}
	if err := ValidateManagedPlan(p); err != nil {
		return err
	}
	if err := launch.ValidateRuntimeGuardrails(p); err != nil {
		return err
	}
	if len(p.Tasks) == 0 {
		return errors.New("plan needs tasks")
	}
	if err := validateExperimentsContext(p, campaignAdmission); err != nil {
		return err
	}
	if err := model.ValidatePlanDisplay(p); err != nil {
		return err
	}
	ids := map[string]bool{}
	outputs := map[string]bool{}
	for name, provider := range p.Providers {
		if provider.Replay != "" && provider.Replay != "safe" && provider.Replay != "manual" {
			return fmt.Errorf("provider %s has invalid replay policy", name)
		}
		if name == "haiku" || name == "mock" || name == "" {
			return fmt.Errorf("reserved provider name %q", name)
		}
		if len(provider.Command) == 0 || provider.Command[0] == "" {
			return fmt.Errorf("provider %s needs command argv", name)
		}
		if err := launch.ValidateSandbox(name, provider.Sandbox); err != nil {
			return err
		}
	}
	safe := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	for _, t := range p.Tasks {
		if !safe.MatchString(t.ID) || ids[t.ID] {
			return errors.New("task IDs must be unique simple names")
		}
		ids[t.ID] = true
		if strings.TrimSpace(t.Prompt) == "" || len(t.Requires) == 0 {
			return fmt.Errorf("%s needs a prompt and required output fields", t.ID)
		}
		if t.ArtifactContracts != nil && p.APIVersion != launch.ArtifactPlanVersion {
			return errors.New("artifact_contracts requires swarm/v6")
		}
		if err := artifactstore.ValidateArtifactContracts(t); err != nil {
			return err
		}
		if len(t.ArtifactContracts) > 0 && len(t.FileFields) > 0 {
			return errors.New("typed artifacts cannot also use legacy file_fields")
		}
		for key, value := range t.Equals {
			if contract, ok := t.ArtifactContracts[key]; ok {
				canonical, err := artifactstore.ArtifactValue(contract, value)
				if err != nil || canonical != value {
					return errors.New("typed equals value violates artifact contract")
				}
			}
		}
		fields := map[string]bool{}
		for _, key := range t.Requires {
			if strings.TrimSpace(key) == "" || fields[key] {
				return errors.New("required fields must be nonempty and unique")
			}
			fields[key] = true
		}
		fileFields := map[string]bool{}
		for _, key := range t.FileFields {
			if !fields[key] || fileFields[key] {
				return fmt.Errorf("file field %q must be required and unique", key)
			}
			fileFields[key] = true
		}
		if err := model.ValidateTaskDisplay(t, fields, fileFields); err != nil {
			return err
		}
		for key := range t.Equals {
			if !fields[key] {
				return fmt.Errorf("equals field %s must be required", key)
			}
		}
		if t.Spawn != nil {
			if !fields[t.Spawn.Field] || t.Spawn.MaxChildren < 0 || t.Spawn.MaxChildren > 64 {
				return fmt.Errorf("%s has invalid spawn policy", t.ID)
			}
		}
		if t.Provider != "" && t.Provider != "haiku" && t.Provider != "mock" {
			if _, ok := p.Providers[t.Provider]; !ok {
				return fmt.Errorf("unknown provider %s", t.Provider)
			}
		}
		if t.Effort != "" && t.Effort != "low" && t.Effort != "medium" && t.Effort != "high" {
			return fmt.Errorf("unsupported effort %s", t.Effort)
		}
		for _, key := range t.Context {
			if _, ok := p.Context[key]; !ok {
				return fmt.Errorf("unknown context %s", key)
			}
		}
		for field, path := range t.Outputs {
			if !fields[field] || !filepath.IsLocal(path) || path == "." || strings.Contains(path, "\\") {
				return fmt.Errorf("invalid output mapping %s=%s", field, path)
			}
			path = strings.ToLower(filepath.Clean(path))
			if path == "swarm-export.json" || strings.HasPrefix(path, "swarm-export.json/") {
				return errors.New("reserved export path")
			}
			if outputs[path] {
				return fmt.Errorf("duplicate output path %s", path)
			}
			outputs[path] = true
		}
		for _, c := range t.Checks {
			switch c.Kind {
			case "contains", "min_length", "go_source":
				if !fields[c.Field] {
					return fmt.Errorf("check field %s must be required", c.Field)
				}
				if c.Kind == "contains" && c.Value == "" {
					return errors.New("contains check needs value")
				}
				if c.Kind == "min_length" && c.Minimum < 1 {
					return errors.New("minimum must be positive")
				}
			case "command":
				if len(c.Command) == 0 || c.Command[0] == "" {
					return errors.New("command validator needs argv")
				}
				if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 300 {
					return errors.New("validator timeout must be between 0 and 300 seconds")
				}
			default:
				return fmt.Errorf("unknown check kind %s", c.Kind)
			}
		}
	}
	for _, t := range p.Tasks {
		if err := launch.ValidateInputFields(p, t); err != nil {
			return err
		}
		for _, d := range t.DependsOn {
			if !ids[d] {
				return fmt.Errorf("unknown dependency %s", d)
			}
		}
	}
	for path := range outputs {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			if outputs[parent] {
				return errors.New("output file and directory paths collide")
			}
		}
	}
	seen := map[string]bool{}
	for len(seen) < len(p.Tasks) {
		before := len(seen)
		for _, t := range p.Tasks {
			ready := true
			for _, d := range t.DependsOn {
				ready = ready && seen[d]
			}
			if ready {
				seen[t.ID] = true
			}
		}
		if len(seen) == before {
			blocked := make([]string, 0)
			for _, task := range p.Tasks {
				if !seen[task.ID] {
					blocked = append(blocked, task.ID)
				}
			}
			return fmt.Errorf("dependency cycle; unresolved nodes: %s", strings.Join(blocked, ", "))
		}
	}
	return nil
}
func LoadPlan(path string) (model.Plan, error) {
	return LoadPlanWithContext(path, nil)
}

func LoadPlanWithContext(path string, imports []string) (p model.Plan, err error) {
	return loadPlanAdmissionContext(path, imports, false)
}

// Only campaign admit may load an unbound channel plan. Standalone plan/apply
// and all other callers retain the ordinary campaign-binding requirement.
func LoadCampaignPlan(path string) (model.Plan, error) {
	return loadPlanAdmissionContext(path, nil, true)
}

func loadPlanAdmissionContext(path string, imports []string, campaignAdmission bool) (p model.Plan, err error) {
	defer func() {
		if err != nil {
			source := path
			if source == "-" {
				source = "stdin"
			}
			err = fmt.Errorf("plan %s: %w", source, err)
		}
	}()
	var input io.Reader
	if path == "-" {
		input = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return p, err
		}
		defer f.Close()
		input = f
	}
	data, err := io.ReadAll(io.LimitReader(input, 8*1024*1024+1))
	if err != nil {
		return p, err
	}
	if len(data) > 8*1024*1024 {
		return p, errors.New("plan exceeds 8 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		var syntax *json.SyntaxError
		var mismatch *json.UnmarshalTypeError
		offset := int64(0)
		switch {
		case errors.As(err, &syntax):
			offset = syntax.Offset
		case errors.As(err, &mismatch):
			offset = mismatch.Offset
		case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			offset = int64(len(data)) + 1
		}
		if offset > 0 {
			end := int(offset - 1)
			if end > len(data) {
				end = len(data)
			}
			line := bytes.Count(data[:end], []byte{'\n'}) + 1
			column := end - bytes.LastIndexByte(data[:end], '\n')
			return p, fmt.Errorf("line %d, column %d: %w", line, column, err)
		}
		return p, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return p, errors.New("plan must contain exactly one JSON object")
	}
	if launch.IsManagedPlanVersion(p.APIVersion) {
		if err := validateManagedPlanJSON(data); err != nil {
			return p, err
		}
	}
	for _, t := range p.Tasks {
		if t.Parent != "" {
			return p, errors.New("parent is assigned by runtime expansion")
		}
	}
	if err := MaterializePlanContext(&p, imports); err != nil {
		return p, err
	}
	launch.ResolveSandboxPaths(&p, path)
	if err := ValidateChannelPublishAdmission(p); err != nil {
		return p, err
	}
	if err := ValidatePlanContext(p, campaignAdmission); err != nil {
		return p, err
	}
	if err := ValidateInitialTurns(p); err != nil {
		return p, err
	}
	// Observer metadata is checked only at initial load, never on revisions.
	return p, ValidateObserverShape(p)
}
func Initialize(p model.Plan, root, backend string, options ...model.Options) error {
	if err := ValidateChannelPublishAdmission(p); err != nil {
		return err
	}
	if err := ValidateCampaignAuthority(root, p); err != nil {
		return err
	}
	if err := RejectQuarantinedRun(root); err != nil {
		return err
	}
	if err := ValidateInitialTurns(p); err != nil {
		return err
	}
	for _, t := range p.Tasks {
		if t.Parent != "" {
			return errors.New("plan roots cannot have owners")
		}
	}
	if err := Validate(p); err != nil {
		return err
	}
	if err := ValidateObserverShape(p); err != nil {
		return err
	}
	if launch.IsManagedPlanVersion(p.APIVersion) && backend != "managed" {
		return errors.New("managed plan requires managed backend")
	}
	if !launch.IsManagedPlanVersion(p.APIVersion) && backend == "managed" {
		return errors.New("managed backend requires swarm/v3 or swarm/v4")
	}
	if backend != "haiku" && backend != "mock" && backend != "managed" {
		if _, ok := p.Providers[backend]; !ok {
			return errors.New("backend must be haiku, mock, or a named provider")
		}
	}
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return err
	}
	initLock, err := FileLock(root+".init.lock", false)
	if err != nil {
		return err
	}
	defer Unlock(initLock)
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		return fmt.Errorf("run already exists: %s", root)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(root), ".swarm-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	encoded, _ := json.Marshal(p)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	s := &model.State{SchemaVersion: model.CurrentSchemaVersion, Version: 1, Plan: p, PlanHash: fmt.Sprintf("%x", sha256.Sum256(encoded)),
		RunID: fmt.Sprintf("%x", id), Backend: backend, Control: "ready", Tasks: map[string]*model.TaskState{}}
	if len(options) > 0 {
		model.SaveCallLimits(s, options[0])
		if err := model.ValidateCallLimits(s.CallLimits); err != nil {
			return err
		}
	} else if !launch.IsManagedPlanVersion(p.APIVersion) && PotentiallyPaid(backend, p.Tasks) {
		// Created by this code without explicit call options: there is no legacy
		// ambiguity to resolve, so record that for the unbudgeted-run gate.
		// Truly free mock runs and managed profile-budget runs are exempt already.
		s.UnbudgetedDecision = UnbudgetedInitial
	}
	if launch.IsManagedPlanVersion(p.APIVersion) {
		s.Version = 2
		if launch.SupportsUnknownCost(p.APIVersion) {
			s.Version = 3
		}
		if launch.SupportsGovernedPlan(p.APIVersion) {
			s.Version = governedCheckpointVersion(p.APIVersion)
			s.Mailbox = NewMailboxState()
		}
		var err error
		s.Execution, err = InitializeManagedExecution(p)
		if err != nil {
			return err
		}
	}
	for _, t := range p.Tasks {
		if err := os.MkdirAll(filepath.Join(tmp, "cells", t.ID), 0700); err != nil {
			return err
		}
		s.Tasks[t.ID] = s.NewTask(t)
	}
	s.Event("created", "", map[string]string{"backend": backend})
	if err := jsonio.AtomicJSON(filepath.Join(tmp, "state.json"), s); err != nil {
		return err
	}
	if err := os.Rename(tmp, root); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(root))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
