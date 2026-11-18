package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/commandflags"
	"github.com/grainulation/irrigator/internal/localfs"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/runops"
)

type benchmarkCell struct {
	Arm        string               `json:"arm"`
	Repetition int                  `json:"repetition"`
	Workers    int                  `json:"workers"`
	WallMS     int64                `json:"wall_ms"`
	Passed     bool                 `json:"held_out_passed"`
	Failures   []string             `json:"failures"`
	ExitCode   int                  `json:"exit_code"`
	Cost       provider.CostSummary `json:"cost"`
	PlanSHA256 string               `json:"plan_sha256"`
	Run        string               `json:"run"`
}

type offlineBenchmark struct {
	Schema               string          `json:"schema"`
	Fixture              string          `json:"fixture"`
	Scope                string          `json:"scope"`
	PaidCalls            int             `json:"paid_calls"`
	ModelQualityMeasured bool            `json:"model_quality_measured"`
	SavingsEstablished   bool            `json:"savings_established"`
	Cells                []benchmarkCell `json:"cells"`
}

func benchmarkFixture() model.Plan {
	tasks := []model.Task{}
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("square_%d", i)
		tasks = append(tasks, model.Task{ID: id, Prompt: fmt.Sprintf("Return the square of %d as answer", i), Requires: []string{"answer"}, MockDelay: .02, MockResult: map[string]string{"answer": fmt.Sprint(i * i)}})
	}
	tasks = append(tasks, model.Task{ID: "join", Prompt: "Confirm all square outputs are present", DependsOn: []string{"square_1", "square_2", "square_3", "square_4"}, Requires: []string{"answer"}, MockResult: map[string]string{"answer": "1,4,9,16"}})
	return model.Plan{APIVersion: "swarm/v1", Name: "offline orchestration benchmark v1", Tasks: tasks}
}

// gradeBenchmark is independent of task-authored equals/check declarations.
// These held-out checks measure fixture correctness through the real runtime.
func gradeBenchmark(s *model.State) []string {
	failures := []string{}
	if s.Control != "complete" {
		failures = append(failures, "run_not_complete")
	}
	expected := map[string]string{"square_1": "1", "square_2": "4", "square_3": "9", "square_4": "16", "join": "1,4,9,16"}
	if len(s.Tasks) != len(expected) {
		failures = append(failures, "unexpected_task_count")
	}
	for _, id := range []string{"square_1", "square_2", "square_3", "square_4", "join"} {
		row := s.Tasks[id]
		if row == nil || row.Status != "verified" || row.Result["answer"] != expected[id] {
			failures = append(failures, "wrong_or_unverified:"+id)
		}
	}
	return failures
}

func offlineBenchmarkCLI(args []string) (int, error) {
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	dir := fs.String("dir", "", "new private benchmark directory")
	repeats := fs.Int("runs", 2, "repetitions per serial/parallel arm (1..10)")
	if err := commandflags.ParseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 2, err
	}
	if fs.NArg() != 0 || *dir == "" || *repeats < 1 || *repeats > 10 {
		return 2, errors.New("benchmark --dir NEW_DIRECTORY [--runs 1..10]; mock only")
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return 1, err
	}
	if err := os.Mkdir(root, 0700); err != nil {
		return 1, err
	}
	plan := benchmarkFixture()
	raw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return 1, err
	}
	if err := localfs.WriteNew(filepath.Join(root, "fixture.json"), append(raw, '\n')); err != nil {
		return 1, err
	}
	report := offlineBenchmark{Schema: "irrigator.benchmark/v1", Fixture: "squares-and-join/v1", Scope: "real mock scheduler, dependency acceptance, held-out output correctness and reported cost; no model quality or provider pricing comparison", Cells: []benchmarkCell{}}
	code := 0
	for repetition := 1; repetition <= *repeats; repetition++ {
		// Alternate arm order to avoid making first-run warmup systematic.
		arms := []int{1, 2}
		if repetition%2 == 0 {
			arms = []int{2, 1}
		}
		for _, workers := range arms {
			arm := "serial"
			if workers == 2 {
				arm = "parallel"
			}
			name := fmt.Sprintf("%02d-%s", repetition, arm)
			run := filepath.Join(root, name)
			opts := model.Options{Workers: workers, Timeout: 10 * time.Second, Budget: .25}
			if err := checkpoint.Initialize(plan, run, "mock", opts); err != nil {
				return 1, err
			}
			start := time.Now()
			exit, runErr := runops.Run(run, opts)
			cell := benchmarkCell{Arm: arm, Repetition: repetition, Workers: workers, WallMS: time.Since(start).Milliseconds(), ExitCode: exit, Run: name, Failures: []string{}}
			if runErr != nil {
				cell.Failures = append(cell.Failures, "runtime_error")
			}
			s, err := checkpoint.ReadState(run)
			if err != nil {
				return 1, err
			}
			cell.Failures = append(cell.Failures, gradeBenchmark(s)...)
			cell.Passed = len(cell.Failures) == 0 && exit == 0
			cell.Cost = provider.ProjectCost(s)
			cell.PlanSHA256 = s.PlanHash
			if !cell.Passed {
				code = 1
			}
			report.Cells = append(report.Cells, cell)
		}
	}
	raw, err = json.MarshalIndent(report, "", "  ")
	if err != nil {
		return 1, err
	}
	path := filepath.Join(root, "benchmark.json")
	if err := localfs.WriteNew(path, append(raw, '\n')); err != nil {
		return 1, err
	}
	fmt.Printf("Benchmark: %d cells, mock only, report %s\n", len(report.Cells), path)
	return code, nil
}
