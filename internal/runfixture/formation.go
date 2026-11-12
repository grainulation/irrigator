package runfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

// packageUpgradeInputs describes an experimental, unadmitted formation proposal.
// It supplies existing profile names, never creates execution capabilities.
type packageUpgradeInputs struct {
	dependency, version, BaseCommit   string
	Packages                          []string
	WorkerProfile, integrationProfile string
}

// CompilePackageUpgrade preserves the supplied execution configuration and uses
// the real runtime types/validator. It does no I/O, admission, or provider work.
// Profiles must implement coding/integration externally; prompts are not fences.
func CompilePackageUpgrade(template model.Plan, in packageUpgradeInputs) (model.Plan, error) {
	bad := func(s string) (model.Plan, error) { return model.Plan{}, errors.New(s) }
	if template.Execution == nil {
		return bad("formation requires an explicit execution template")
	}
	if !regexp.MustCompile(`^(@[a-zA-Z0-9_.-]+/)?[a-zA-Z0-9_.-]+$`).MatchString(in.dependency) || strings.HasPrefix(in.dependency, "-") {
		return bad("invalid dependency")
	}
	if !regexp.MustCompile(`^[~^]?[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9_.-]+)?$`).MatchString(in.version) {
		return bad("unsupported version constraint")
	}
	if !regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`).MatchString(in.BaseCommit) {
		return bad("invalid base commit identifier")
	}
	if len(in.Packages) == 0 || len(in.Packages) > 127 {
		return bad("formation requires 1..127 packages")
	}
	for _, profile := range []string{in.WorkerProfile, in.integrationProfile} {
		allowed := false
		for _, name := range template.Execution.AllowedProfiles {
			allowed = allowed || name == profile
		}
		if _, ok := template.Execution.Catalog.Profiles[profile]; !ok || !allowed {
			return bad("formation profile is missing or not allowed")
		}
	}
	packages := append([]string(nil), in.Packages...)
	sort.Strings(packages)
	for i, p := range packages {
		if !regexp.MustCompile(`^[a-zA-Z0-9_.-]+(/[a-zA-Z0-9_.-]+)*$`).MatchString(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.Contains("/"+p+"/", "/.git/") || strings.Contains("/"+p+"/", "/.irrigator/") {
			return bad("invalid canonical package path")
		}
		for _, prev := range packages[:i] {
			if p == prev || strings.HasPrefix(p, prev+"/") {
				return bad("duplicate or overlapping package scopes")
			}
		}
	}
	// Clone so edits to the proposal cannot mutate caller-owned execution config.
	raw, err := json.Marshal(template)
	if err != nil {
		return model.Plan{}, err
	}
	var p model.Plan
	if err = json.Unmarshal(raw, &p); err != nil {
		return model.Plan{}, err
	}
	p.APIVersion = launch.ArtifactPlanVersion
	p.Name = "Upgrade workspace packages"
	p.Tasks = nil
	p.Providers = nil
	p.Experiments = nil
	p.ObserverShape = nil
	p.Display = nil
	p.Campaign = nil
	p.TurnShape = &model.TurnShape{Roots: len(packages) + 1, MainTurns: 1, HelpersPerTurn: 0}
	p.MaxTasks = len(packages) + 1
	p.MaxDepth = 0
	// Do not silently change the supplied cost policy or execution catalog.
	inputs := map[string][]string{}
	var deps []string
	contract := model.ArtifactContract{Type: "string", MaxBytes: 128}
	for _, dir := range packages {
		digest := sha256.Sum256([]byte(dir))
		id := "upgrade_" + hex.EncodeToString(digest[:])
		prompt := fmt.Sprintf("Package upgrade proposal. Base commit %s; package path %q; dependency %q; version %q. Produce candidate_tree_oid as a JSON string. The coding adapter must enforce scope, capture the tree and verify against the requested dependency before accepting output.", in.BaseCommit, dir, in.dependency, in.version)
		p.Tasks = append(p.Tasks, model.Task{ID: id, Prompt: prompt, Profile: in.WorkerProfile, Requires: []string{"candidate_tree_oid"}, ArtifactContracts: map[string]model.ArtifactContract{"candidate_tree_oid": contract}, Turn: &model.TurnIdentity{AgentID: id, Role: "main", Index: 1}})
		deps = append(deps, id)
		inputs[id] = []string{"candidate_tree_oid"}
	}
	p.Tasks = append(p.Tasks, model.Task{ID: "integrate_upgrade", Prompt: fmt.Sprintf("Integration proposal against base %s. Package paths in dependency order: %q; dependency %q at %q. Consume selected worker candidate_tree_oid outputs. The integration adapter must assemble deltas against the common base, reconcile and install dependencies, verify the immutable candidate and emit review_candidate_oid as a JSON string. This prompt itself supplies no integration implementation or application authority.", in.BaseCommit, packages, in.dependency, in.version), Profile: in.integrationProfile, DependsOn: deps, InputFields: inputs, Requires: []string{"review_candidate_oid"}, ArtifactContracts: map[string]model.ArtifactContract{"review_candidate_oid": contract}, Turn: &model.TurnIdentity{AgentID: "integrate_upgrade", Role: "main", Index: 1}})
	if err := checkpoint.Validate(p); err != nil {
		return model.Plan{}, fmt.Errorf("compiled formation: %w", err)
	}
	return p, nil
}

func FormationFixture(t *testing.T) (model.Plan, packageUpgradeInputs) {
	t.Helper()
	p, err := checkpoint.LoadPlan("../../examples/managed-profiles-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	// The checked-in managed fixture supplies the genuine mock execution catalog.
	// v6 requires an explicit policy; no profile, adapter or budget is fabricated.
	p.Execution.CostPolicy = "retain_unknown"
	return p, packageUpgradeInputs{dependency: "fixture-lib", version: "2.0.0", BaseCommit: strings.Repeat("a", 40), Packages: []string{"packages/b", "packages/a"}, WorkerProfile: "writer", integrationProfile: "reviewer"}
}
