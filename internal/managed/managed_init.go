package managed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
)

// managedInitialPlan inspects a local executable but never launches it, probes
// authentication, copies credentials, or grants a new executable to an old run.
func managedInitialPlan(selector string) (model.Plan, error) {
	var empty model.Plan
	provider, modelName, ok := strings.Cut(selector, ":")
	if !ok || (provider != "claude" && provider != "codex" && provider != "agy") || !launch.ExecutionModel.MatchString(modelName) {
		return empty, errors.New("--profile must be claude:MODEL, codex:MODEL or agy:MODEL")
	}
	found, err := exec.LookPath(provider)
	if err != nil {
		return empty, fmt.Errorf("%s executable not found on PATH; install and authenticate that CLI first: %w", provider, err)
	}
	executable, err := filepath.EvalSymlinks(found)
	if err != nil {
		return empty, err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return empty, err
	}
	f, err := launch.OpenContextFile(executable, os.O_RDONLY, 0)
	if err != nil {
		return empty, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return empty, errors.New("provider must resolve to an executable regular file")
	}
	if info.Size() > launch.ExecutionExecutableLimit {
		return empty, errors.New("provider executable exceeds 512 MiB limit")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, launch.ExecutionExecutableLimit+1))
	if err != nil {
		return empty, err
	}
	if n > launch.ExecutionExecutableLimit {
		return empty, errors.New("provider executable exceeds 512 MiB limit")
	}
	profile := model.ExecutionProfile{Instance: "local", Model: modelName, Effort: "low", RequiredCapabilities: []string{"structured_strings", "outcome_certainty"}, Limits: model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}, Tools: []string{}, OperationBudgetUSD: "0.25"}
	if provider == "claude" && (strings.Contains(modelName, "sonnet") || strings.Contains(modelName, "opus")) {
		profile.TimeoutSeconds = 600
	}
	catalog := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{provider: {Kind: provider + "_cli", Protocol: model.ProtocolVersion, Command: []string{executable}, ExecutableSHA256: hex.EncodeToString(hash.Sum(nil))}}, Instances: map[string]model.ExecutionInstance{"local": {Adapter: provider, CredentialRefs: []model.ExecutionCredentialRef{{Kind: "host_session", Name: provider}}}}, Profiles: map[string]model.ExecutionProfile{"default": profile}, DefaultProfile: "default"}
	p := model.Plan{APIVersion: launch.UnknownCostPlanVersion, Name: "First managed Irrigator run", MaxTasks: 1, TurnShape: &model.TurnShape{Roots: 1, MainTurns: 1, HelpersPerTurn: 0}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"default"}, CostPolicy: "track", RunBudgetUSD: "0.25"}, Tasks: []model.Task{{ID: "ready", Profile: "default", Prompt: "Return the word ready in answer.", Requires: []string{"answer"}, Equals: map[string]string{"answer": "ready"}, Turn: &model.TurnIdentity{AgentID: "ready", Role: "main", Index: 1}}}}
	if err := checkpoint.Validate(p); err != nil {
		return empty, err
	}
	// Re-read through normal binding verification; a changed executable cannot
	// silently slip between scaffold hashing and plan publication.
	if _, err := launch.ResolveExecutionCatalog(catalog, "inline", "default"); err != nil {
		return empty, err
	}
	return p, nil
}

func WriteManagedInitialPlan(out, selector string) error {
	p, err := managedInitialPlan(selector)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := localfs.WriteNew(out, append(data, '\n')); err != nil {
		return err
	}
	fmt.Printf("Created %s for %s. Cost tracking only; warning threshold USD %s; provider per-operation limit USD %s where supported. No provider call or authentication check performed. Review the plan before apply.\n", out, selector, p.Execution.RunBudgetUSD, p.Execution.Catalog.Profiles["default"].OperationBudgetUSD)
	return nil
}
