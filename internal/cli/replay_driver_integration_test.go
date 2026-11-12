//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/artifactstore"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/debug"
	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

// Task replay classes. Everything except the reducer-* classes runs through the
// real worker path.
const (
	rcSkip           = "skip"                // never dispatched: pending behind a blocked dependency
	rcPreCancel      = "pre-cancel"          // cancelled before dispatch (cancelNode)
	rcMockRun        = "mock-run"            // mock executor + real validation: verified or failed
	rcMockInflight   = "mock-cancel-flight"  // cancelled while the mock executor ran
	rcHaikuRun       = "haiku-run"           // stub provider: verified, settled failure with diagnostic code
	rcHaikuTimeout   = "haiku-timeout"       // stub provider hangs: provider timeout -> uncertain
	rcHaikuMalformed = "haiku-malformed"     // stub provider prints garbage -> uncertain
	rcHaikuInflight  = "haiku-cancel-flight" // cancelled while the stub provider ran
	rcHaikuKilled    = "haiku-killed-worker" // worker process SIGKILLed mid-call, supervisor recovery
	rcReducer        = "reducer"             // no executor driver: cell transition reducer
)

func replayTaskClass(fx debug.ReplayFixture, ft debug.ReplayTask) string {
	switch {
	case ft.Attempt == 0 && ft.Status == "cancelled":
		return rcPreCancel
	case ft.Attempt == 0:
		return rcSkip
	case fx.Backend == "managed" && (ft.Status == "verified" || ft.Status == "failed" && ft.FailurePhase == "validation"):
		return rcMockRun // managed mock adapter: the managed worker path, no external call
	case fx.Backend == "mock" && ft.Status == "cancelled":
		return rcMockInflight
	case fx.Backend == "mock" && ft.Status == "verified" || fx.Backend == "mock" && ft.Status == "failed" && ft.FailurePhase == "validation":
		return rcMockRun
	case fx.Backend == "mock" && ft.Status == "waiting":
		return rcMockRun // a waiting parent is a verified spawn producer whose children are not all verified
	case fx.Backend == "haiku" && ft.Status == "verified":
		return rcHaikuRun
	case fx.Backend == "haiku" && ft.Status == "failed" && ft.FailurePhase == "execution" && ft.DiagnosticCode != "":
		return rcHaikuRun
	case fx.Backend == "haiku" && (ft.Status == "cancelled" || ft.CancelRequested):
		return rcHaikuInflight
	case fx.Backend == "haiku" && ft.Status == "uncertain" && ft.ErrorCode == "outcome_uncertain_deadline":
		return rcHaikuTimeout
	case fx.Backend == "haiku" && ft.Status == "uncertain" && ft.ErrorCode == "outcome_uncertain_provider_reported":
		return rcHaikuMalformed
	case fx.Backend == "haiku" && ft.Status == "uncertain" && ft.ErrorCode == replayKilledCode:
		return rcHaikuKilled
	}
	return rcReducer
}

// replayPathOf reports "executor" when every dispatched task replays through the
// real worker path, "reducer" when none does, and "mixed" otherwise.
func replayPathOf(fx debug.ReplayFixture) string {
	exec, red := 0, 0
	for _, ft := range fx.Tasks {
		switch c := replayTaskClass(fx, ft); c {
		case rcSkip:
		case rcReducer:
			red++
		default:
			exec++
		}
	}
	switch {
	case red == 0:
		return "executor"
	case exec == 0:
		return "reducer"
	}
	return "mixed"
}

func replayTopo(fx debug.ReplayFixture) []debug.ReplayTask {
	byID := map[string]debug.ReplayTask{}
	for _, t := range fx.Tasks {
		byID[t.ID] = t
	}
	var out []debug.ReplayTask
	done := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if done[id] {
			return
		}
		done[id] = true
		if p := byID[id].Parent; p != "" {
			visit(p)
		}
		for _, d := range byID[id].DependsOn {
			visit(d)
		}
		out = append(out, byID[id])
	}
	for _, t := range fx.Tasks {
		visit(t.ID)
	}
	return out
}

func replayUncertainReason(code string) string {
	r := strings.TrimPrefix(code, "outcome_uncertain_")
	if r == "unspecified" || r == "unclassified" {
		return ""
	}
	return r
}

func replayNum(id string) string { return strings.TrimPrefix(id, "t") }

func replayIsDynamic(ft debug.ReplayTask) bool {
	return ft.Parent != "" || ft.TurnRole == "helper" || (ft.TurnRole == "main" && ft.TurnIndex > 1)
}

// replayBuildPlan derives a plan from the fixture: dependency shape, spawn
// parents with their children as an expansion graph, and turn mains with their
// helpers and continuation. A failed task gets an unsatisfiable Equals check
// (validation failure); an in-flight cancel gets a delay long enough to cancel.
func replayBuildPlan(t *testing.T, fx debug.ReplayFixture) (model.Plan, error) {
	byID := map[string]debug.ReplayTask{}
	turn := false
	for _, ft := range fx.Tasks {
		byID[ft.ID] = ft
		turn = turn || ft.TurnRole != ""
	}
	local := func(ft debug.ReplayTask) string { return "n" + replayNum(ft.ID) }
	children := func(parent string) (out []debug.ReplayTask) {
		for _, ft := range fx.Tasks {
			if ft.Parent == parent && ft.TurnRole == "" {
				out = append(out, ft)
			}
		}
		return
	}
	var build func(ft debug.ReplayTask, name string, siblings bool) (model.Task, error)
	build = func(ft debug.ReplayTask, name string, siblings bool) (model.Task, error) {
		tk := model.Task{ID: name, Prompt: "replay", Requires: []string{"answer"}}
		for _, d := range ft.DependsOn {
			dep := byID[d]
			switch {
			case ft.TurnRole != "" && replayIsDynamic(ft):
				// helpers and continuations are wired by the turn request, not by names
			case siblings && dep.Parent == ft.Parent:
				tk.DependsOn = append(tk.DependsOn, local(dep))
			case !siblings:
				tk.DependsOn = append(tk.DependsOn, d)
			default:
				return tk, fmt.Errorf("task %s depends outside its spawn graph; not reproducible", ft.ID)
			}
		}
		// haiku cancelled attempts: the stub is long-running; nothing to set on the task
		switch {
		case ft.Status == "failed" && ft.FailurePhase == "validation":
			tk.Equals = map[string]string{"answer": "never-matches"}
		case ft.Status == "cancelled" && ft.Attempt > 0 && fx.Backend == "mock":
			tk.MockDelay = 5
		}
		if ft.Spawn {
			if fx.Backend != "mock" {
				return tk, fmt.Errorf("spawn parent %s on backend %s is not reproducible", ft.ID, fx.Backend)
			}
			var kids []model.Task
			for _, c := range children(ft.ID) {
				kt, err := build(c, local(c), true)
				if err != nil {
					return tk, err
				}
				kids = append(kids, kt)
			}
			raw, _ := json.Marshal(struct {
				Tasks []model.Task `json:"tasks"`
			}{kids})
			tk.Requires = []string{"graph"}
			tk.Spawn = &model.SpawnPolicy{Field: "graph", MaxChildren: 3}
			tk.MockResult = map[string]string{"graph": string(raw)}
		}
		return tk, nil
	}
	var roots []model.Task
	var buildTurn func(ft debug.ReplayTask, name string) (model.Task, error)
	buildTurn = func(ft debug.ReplayTask, name string) (model.Task, error) {
		tk, err := build(ft, name, false)
		if err != nil {
			return tk, err
		}
		tk.DependsOn = nil
		if ft.TurnIndex <= 1 {
			tk.DependsOn = append([]string{}, ft.DependsOn...)
		}
		var req checkpoint.TurnRequest
		for _, h := range fx.Tasks {
			if h.TurnRole == "helper" && h.Parent == ft.ID {
				ht, err := build(h, "h"+replayNum(h.ID), false)
				if err != nil {
					return tk, err
				}
				ht.DependsOn = nil
				req.Helpers = append(req.Helpers, ht)
			}
		}
		for _, c := range fx.Tasks {
			if c.TurnRole != "main" || c.TurnIndex != ft.TurnIndex+1 {
				continue
			}
			follows := false
			for _, d := range c.DependsOn {
				follows = follows || d == ft.ID
				if byID[d].TurnRole == "helper" {
					req.WaitForHelpers = true
				}
			}
			if follows {
				ct, err := buildTurn(c, "next")
				if err != nil {
					return tk, err
				}
				req.Continue = &ct
			}
		}
		if len(req.Helpers) > 0 || req.Continue != nil {
			raw, _ := json.Marshal(req)
			tk.Requires = []string{"answer", artifactstore.TurnRequestField}
			tk.MockResult = map[string]string{"answer": "ok", artifactstore.TurnRequestField: string(raw)}
		}
		if !replayIsDynamic(ft) {
			// The runtime assigns identity to generated continuations and helpers.
			tk.Turn = &model.TurnIdentity{AgentID: name, Role: "main", Index: ft.TurnIndex}
		}
		return tk, nil
	}
	for _, ft := range fx.Tasks {
		if replayIsDynamic(ft) {
			continue
		}
		var (
			tk  model.Task
			err error
		)
		if turn {
			tk, err = buildTurn(ft, ft.ID)
		} else {
			tk, err = build(ft, ft.ID, false)
		}
		if err != nil {
			return model.Plan{}, err
		}
		roots = append(roots, tk)
	}
	if !turn {
		return model.Plan{Name: "replay", Tasks: roots}, nil
	}
	shape := model.TurnShape{Roots: len(roots), MainTurns: 1}
	helpers := 0
	for _, ft := range fx.Tasks {
		if ft.TurnRole == "main" && ft.TurnIndex > shape.MainTurns {
			shape.MainTurns = ft.TurnIndex
		}
		n := 0
		for _, h := range fx.Tasks {
			if h.TurnRole == "helper" && h.Parent == ft.ID {
				n++
			}
		}
		if n > helpers {
			helpers = n
		}
	}
	shape.HelpersPerTurn = helpers
	report, err := model.ReportTurnShape(shape)
	if err != nil {
		return model.Plan{}, err
	}
	if fx.Backend == "managed" {
		_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
		return model.Plan{APIVersion: launch.ManagedPlanVersion, Name: "replay", TurnShape: &shape, MaxTasks: report.MaxExecutions, Tasks: roots,
			Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, RunBudgetUSD: "0"}}, nil
	}
	return model.Plan{APIVersion: checkpoint.TurnPlanVersion, Name: "replay", TurnShape: &shape, MaxTasks: report.MaxExecutions, Tasks: roots}, nil
}

// replayStubScript is the stub provider for one haiku task, chosen only from the
// fixture's recorded status, codes and cost flags.
func replayStubScript(ft debug.ReplayTask, class, marker string) (string, error) {
	cost := func(v string) string {
		if ft.CostReported {
			return `,"total_cost_usd":` + v
		}
		return ""
	}
	touch := "touch '" + marker + "'\n"
	switch class {
	case rcHaikuRun:
		if ft.Status == "verified" {
			body := `{"structured_output":{"answer":"ok"}` + cost("0.07") + `}`
			if !ft.CostReported && !ft.CostUnknown {
				body = `{"structured_output":{"answer":"ok"},"total_cost_usd":0}`
			}
			return "cat >/dev/null\nprintf '%s' '" + body + "'\n", nil
		}
		switch ft.DiagnosticCode {
		case model.DiagProviderAuth:
			free := `,"total_cost_usd":0`
			if ft.CostReported {
				free = `,"total_cost_usd":0.03`
			}
			return `cat >/dev/null; printf '%s' '{"type":"result","is_error":true,"result":"Not logged in"` + free + `}'; exit 1` + "\n", nil
		case model.DiagProviderError, model.DiagProviderSessionBudget, model.DiagProviderStructuredOutput:
			subtype := map[string]string{model.DiagProviderError: "error_max_turns", model.DiagProviderSessionBudget: "error_max_budget_usd", model.DiagProviderStructuredOutput: "error_max_structured_output_retries"}[ft.DiagnosticCode]
			return `cat >/dev/null; printf '%s' '{"type":"result","is_error":true,"subtype":"` + subtype + `"` + cost("0.03") + `}'; exit 1` + "\n", nil
		}
	case rcHaikuTimeout:
		s := "cat >/dev/null\n"
		if ft.CostReported {
			s += "printf '%s' '{\"structured_output\":{\"answer\":\"ok\"}" + cost("0.07") + "}'\n"
		}
		return s + "sleep 10\n", nil
	case rcHaikuMalformed:
		return "cat >/dev/null\nprintf '%s' '{bad'\n", nil
	case rcHaikuInflight, rcHaikuKilled:
		s := "cat >/dev/null\n"
		if ft.CostReported {
			s += "printf '%s' '{\"structured_output\":{\"answer\":\"ok\"}" + cost("0.07") + "}'\n"
		}
		return s + touch + "sleep 10\n", nil
	}
	return "", fmt.Errorf("no stub for %s", class)
}

func replayWaitMarker(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("stub provider never dispatched")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func replayWaitStatus(t *testing.T, root, id, status string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if runfixture.Read(t, root).Tasks[id].Status == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never reached %s", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func replayJoin(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("worker:", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not return")
	}
}

// replayRun rebuilds the run from the fixture. Dispatched tasks run through the
// real worker path in dependency order; tasks the fixture records as never
// dispatched are left alone.
func replayRun(t *testing.T, fx debug.ReplayFixture) *model.State {
	t.Helper()
	plan, err := replayBuildPlan(t, fx)
	if err != nil {
		t.Fatalf("replay %s: %v", fx.Scenario, err)
	}
	root := filepath.Join(t.TempDir(), "run")
	initBackend := "mock"
	if fx.Backend == "managed" {
		initBackend = "managed"
	}
	if err := checkpoint.Initialize(plan, root, initBackend); err != nil {
		t.Fatal(err)
	}
	if fx.Backend == "haiku" {
		if err := checkpoint.Update(root, func(s *model.State) error {
			s.Backend = "haiku"
			model.SaveCallLimits(s, model.Options{Budget: .25, Timeout: 120 * time.Second})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	marker := filepath.Join(stubDir, "dispatched")
	if fx.Backend == "haiku" {
		t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))
		t.Setenv("HOME", t.TempDir())
	}
	index := map[string]int{}
	for i, ft := range fx.Tasks {
		index[ft.ID] = i
	}
	realID := func(ft debug.ReplayTask) string {
		ids := runfixture.Read(t, root).AllTasks()
		if index[ft.ID] >= len(ids) {
			t.Fatalf("replay %s: %s was never created by its parent", fx.Scenario, ft.ID)
		}
		return ids[index[ft.ID]].ID
	}
	cells := executor.CheckpointCells{Root: root}
	for _, ft := range replayTopo(fx) {
		class := replayTaskClass(fx, ft)
		if class == rcSkip {
			continue
		}
		id := realID(ft)
		opts := model.Options{Timeout: 30 * time.Second, Budget: .25}
		if class == rcHaikuTimeout {
			opts.Timeout = 2 * time.Second
		}
		if strings.HasPrefix(class, "haiku") {
			script, err := replayStubScript(ft, class, marker)
			if err != nil {
				t.Fatalf("replay %s: %s: %v", fx.Scenario, ft.ID, err)
			}
			if err := os.WriteFile(filepath.Join(stubDir, "claude"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(marker)
		}
		runWorker := func() <-chan error {
			done := make(chan error, 1)
			go func() { done <- executor.WorkerOperation(root, id, opts, "") }()
			return done
		}
		switch class {
		case rcPreCancel:
			if runfixture.Read(t, root).Tasks[id].Status != "cancelled" {
				if err := runops.CancelNode(root, id); err != nil {
					t.Fatal(err)
				}
			}
		case rcMockRun, rcHaikuRun, rcHaikuTimeout, rcHaikuMalformed:
			if err := executor.WorkerOperation(root, id, opts, ""); err != nil {
				t.Fatalf("replay %s: worker %s: %v", fx.Scenario, ft.ID, err)
			}
		case rcMockInflight:
			done := runWorker()
			replayWaitStatus(t, root, id, "running")
			if err := runops.CancelNode(root, id); err != nil {
				t.Fatal(err)
			}
			replayJoin(t, done)
		case rcHaikuInflight:
			done := runWorker()
			replayWaitMarker(t, marker)
			if err := runops.CancelNode(root, id); err != nil {
				t.Fatal(err)
			}
			replayJoin(t, done)
		case rcHaikuKilled:
			replayKillWorker(t, root, id, marker)
		default:
			replayReducerTask(t, fx, cells, root, id, ft)
		}
	}
	return runfixture.Read(t, root)
}

// replayKillWorker runs a real _worker process, SIGKILLs it after the stub has
// dispatched, then lets the supervisor reconcile until the task leaves running.
func replayKillWorker(t *testing.T, root, id, marker string) {
	t.Helper()
	binary, _ := os.Executable()
	cmd := exec.Command(binary, "_worker", root, id, "--timeout", "30s", "--budget", "0.25")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	replayWaitMarker(t, marker)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := runops.Reconcile(root); err != nil {
			t.Fatal(err)
		}
		if runfixture.Read(t, root).Tasks[id].Status != "running" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not drain the killed worker")
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// replayReducerTask is the fallback for outcomes with no executor driver: it
// commits the recorded outcome through the real cell transition reducer.
func replayReducerTask(t *testing.T, fx debug.ReplayFixture, cells executor.CheckpointCells, root, id string, ft debug.ReplayTask) {
	t.Helper()
	begin := func() executor.AdmittedCellAttempt {
		s := runfixture.Read(t, root)
		a, err := cells.BeginAttempt(id, s.Tasks[id].OperationID, 123)
		if err != nil {
			t.Fatalf("replay %s: begin %s: %v", fx.Scenario, ft.ID, err)
		}
		return a
	}
	cost := 0.0
	if ft.CostReported {
		cost = .07
	}
	switch ft.Status {
	case "uncertain":
		a := begin()
		err := &model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: replayUncertainReason(ft.ErrorCode), Err: context.DeadlineExceeded}
		if e := cells.RecordOutcome(managed.CellOutcome{Fence: a.Fence, Cost: cost, Err: err, Diagnostic: ft.DiagnosticCode}); e != nil {
			t.Fatal(e)
		}
	default:
		t.Fatalf("replay %s: %s/%s on backend %s is not reproducible by this harness", fx.Scenario, ft.ID, ft.Status, fx.Backend)
	}
}

var _ = strconv.Itoa
