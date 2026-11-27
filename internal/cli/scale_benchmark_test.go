package cli

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func TestScaleFixtureAdmitsTenThousandBoundedTasksWithoutModelCalls(t *testing.T) {
	for _, mode := range []string{"mock", "ollama"} {
		for _, count := range []int{900, 2000, model.MaxGraphTasks} {
			t.Run(fmt.Sprintf("%s/%d", mode, count), func(t *testing.T) {
				c := scaleBenchmarkConfig{Tasks: count, Workers: 6, Mode: mode, OutputTokens: 32, Timeout: time.Second}
				if mode == "ollama" {
					c.Endpoint, c.Model, c.Digest = "http://127.0.0.1:11434", "fixture:latest", strings.Repeat("a", 64)
				}
				p, err := scaleBenchmarkFixture(c)
				if err != nil || len(p.Tasks) != count || p.MaxTasks != count {
					t.Fatalf("fixture admission failed: tasks=%d err=%v", len(p.Tasks), err)
				}
				if err := checkpoint.Validate(p); err != nil {
					t.Fatal(err)
				}
				if mode == "ollama" && (p.Execution == nil || p.Execution.Catalog.Profiles["local"].Limits.MaxOutputTokens != 32) {
					t.Fatal("local output bound missing")
				}
			})
		}
	}
}

func TestScaleFixtureRejectsRemotePaidAndExcessiveWork(t *testing.T) {
	base := scaleBenchmarkConfig{Tasks: 1, Workers: 1, Mode: "ollama", Endpoint: "http://127.0.0.1:11434", Model: "fixture", Digest: strings.Repeat("a", 64), OutputTokens: 32, Timeout: time.Second}
	for _, change := range []func(*scaleBenchmarkConfig){
		func(c *scaleBenchmarkConfig) { c.Endpoint = "https://example.com" },
		func(c *scaleBenchmarkConfig) { c.Mode = "haiku" },
		func(c *scaleBenchmarkConfig) { c.Tasks = model.MaxGraphTasks + 1 },
		func(c *scaleBenchmarkConfig) { c.OutputTokens = 513 },
		func(c *scaleBenchmarkConfig) { c.Workers = 129 },
		func(c *scaleBenchmarkConfig) { c.Digest = "unpinned" },
	} {
		c := base
		change(&c)
		if _, err := scaleBenchmarkFixture(c); err == nil {
			t.Fatalf("unsafe config admitted: %+v", c)
		}
	}
}

func TestScaleGradingRejectsReplayAndMissingLocalGeneration(t *testing.T) {
	c := scaleBenchmarkConfig{Tasks: 1, Workers: 1, Mode: "ollama", Endpoint: "http://127.0.0.1:11434", Model: "fixture", Digest: strings.Repeat("a", 64), OutputTokens: 32, Timeout: time.Second}
	p, err := scaleBenchmarkFixture(c)
	if err != nil {
		t.Fatal(err)
	}
	row := &model.TaskState{Spec: p.Tasks[0], Status: "verified", Attempt: 1, Generation: 1, Result: map[string]string{"answer": "ok"}, Usage: &model.TaskUsage{Model: "fixture", InputTokens: 12, OutputTokens: 7}}
	s := &model.State{Control: "complete", Plan: p, Tasks: map[string]*model.TaskState{row.Spec.ID: row}}
	if failures, calls, _, _ := gradeScaleBenchmark(s, c); len(failures) > 0 || calls != 1 {
		t.Fatalf("valid receipt rejected: %v calls=%d", failures, calls)
	}
	for _, change := range []func(){func() { row.Attempt = 2 }, func() { row.Generation = 2 }, func() { row.Usage = nil }, func() { row.Result = map[string]string{"answer": "wrong"} }} {
		row.Attempt, row.Generation, row.Usage, row.Result = 1, 1, &model.TaskUsage{Model: "fixture", InputTokens: 12, OutputTokens: 7}, map[string]string{"answer": "ok"}
		change()
		if failures, _, _, _ := gradeScaleBenchmark(s, c); len(failures) == 0 {
			t.Fatal("invalid or repeated generation accepted")
		}
	}
}
