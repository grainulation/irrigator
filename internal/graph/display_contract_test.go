package graph

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestPublicDisplaySpawnPersistence(t *testing.T) {
	p := displayContractPlan(t, nil)
	child := p.Tasks[0]
	p.Tasks = []model.Task{runfixture.SpawnTask(t, "parent", child)}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(p, root, "mock"); err != nil {
		t.Fatal(err)
	}
	runfixture.StageExpansion(t, root, "parent")
	for i := 0; i < 2; i++ {
		if err := commitExpansion(root, "parent", "structural"); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["parent__review"].Result = map[string]string{"public_summary": strings.Repeat("界", 200), "private": "private-result-marker"}
		s.Tasks["parent__review"].Status = "produced"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	recovered := runfixture.Read(t, root)
	if len(recovered.Tasks) != 2 || recovered.Tasks["parent__review"].Spec.Parent != "parent" {
		t.Fatal("idempotent expansion lost child ownership")
	}
	if d := recovered.Tasks["parent__review"].Spec.Display; d == nil || d.Title != "Review tests" || d.OutcomeField != "public_summary" {
		t.Fatalf("spawn display metadata lost after reload: %+v", d)
	}
}

func displayContractPlan(t *testing.T, modify func(map[string]any, map[string]any)) model.Plan {
	t.Helper()
	task := map[string]any{
		"id": "review", "prompt": "private prompt", "requires": []string{"public_summary"},
		"display": map[string]any{"title": "Review tests", "why_spawned": "Independent verification", "done_when": "Tests checked", "outcome_field": "public_summary"},
	}
	plan := map[string]any{
		"name": "legacy name", "display": map[string]any{"version": 1, "publish": true, "title": "Release review", "purpose": "Assess the release"},
		"tasks": []any{task},
	}
	if modify != nil {
		modify(plan, task)
	}
	b, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var p model.Plan
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPublicDisplayContractValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any, map[string]any)
		ok   bool
	}{
		{"valid", nil, true},
		{"unicode limits", func(p, n map[string]any) {
			p["display"].(map[string]any)["title"] = strings.Repeat("λ", 60)
			p["display"].(map[string]any)["purpose"] = strings.Repeat("界", 140)
			n["display"].(map[string]any)["title"] = strings.Repeat("λ", 48)
			n["display"].(map[string]any)["why_spawned"] = strings.Repeat("界", 100)
			n["display"].(map[string]any)["done_when"] = strings.Repeat("λ", 140)
		}, true},
		{"unknown version", func(p, n map[string]any) { p["display"].(map[string]any)["version"] = 999 }, false},
		{"run title overflow", func(p, n map[string]any) { p["display"].(map[string]any)["title"] = strings.Repeat("λ", 61) }, false},
		{"purpose overflow", func(p, n map[string]any) { p["display"].(map[string]any)["purpose"] = strings.Repeat("界", 141) }, false},
		{"task title overflow", func(p, n map[string]any) { n["display"].(map[string]any)["title"] = strings.Repeat("λ", 49) }, false},
		{"spawn reason overflow", func(p, n map[string]any) { n["display"].(map[string]any)["why_spawned"] = strings.Repeat("界", 101) }, false},
		{"completion condition overflow", func(p, n map[string]any) { n["display"].(map[string]any)["done_when"] = strings.Repeat("λ", 141) }, false},
		{"outcome must be required", func(p, n map[string]any) { n["display"].(map[string]any)["outcome_field"] = "private_artifact" }, false},
		{"outcome cannot be files", func(p, n map[string]any) { n["file_fields"] = []string{"public_summary"} }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkpoint.Validate(displayContractPlan(t, tc.edit))
			if (err == nil) != tc.ok {
				t.Fatalf("valid=%v wanted %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestPublicDisplayOmissionPreservesLegacyEncoding(t *testing.T) {
	const legacy = `{"name":"legacy","tasks":[{"id":"a","prompt":"work","requires":["answer"]}]}`
	var p model.Plan
	if err := json.Unmarshal([]byte(legacy), &p); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != legacy {
		t.Fatalf("absent metadata changed legacy plan bytes: %s", b)
	}
}
