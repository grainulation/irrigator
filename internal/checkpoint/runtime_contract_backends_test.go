package checkpoint

import (
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func TestRuntimeHardTokenRequirementUsesActualSelectedProfiles(t *testing.T) {
	template, in := formationFixture(t)
	p, err := compilePackageUpgrade(template, in)
	if err != nil {
		t.Fatal(err)
	}
	p.Execution.Catalog.Adapters["mock"] = model.ExecutionAdapter{Kind: "ollama_http", Protocol: model.ProtocolVersion, Endpoint: "http://127.0.0.1:11434"}
	for name, profile := range p.Execution.Catalog.Profiles {
		profile.Model = "fixture:latest"
		profile.ModelDigest = strings.Repeat("a", 64)
		profile.Effort = "none"
		profile.Limits.MaxOutputTokens = 16
		profile.OperationBudgetUSD = "0.25"
		p.Execution.Catalog.Profiles[name] = profile
	}
	p.Execution.RunBudgetUSD = "1"
	p.Guardrails = &model.RuntimeGuardrails{Schema: launch.RuntimeGuardrailsSchema, Require: []string{"hard_token_limit"}}
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	if !launch.PlanHasHardOutputTokenLimit(p) {
		t.Fatal("explicit capped Ollama profiles unavailable")
	}
	profile := p.Execution.Catalog.Profiles["reviewer"]
	profile.Limits.MaxOutputTokens = 0
	p.Execution.Catalog.Profiles["reviewer"] = profile
	if err := Validate(p); err == nil {
		t.Fatal("uncapped integration profile satisfied explicit cap requirement")
	}
}

func TestRuntimeHostResourceRequirementChecksEffectiveTaskProviders(t *testing.T) {
	p := model.Plan{Name: "bounded local worker", Tasks: []model.Task{{ID: "a", Prompt: "fixture", Requires: []string{"answer"}, Provider: "bounded"}}, Providers: map[string]model.CommandSpec{"bounded": {Command: []string{"fixture-worker"}, Sandbox: &model.SandboxSpec{Runtime: "container", Image: "fixture:local", Resources: &model.SandboxResources{CPUMillis: 1000, MemoryBytes: 256 * 1024 * 1024}}}}, Guardrails: &model.RuntimeGuardrails{Schema: launch.RuntimeGuardrailsSchema, Require: []string{"host_resource_limits"}}}
	if err := Validate(p); err != nil {
		t.Fatal(err)
	}
	// A spawned task cannot silently inherit an uncontained invocation backend.
	s := &model.State{Plan: p, ActiveTasks: append(append([]model.Task{}, p.Tasks...), model.Task{ID: "child", Prompt: "fixture", Requires: []string{"answer"}})}
	if err := launch.AdmitRuntimeGuardrails(s, "child"); err == nil {
		t.Fatal("effective task without bounded provider admitted")
	}
	p.Tasks[0].Provider = ""
	if err := Validate(p); err == nil {
		t.Fatal("implicit backend satisfied host containment requirement")
	}
}
