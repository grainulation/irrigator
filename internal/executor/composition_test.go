package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/composition"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func compositionFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
	template := composition.CompositionTemplate{Schema: "irrigator.workflow/v1", Version: "1", Inputs: map[string]string{"question": "string"}, Nodes: []composition.CompositionNode{{ID: "answer", Prompt: "Answer using the provided question.", Profile: "small", Inputs: map[string]composition.CompositionRef{"question": {Input: "question"}}, Outputs: map[string]string{"answer": "string"}}}, Exports: map[string]composition.CompositionRef{"answer": {Node: "answer", Port: "answer"}}}
	raw, _ := json.Marshal(template)
	if err := os.WriteFile(filepath.Join(dir, "template.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	pin := composition.CompositionPin{Path: "template.json", Version: "1", SHA256: composition.CompositionDigest(raw)}
	literal := "hello"
	c := composition.CompositionConfig{Schema: "irrigator.composition/v1", Name: "two reusable calls", Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, CostPolicy: "retain_unknown", RunBudgetUSD: "0"}, MaxTasks: 2, MaxInputBytes: 1024, MaxOutputBytes: 1024, Calls: []composition.CompositionCall{{ID: "first", Template: pin, Inputs: map[string]composition.CompositionRef{"question": {Literal: &literal}}}, {ID: "second", Template: pin, Inputs: map[string]composition.CompositionRef{"question": {Call: "first", Port: "answer"}}}}, Exports: map[string]composition.CompositionRef{"final": {Call: "second", Port: "answer"}}}
	b, _ := json.Marshal(c)
	path := filepath.Join(dir, "composition.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestCompositionReusableSubgraph(t *testing.T) {
	path := compositionFixture(t)
	p, receipt, err := composition.CompileComposition(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tasks) != 2 || len(p.Tasks[1].DependsOn) != 1 || p.Tasks[1].DependsOn[0] != "first__answer" || p.Tasks[1].InputFields["first__answer"][0] != "answer" || receipt.Exports["final"].Node != "second__answer" {
		t.Fatalf("lost workflow wiring: %+v %+v", p, receipt)
	}
	out := filepath.Join(filepath.Dir(path), "plan.json")
	if code, err := composition.CompositionCLI([]string{path, "--out", out}); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	before, _ := os.ReadFile(out)
	if _, err := composition.CompositionCLI([]string{path, "--out", out}); err == nil {
		t.Fatal("overwrote existing plan")
	}
	after, _ := os.ReadFile(out)
	if string(before) != string(after) {
		t.Fatal("changed immutable output")
	}
}
func TestCompositionRejectsDuplicateJSONAndTemplateEscape(t *testing.T) {
	var c composition.CompositionConfig
	if _, err := composition.CompositionRead(strings.NewReader(`{"schema":"a","schema":"b"}`), &c); err == nil {
		t.Fatal("duplicate JSON accepted")
	}
	path := compositionFixture(t)
	dir := filepath.Dir(path)
	outside := filepath.Join(t.TempDir(), "outside.json")
	raw, err := os.ReadFile(filepath.Join(dir, "template.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(outside, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "template.json")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "template.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = composition.CompileComposition(path); err == nil {
		t.Fatal("symlink escaped template root")
	}
}

func typedCompositionFixture(t *testing.T) string {
	t.Helper()
	path := compositionFixture(t)
	var config composition.CompositionConfig
	raw, _ := os.ReadFile(path)
	json.Unmarshal(raw, &config)
	config.Schema = "irrigator.composition/v2"
	config.MaxInputBytes = 8192
	profile := config.Execution.Catalog.Profiles["small"]
	profile.Limits.InputBytes = 8192
	config.Execution.Catalog.Profiles["small"] = profile
	report := runfixture.ReportContract()
	template := composition.CompositionTemplate{Schema: "irrigator.workflow/v2", Version: "2", InputContracts: map[string]model.ArtifactContract{"report": report}, Nodes: []composition.CompositionNode{{ID: "summarize", Prompt: "Verify readiness from the report and return it.", Profile: "small", Inputs: map[string]composition.CompositionRef{"report": {Input: "report"}}, InputContracts: map[string]model.ArtifactContract{"report": report}, OutputContracts: map[string]model.ArtifactContract{"report": report}}}, Exports: map[string]composition.CompositionRef{"report": {Node: "summarize", Port: "report"}}}
	raw, _ = json.Marshal(template)
	os.WriteFile(filepath.Join(filepath.Dir(path), "template.json"), raw, 0600)
	pin := composition.CompositionPin{Path: "template.json", Version: "2", SHA256: composition.CompositionDigest(raw)}
	literal := `{"count":2,"ready":true,"tags":["tested"]}`
	config.Calls = []composition.CompositionCall{{ID: "producer", Template: pin, Inputs: map[string]composition.CompositionRef{"report": {Literal: &literal}}}, {ID: "consumer", Template: pin, Inputs: map[string]composition.CompositionRef{"report": {Call: "producer", Port: "report"}}}}
	config.Exports = map[string]composition.CompositionRef{"final": {Call: "consumer", Port: "report"}}
	raw, _ = json.Marshal(config)
	os.WriteFile(path, raw, 0600)
	return path
}
func TestCompositionTypedObjectConsumer(t *testing.T) {
	path := typedCompositionFixture(t)
	p, _, err := composition.CompileComposition(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.APIVersion != launch.ArtifactPlanVersion {
		t.Fatal("typed plan missing version fence")
	}
	result := map[string]string{"report": `{"count":2,"ready":true,"tags":["tested"]}`}
	if err = artifactstore.Accept(p.Tasks[0], result); err != nil {
		t.Fatal(err)
	}
	s := &model.State{Tasks: map[string]*model.TaskState{p.Tasks[0].ID: {Spec: p.Tasks[0], Status: "verified", Result: result}}}
	inputs, err := managed.InputsForTask(s, p.Tasks[1])
	if err != nil {
		t.Fatal(err)
	}
	if inputs[p.Tasks[0].ID]["report"] != result["report"] {
		t.Fatal("typed object did not reach consumer")
	}
	if err = artifactstore.Accept(p.Tasks[1], inputs[p.Tasks[0].ID]); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{"report": `{"count":"bad","ready":true,"tags":[]}`}
	if artifactstore.Accept(p.Tasks[0], bad) == nil {
		t.Fatal("wrong typed output accepted")
	}
	s.Tasks[p.Tasks[0].ID].Status = "failed"
	if _, err = managed.InputsForTask(s, p.Tasks[1]); err == nil {
		t.Fatal("failed output reached consumer")
	}
	p2, _, err := composition.CompileComposition(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(p2)
	if string(a) != string(b) {
		t.Fatal("nondeterministic compile")
	}
}
func TestCompositionTypedRejectsBadLiteralAndPort(t *testing.T) {
	for _, kind := range []string{"literal", "port", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			path := typedCompositionFixture(t)
			var config composition.CompositionConfig
			raw, _ := os.ReadFile(path)
			json.Unmarshal(raw, &config)
			if kind == "literal" {
				bad := `{"count":"bad","ready":true,"tags":[]}`
				config.Calls[0].Inputs["report"] = composition.CompositionRef{Literal: &bad}
			}
			if kind == "legacy" {
				config.Schema = "irrigator.composition/v1"
			}
			if kind == "port" {
				var template composition.CompositionTemplate
				raw, _ = os.ReadFile(filepath.Join(filepath.Dir(path), "template.json"))
				json.Unmarshal(raw, &template)
				template.Nodes[0].OutputContracts["report"] = model.ArtifactContract{Type: "boolean", MaxBytes: 5}
				raw, _ = json.Marshal(template)
				os.WriteFile(filepath.Join(filepath.Dir(path), "template.json"), raw, 0600)
				for i := range config.Calls {
					config.Calls[i].Template.SHA256 = composition.CompositionDigest(raw)
				}
			}
			raw, _ = json.Marshal(config)
			os.WriteFile(path, raw, 0600)
			if _, _, err := composition.CompileComposition(path); err == nil {
				t.Fatal("invalid typed composition accepted")
			}
		})
	}
}

func TestCompositionTypedManagedWorkflowExecutes(t *testing.T) {
	p, _, err := composition.CompileComposition(typedCompositionFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	report := `{"count":2,"ready":true,"tags":["tested"]}`
	for i := range p.Tasks {
		p.Tasks[i].MockResult = map[string]string{"report": report}
	}
	root := filepath.Join(t.TempDir(), "typed-run")
	if err = checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err = checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	for _, task := range p.Tasks {
		if err = worker(root, task.ID, model.Options{Timeout: time.Second * 5}); err != nil {
			t.Fatal(err)
		}
	}
	state := runfixture.Read(t, root)
	for _, task := range p.Tasks {
		row := state.Tasks[task.ID]
		if row.Status != "verified" || row.Result["report"] != report {
			t.Fatalf("typed task failed: %+v", row)
		}
	}
	inputs, err := managed.InputsForTask(state, p.Tasks[1])
	if err != nil || inputs[p.Tasks[0].ID]["report"] != report {
		t.Fatalf("typed downstream payload: %v %v", inputs, err)
	}
}

func TestCompositionTypedCycleAndAuthority(t *testing.T) {
	for _, kind := range []string{"cycle", "byte_cap", "profile", "template_input"} {
		t.Run(kind, func(t *testing.T) {
			path := typedCompositionFixture(t)
			var config composition.CompositionConfig
			raw, _ := os.ReadFile(path)
			json.Unmarshal(raw, &config)
			switch kind {
			case "cycle":
				config.Calls[0].Inputs["report"] = composition.CompositionRef{Call: "consumer", Port: "report"}
			case "byte_cap":
				config.MaxInputBytes = 8
			case "profile":
				config.Execution.AllowedProfiles = []string{"forbidden"}
			case "template_input":
				var template composition.CompositionTemplate
				raw, _ = os.ReadFile(filepath.Join(filepath.Dir(path), "template.json"))
				json.Unmarshal(raw, &template)
				wider := template.InputContracts["report"]
				wider.MaxBytes = 1024
				template.InputContracts["report"] = wider
				raw, _ = json.Marshal(template)
				os.WriteFile(filepath.Join(filepath.Dir(path), "template.json"), raw, 0600)
				for i := range config.Calls {
					config.Calls[i].Template.SHA256 = composition.CompositionDigest(raw)
				}
			}
			raw, _ = json.Marshal(config)
			os.WriteFile(path, raw, 0600)
			if _, _, err := composition.CompileComposition(path); err == nil {
				t.Fatal("invalid composition accepted")
			}
		})
	}
}

func TestCompositionTypedPlanVersionFence(t *testing.T) {
	p, _, err := composition.CompileComposition(typedCompositionFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", "swarm/v1", "swarm/v2", "swarm/v3", "swarm/v4", "swarm/v5"} {
		p.APIVersion = version
		if checkpoint.Validate(p) == nil {
			t.Errorf("typed contracts admitted under %q", version)
		}
	}
}
