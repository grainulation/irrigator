package cli

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/grainulation/irrigator/internal/backup"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runops"
)

type scaleBenchmarkConfig struct {
	Dir, Mode, Endpoint, Model, Digest string
	Tasks, Workers                     int
	OutputTokens                       int64
	Timeout                            time.Duration
}

type scaleBenchmarkReport struct {
	Schema               string            `json:"schema"`
	Mode                 string            `json:"mode"`
	Model                string            `json:"model,omitempty"`
	ModelDigest          string            `json:"model_digest,omitempty"`
	Tasks                int               `json:"tasks"`
	Workers              int               `json:"workers"`
	GenerationTokenCap   int64             `json:"generation_token_cap"`
	AggregateTokenCap    int64             `json:"aggregate_output_token_cap"`
	Generations          int               `json:"generation_receipts"`
	InputTokens          int64             `json:"input_tokens"`
	OutputTokens         int64             `json:"output_tokens"`
	RunID                string            `json:"run_id,omitempty"`
	PlanSHA256           string            `json:"plan_sha256,omitempty"`
	TimingsMS            map[string]int64  `json:"timings_ms"`
	StorageCounters      map[string]uint64 `json:"storage_counters"`
	StorageCounterScope  string            `json:"storage_counter_scope"`
	RunFootprintBytes    int64             `json:"run_footprint_bytes"`
	RunFiles             int               `json:"run_files"`
	Passed               bool              `json:"passed"`
	Failures             []string          `json:"failures"`
	ModelQualityMeasured bool              `json:"model_quality_measured"`
	Scope                string            `json:"scope"`
}

func scaleBenchmarkFixture(c scaleBenchmarkConfig) (model.Plan, error) {
	if c.Tasks < 1 || c.Tasks > model.MaxGraphTasks || c.Workers < 1 || c.Workers > 128 || c.Timeout <= 0 || c.Timeout > time.Hour || c.OutputTokens < 1 || c.OutputTokens > launch.OllamaMaxOutputTokens {
		return model.Plan{}, errors.New("scale limits: tasks 1..10000, workers 1..128, timeout (0,1h], output tokens 1..512")
	}
	p := model.Plan{APIVersion: "swarm/v1", Name: "bounded orchestration scale check", MaxTasks: c.Tasks, Tasks: make([]model.Task, c.Tasks)}
	for i := range p.Tasks {
		id := fmt.Sprintf("scale_%05d", i)
		p.Tasks[i] = model.Task{ID: id, Prompt: `Return the JSON object {"answer":"ok"}. Do not add anything else.`, Requires: []string{"answer"}, Equals: map[string]string{"answer": "ok"}}
	}
	switch c.Mode {
	case "mock":
		if c.Endpoint != "" || c.Model != "" || c.Digest != "" {
			return model.Plan{}, errors.New("mock scale checks must not declare model endpoints or identities")
		}
		for i := range p.Tasks {
			p.Tasks[i].MockResult = map[string]string{"answer": "ok"}
		}
	case "ollama":
		if err := launch.ValidateOllamaEndpoint(c.Endpoint); err != nil {
			return model.Plan{}, err
		}
		digest, err := hex.DecodeString(c.Digest)
		if err != nil || len(digest) != 32 || strings.ToLower(c.Digest) != c.Digest || !launch.ExecutionModel.MatchString(c.Model) {
			return model.Plan{}, errors.New("local scale checks require an explicit model tag and lowercase 64-character digest")
		}
		p.APIVersion = launch.ArtifactPlanVersion
		p.TurnShape = &model.TurnShape{Roots: c.Tasks, MainTurns: 1}
		catalog := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema,
			Adapters:  map[string]model.ExecutionAdapter{"local": {Kind: "ollama_http", Protocol: model.ProtocolVersion, Endpoint: c.Endpoint}},
			Instances: map[string]model.ExecutionInstance{"local": {Adapter: "local", CredentialRefs: []model.ExecutionCredentialRef{}}},
			Profiles:  map[string]model.ExecutionProfile{"local": {Instance: "local", Model: c.Model, ModelDigest: c.Digest, Effort: "none", RequiredCapabilities: []string{"outcome_certainty", "structured_strings"}, Limits: model.ExecutionLimits{InputBytes: 8192, OutputBytes: 16384, MaxOutputTokens: c.OutputTokens}, Tools: []string{}, OperationBudgetUSD: "0.01"}}, DefaultProfile: "local"}
		p.Execution = &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"local"}, CostPolicy: "track", RunBudgetUSD: "0"}
		for i := range p.Tasks {
			p.Tasks[i].Profile = "local"
			p.Tasks[i].Turn = &model.TurnIdentity{AgentID: p.Tasks[i].ID, Role: "main", Index: 1}
		}
	default:
		return model.Plan{}, errors.New("scale mode must be mock or ollama; no paid or remote route is supported")
	}
	return p, checkpoint.Validate(p)
}

func gradeScaleBenchmark(s *model.State, c scaleBenchmarkConfig) (failures []string, generations int, input, output int64) {
	failures = []string{}
	if s.Control != "complete" || len(s.Tasks) != c.Tasks {
		failures = append(failures, "run_not_complete_or_wrong_task_count")
	}
	for _, spec := range s.Plan.Tasks {
		row := s.Tasks[spec.ID]
		if row == nil || row.Status != "verified" || row.Attempt != 1 || row.Generation != 1 || row.Result["answer"] != "ok" {
			if len(failures) < 20 {
				failures = append(failures, "wrong_unverified_or_repeated:"+spec.ID)
			}
			continue
		}
		if c.Mode == "ollama" {
			if row.Usage == nil || row.Usage.Model != c.Model || row.Usage.OutputTokens < 1 || row.Usage.OutputTokens > c.OutputTokens {
				if len(failures) < 20 {
					failures = append(failures, "invalid_local_generation_usage:"+spec.ID)
				}
				continue
			}
			generations++
			input += row.Usage.InputTokens
			output += row.Usage.OutputTokens
		}
	}
	if c.Mode == "ollama" && generations != c.Tasks {
		failures = append(failures, "not_one_local_generation_receipt_per_task")
	}
	return
}

func scaleBenchmarkCLI(c scaleBenchmarkConfig) (int, error) {
	beforeStorage := checkpoint.StorageMetrics()
	start := time.Now()
	plan, err := scaleBenchmarkFixture(c)
	if err != nil {
		return 2, err
	}
	report := scaleBenchmarkReport{Schema: "irrigator.scale-benchmark/v1", Mode: c.Mode, Model: c.Model, ModelDigest: c.Digest, Tasks: c.Tasks, Workers: c.Workers, GenerationTokenCap: c.OutputTokens, AggregateTokenCap: int64(c.Tasks) * c.OutputTokens, TimingsMS: map[string]int64{"validate": time.Since(start).Milliseconds()}, Failures: []string{}, Scope: "real execution engine, accepted task output, one attempt and generation per task, restart, run lookup and backup/restore; mock and local model modes are distinct; no paid calls or model-quality comparison"}
	if c.Mode == "mock" {
		report.GenerationTokenCap, report.AggregateTokenCap = 0, 0
	}
	root, err := filepath.Abs(c.Dir)
	if err != nil {
		return 1, err
	}
	if err = os.Mkdir(root, 0700); err != nil {
		return 1, err
	}
	finish := func(runErr error) (int, error) {
		report.StorageCounters = map[string]uint64{}
		for key, after := range checkpoint.StorageMetrics() {
			report.StorageCounters[key] = after - beforeStorage[key]
		}
		report.StorageCounterScope = "application counters in the benchmark/controller process only; excludes re-executed workers and physical disk traffic; includes lifecycle verification"
		if runErr != nil {
			report.Failures = append(report.Failures, runErr.Error())
		}
		report.Passed = len(report.Failures) == 0
		raw, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr == nil {
			marshalErr = localfs.WriteNew(filepath.Join(root, "scale-report.json"), append(raw, '\n'))
		}
		if marshalErr != nil {
			return 1, marshalErr
		}
		if !report.Passed {
			return 1, errors.New("scale checks failed; inspect scale-report.json")
		}
		fmt.Printf("Scale benchmark: PASS tasks=%d mode=%s generations=%d report=%s\n", c.Tasks, c.Mode, report.Generations, filepath.Join(root, "scale-report.json"))
		return 0, nil
	}
	raw, _ := json.Marshal(plan)
	if err = localfs.WriteNew(filepath.Join(root, "plan.json"), append(raw, '\n')); err != nil {
		return finish(err)
	}
	if err = os.Mkdir(filepath.Join(root, "runs"), 0700); err != nil {
		return finish(err)
	}
	run := filepath.Join(root, "runs", "scale")
	backend := "mock"
	if c.Mode == "ollama" {
		backend = "managed"
	}
	opts := model.Options{Workers: c.Workers, Timeout: c.Timeout, Budget: .01}
	start = time.Now()
	if err = checkpoint.Initialize(plan, run, backend, opts); err != nil {
		return finish(err)
	}
	report.TimingsMS["initialize"] = time.Since(start).Milliseconds()
	start = time.Now()
	code, err := runops.Run(run, opts)
	report.TimingsMS["execute"] = time.Since(start).Milliseconds()
	if err != nil || code != 0 {
		return finish(fmt.Errorf("execution failed: exit=%d error=%v", code, err))
	}
	start = time.Now()
	s, err := checkpoint.ReadState(run)
	report.TimingsMS["read_completed"] = time.Since(start).Milliseconds()
	if err != nil {
		return finish(err)
	}
	report.RunID, report.PlanSHA256 = s.RunID, s.PlanHash
	report.Failures, report.Generations, report.InputTokens, report.OutputTokens = gradeScaleBenchmark(s, c)
	if len(report.Failures) != 0 {
		return finish(nil)
	}
	// Restart from the durable completed state. Verified work must not dispatch again.
	start = time.Now()
	code, err = runops.Run(run, opts)
	report.TimingsMS["resume_completed"] = time.Since(start).Milliseconds()
	if err != nil || code != 0 {
		return finish(fmt.Errorf("restart failed: exit=%d error=%v", code, err))
	}
	s, err = checkpoint.ReadState(run)
	if err != nil {
		return finish(err)
	}
	again, generations, _, output := gradeScaleBenchmark(s, c)
	if len(again) > 0 || generations != report.Generations || output != report.OutputTokens {
		return finish(errors.New("restart repeated or changed accepted work"))
	}
	binary, err := os.Executable()
	if err != nil {
		return finish(err)
	}
	start = time.Now()
	lookup := exec.Command(binary, "status", report.RunID[:8])
	lookup.Dir = root
	if err := lookup.Run(); err != nil {
		return finish(fmt.Errorf("run-ID lookup failed: %w", err))
	}
	report.TimingsMS["run_id_lookup"] = time.Since(start).Milliseconds()
	start = time.Now()
	bundle := filepath.Join(root, "backup.json")
	if err = backup.BackupCreate(run, bundle); err != nil {
		return finish(fmt.Errorf("backup: %w", err))
	}
	if _, err = backup.BackupRead(bundle); err != nil {
		return finish(fmt.Errorf("backup verification: %w", err))
	}
	report.TimingsMS["backup"] = time.Since(start).Milliseconds()
	start = time.Now()
	if err = backup.BackupRestore(bundle, filepath.Join(root, "restored")); err != nil {
		return finish(fmt.Errorf("restore: %w", err))
	}
	restored, err := checkpoint.ReadState(filepath.Join(root, "restored", "evidence"))
	if err != nil {
		return finish(fmt.Errorf("restored read: %w", err))
	}
	if restored.PlanHash != report.PlanSHA256 || len(restored.Tasks) != c.Tasks {
		return finish(errors.New("restore changed run evidence"))
	}
	restoredFailures, restoredGenerations, _, restoredOutput := gradeScaleBenchmark(restored, c)
	if len(restoredFailures) != 0 || restoredGenerations != report.Generations || restoredOutput != report.OutputTokens || restored.RunID != report.RunID {
		return finish(errors.New("restore changed accepted task results or generation evidence"))
	}
	report.TimingsMS["restore_read"] = time.Since(start).Milliseconds()
	if err := filepath.WalkDir(run, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			report.RunFiles++
			report.RunFootprintBytes += info.Size()
		}
		return nil
	}); err != nil {
		return finish(err)
	}
	return finish(nil)
}
