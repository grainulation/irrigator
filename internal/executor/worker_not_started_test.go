package executor

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestWorkerNotStartedCertaintyWithoutReplaySafety(t *testing.T) {
	for _, tc := range []struct{ name, extra, raw, want string }{
		{"not_started", `,"outcome":"not_started","error":"launch rejected"`, "", "failed"},
		{"missing_executable", "", "", "failed"},
		{"preparation_rejected", "", "", "failed"},
		{"legacy", `,"error":"failure"`, "", "uncertain"},
		{"settled", `,"outcome":"settled","error":"failure"`, "", "failed"},
		{"uncertain", `,"outcome":"uncertain","error":"unknown"`, "", "uncertain"},
		{"invalid", `,"outcome":"maybe","error":"unknown"`, "", "uncertain"},
		{"not_started_without_error", `,"outcome":"not_started","output":{"x":"v"}`, "", "uncertain"},
		{"not_started_without_error_exit1", "", "", "uncertain"},
		{"malformed", "", `{bad`, "uncertain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "preparation_rejected" {
				t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", filepath.Join(t.TempDir(), "missing-capsule"))
			}
			script := runfixture.WriteOutcomeScript(t)
			runfixture.SetOutcomeEnv(t, tc.extra, tc.raw, "")
			if tc.name == "not_started_without_error_exit1" {
				script = runfixture.WriteScriptBody(t, strings.Replace(runfixture.ValidExitScript, `"error":"known"`, `"outcome":"not_started"`, 1))
			}
			if tc.name == "missing_executable" {
				script = filepath.Join(t.TempDir(), "missing")
			}
			plan := model.Plan{Name: "certainty", Tasks: []model.Task{{ID: "a", Prompt: "p", Requires: []string{"x"}, Provider: "external"}}, Providers: map[string]model.CommandSpec{"external": {Command: []string{script}, Env: []string{"RECOVERY_EXTRA", "RECOVERY_RAW"}}}}
			root := filepath.Join(t.TempDir(), "run")
			if err := checkpoint.Initialize(plan, root, "mock"); err != nil {
				t.Fatal(err)
			}
			if err := checkpoint.SetControl(root, "run"); err != nil {
				t.Fatal(err)
			}
			opts := model.Options{Timeout: 10 * time.Second}
			if err := worker(root, "a", opts); err != nil {
				t.Fatal(err)
			}
			s, err := checkpoint.ReadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if graph.ReplaySafe(s, s.Tasks["a"].Spec) {
				t.Fatal("fixture unexpectedly replay safe")
			}
			if got := s.Tasks["a"].Status; got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
			if err := worker(root, "a", opts); err != nil {
				t.Fatal(err)
			}
			again, err := checkpoint.ReadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if again.Tasks["a"].Attempt != 1 || again.Tasks["a"].Status != tc.want {
				t.Fatalf("automatic replay: %+v", again.Tasks["a"])
			}
		})
	}
}

func TestLocalNotStartedMarkerSurvivesMalformedResponse(t *testing.T) {
	err := model.MalformedOutcome(model.WorkRequest{Kind: "task"}, nil, model.MarkNotStarted(errors.New("executable not started")), errors.New("empty response"))
	if !model.KnownNotStarted(err) || model.IsUncertain(err) {
		t.Fatalf("lost positive non-dispatch evidence: %v", err)
	}
}
