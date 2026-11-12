//go:build integration

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func contextPlanFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	p := model.Plan{Name: "file context", Tasks: []model.Task{runfixture.Task("review", 0)}}
	p.Tasks[0].Context = []string{"source"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(dir, "template.json")
	if err = os.WriteFile(plan, raw, 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source.go")
	if err = os.WriteFile(source, []byte("package example\n// $HOME stays literal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return plan, source, raw
}

func TestPlanContextImportMaterializesExplicitFiles(t *testing.T) {
	plan, source, before := contextPlanFixture(t)
	out := filepath.Join(filepath.Dir(plan), "materialized.json")
	if _, err := checkpoint.LoadPlan(plan); err == nil {
		t.Fatal("template with unresolved context should not execute")
	}
	code, err := runCommand([]string{"plan", plan, "--context-file", "source=" + source, "--context-file", "notes=" + source, "--out", out})
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	p, err := checkpoint.LoadPlan(out)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := os.ReadFile(source)
	if p.Context["source"] != string(expected) || p.Context["notes"] != string(expected) {
		t.Fatal("context contents changed")
	}
	if len(p.Tasks[0].Context) != 1 || p.Tasks[0].Context[0] != "source" {
		t.Fatal("import changed task scope")
	}
	after, _ := os.ReadFile(plan)
	if !bytes.Equal(before, after) {
		t.Fatal("source plan changed")
	}
	raw, _ := os.ReadFile(out)
	if bytes.Contains(raw, []byte(filepath.Dir(plan))) {
		t.Fatal("local source path leaked into plan")
	}
	if st, err := os.Stat(out); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("plan is not private", err)
	}
	// Snapshot contents no longer depend on the source file.
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.LoadPlan(out); err != nil {
		t.Fatal(err)
	}
}

func TestPlanContextImportRejectsOverwriteAndInvalidTemplate(t *testing.T) {
	plan, source, before := contextPlanFixture(t)
	for _, args := range [][]string{
		{"plan", plan, "--context-file", "source=" + source},
		{"plan", plan, "--context-file", "source=" + source, "--out", plan},
		{"plan", plan, "--context-file", "source=" + source, "--context-file", "source=" + source, "--out", filepath.Join(filepath.Dir(plan), "duplicate.json")},
	} {
		if code, err := runCommand(args); code == 0 || err == nil {
			t.Fatal("accepted invalid import", args)
		}
	}
	after, _ := os.ReadFile(plan)
	if !bytes.Equal(before, after) {
		t.Fatal("source plan overwritten")
	}
	p := model.Plan{Context: map[string]string{"source": "keep"}}
	if err := checkpoint.MaterializePlanContext(&p, []string{"source=" + source}); err == nil || p.Context["source"] != "keep" {
		t.Fatal("existing context overwritten")
	}
	invalid := filepath.Join(filepath.Dir(plan), "invalid.json")
	os.WriteFile(invalid, []byte(`{"name":"bad","unknown":true,"tasks":[]}`), 0600)
	if _, err := checkpoint.LoadPlanWithContext(invalid, []string{"source=" + source}); err == nil {
		t.Fatal("unknown field bypass")
	}
}
