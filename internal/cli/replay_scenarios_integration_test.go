//go:build integration

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/debug"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/runops"
)

// replayKilledCode is the error code a SIGKILLed worker's task carries after
// supervisor reconciliation.
const replayKilledCode = "outcome_uncertain_unclassified"

func replayTask1(id string, deps ...string) model.Task {
	t := runfixture.Task(id, 0, deps...)
	t.Prompt = strings.Join([]string{runfixture.SentinelPrompt, id, runfixture.SentinelHomePath(), runfixture.SentinelToken}, " ")
	return t
}

func replayWait(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("controller did not exit")
	}
}

func replaySetEnv(t *testing.T) {
	t.Helper()
	t.Setenv("IRRIGATOR_SENTINEL_TOKEN", runfixture.SentinelEnv)
}

func replayMockRoot(t *testing.T, tasks ...model.Task) string {
	t.Helper()
	replaySetEnv(t)
	return runfixture.New(t, tasks...)
}

func replayHaikuRoot(t *testing.T) string {
	t.Helper()
	replaySetEnv(t)
	root := runfixture.New(t, replayTask1("a"), replayTask1("b", "a"))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Backend = "haiku"
		model.SaveCallLimits(s, model.Options{Budget: .25, Timeout: 120 * time.Second})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func replayStubMarker(t *testing.T, script string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "dispatched")
	runfixture.HaikuClaudeStub(t, strings.ReplaceAll(script, "@MARKER@", marker))
	return marker
}

func replaySpawn(t *testing.T, id string, children ...model.Task) model.Task {
	t.Helper()
	p := runfixture.SpawnTask(t, id, children...)
	p.Prompt = runfixture.SentinelPrompt + " " + runfixture.SentinelHomePath()
	return p
}

type replayScenario struct {
	name string
	set  string
	run  func(t *testing.T) *model.State
}

var replayScenarios = []replayScenario{
	{"failed-validation-blocks-dependent", "dev", func(t *testing.T) *model.State {
		a := replayTask1("a")
		a.Equals = map[string]string{"answer": runfixture.SentinelEquals}
		root := replayMockRoot(t, a, replayTask1("b", "a"), replayTask1("c"))
		_, done := clitest.Start(t, root)
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"cancelled-running-task", "dev", func(t *testing.T) *model.State {
		a := replayTask1("a")
		a.MockDelay = 5
		root := replayMockRoot(t, a, replayTask1("b", "a"))
		release := runfixture.HoldMock(t, "a") // still running when the cancel lands
		_, done := clitest.Start(t, root)
		waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
		if err := runops.CancelNode(root, "a"); err != nil {
			t.Fatal(err)
		}
		release("a")
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"uncertain-provider-timeout", "dev", func(t *testing.T) *model.State {
		runfixture.HaikuClaudeStub(t, "cat >/dev/null\nprintf '%s' '{\"structured_output\":{\"answer\":\""+runfixture.SentinelOutput+"\"},\"total_cost_usd\":0.07}'\nsleep 10\n")
		root := replayHaikuRoot(t)
		_, done := clitest.Start(t, root, "--timeout", "2s")
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"failed-mid-dag-partial-success", "heldout", func(t *testing.T) *model.State {
		b := replayTask1("b", "a")
		b.Equals = map[string]string{"answer": runfixture.SentinelEquals}
		root := replayMockRoot(t, replayTask1("a"), b, replayTask1("c", "b"), replayTask1("d", "a"))
		_, done := clitest.Start(t, root)
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"cancelled-pending-subtree", "heldout", func(t *testing.T) *model.State {
		root := replayMockRoot(t, replayTask1("a"), replayTask1("b", "a"), replayTask1("c", "b"), replayTask1("d", "a"))
		if err := runops.CancelNode(root, "b"); err != nil {
			t.Fatal(err)
		}
		_, done := clitest.Start(t, root)
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"uncertain-malformed-response", "heldout", func(t *testing.T) *model.State {
		runfixture.HaikuClaudeStub(t, "cat >/dev/null\nprintf '%s' '{bad "+runfixture.SentinelOutput+"'\n")
		root := replayHaikuRoot(t)
		_, done := clitest.Start(t, root)
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	// --- executor-path classes added in RF ---------------------------------
	{"uncertain-killed-worker", "dev", func(t *testing.T) *model.State {
		marker := replayStubMarker(t, "cat >/dev/null\nprintf '%s' '{\"structured_output\":{\"answer\":\""+runfixture.SentinelOutput+"\"},\"total_cost_usd\":0.07}'\ntouch '@MARKER@'\nsleep 10\n")
		root := replayHaikuRoot(t)
		if err := checkpoint.SetControl(root, "run"); err != nil {
			t.Fatal(err)
		}
		replayKillWorker(t, root, "a", marker)
		return runfixture.Read(t, root)
	}},
	{"cancelled-in-flight-provider", "heldout", func(t *testing.T) *model.State {
		marker := replayStubMarker(t, "cat >/dev/null\nprintf '%s' '{\"structured_output\":{\"answer\":\""+runfixture.SentinelOutput+"\"},\"total_cost_usd\":0.07}'\ntouch '@MARKER@'\nsleep 10\n")
		root := replayHaikuRoot(t)
		_, done := clitest.Start(t, root)
		replayWaitMarker(t, marker)
		if err := runops.CancelNode(root, "a"); err != nil {
			t.Fatal(err)
		}
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"failed-provider-auth", "dev", func(t *testing.T) *model.State {
		runfixture.HaikuClaudeStub(t, `cat >/dev/null; printf '%s' '{"type":"result","is_error":true,"result":"Not logged in `+runfixture.SentinelOutput+`","total_cost_usd":0}'; exit 1`)
		root := replayHaikuRoot(t)
		_, done := clitest.Start(t, root)
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"failed-provider-error-with-cost", "heldout", func(t *testing.T) *model.State {
		runfixture.HaikuClaudeStub(t, `cat >/dev/null; printf '%s' '{"type":"result","is_error":true,"subtype":"error_max_turns","result":"`+runfixture.SentinelOutput+`","total_cost_usd":0.03}'; exit 1`)
		root := replayHaikuRoot(t)
		_, done := clitest.Start(t, root)
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"failed-provider-session-budget", "heldout", func(t *testing.T) *model.State {
		runfixture.HaikuClaudeStub(t, `cat >/dev/null; printf '%s' '{"type":"result","is_error":true,"subtype":"error_max_budget_usd","result":"`+runfixture.SentinelOutput+`","total_cost_usd":0.03}'; exit 1`)
		root := replayHaikuRoot(t)
		_, done := clitest.Start(t, root)
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"verified-cost-unknown", "dev", func(t *testing.T) *model.State {
		runfixture.HaikuClaudeStub(t, "cat >/dev/null\nprintf '%s' '{\"structured_output\":{\"answer\":\""+runfixture.SentinelOutput+"\"}}'\n")
		root := replayHaikuRoot(t)
		_, done := clitest.Start(t, root)
		clitest.Finished(t, done, true)
		return runfixture.Read(t, root)
	}},
	{"spawn-waiting-parent-failed-child", "dev", func(t *testing.T) *model.State {
		bad := replayTask1("bad")
		bad.Equals = map[string]string{"answer": runfixture.SentinelEquals}
		root := replayMockRoot(t, replaySpawn(t, "parent", replayTask1("ok"), bad, replayTask1("after", "bad")))
		_, done := clitest.Start(t, root)
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"spawn-nested-waiting-cancelled-child", "heldout", func(t *testing.T) *model.State {
		// leaf2 waits on a slow sibling, so it cannot be dispatched before the cancel lands.
		slow := replayTask1("slow")
		slow.MockDelay = 1.5
		nested := replaySpawn(t, "branch", replayTask1("leaf"), slow, replayTask1("leaf2", "slow"))
		root := replayMockRoot(t, replaySpawn(t, "parent", nested, replayTask1("side")), replayTask1("join", "parent"))
		release := runfixture.HoldMock(t, "parent__branch__slow") // still running when the cancel lands
		_, done := clitest.Start(t, root)
		waitFor(t, root, func(s *model.State) bool {
			r := s.Tasks["parent__branch__slow"]
			return r != nil && r.Status == "running"
		})
		if err := runops.CancelNode(root, "parent__branch__leaf2"); err != nil {
			t.Fatal(err)
		}
		release("parent__branch__slow")
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
	{"managed-mock-failed-validation", "dev", func(t *testing.T) *model.State {
		replaySetEnv(t)
		_, catalog, _ := runfixture.ExecutionFixture(t, "mock")
		tasks := []model.Task{runfixture.Task("a", 0), runfixture.Task("b", 0), runfixture.Task("c", 0, "a")}
		for i := range tasks {
			tasks[i].Prompt = runfixture.SentinelPrompt + " " + tasks[i].ID // the mock profile admits at most 1024 input bytes
		}
		tasks[0].Equals = map[string]string{"answer": runfixture.SentinelEquals}
		for i := range tasks {
			tasks[i].Turn = &model.TurnIdentity{AgentID: tasks[i].ID, Role: "main", Index: 1}
		}
		shape := model.TurnShape{Roots: 3, MainTurns: 1}
		report, err := model.ReportTurnShape(shape)
		if err != nil {
			t.Fatal(err)
		}
		p := model.Plan{APIVersion: launch.ManagedPlanVersion, Name: "managed", TurnShape: &shape, MaxTasks: report.MaxExecutions, Tasks: tasks, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, RunBudgetUSD: "0"}}
		root := filepath.Join(t.TempDir(), "run")
		if err := checkpoint.Initialize(p, root, "managed"); err != nil {
			t.Fatal(err)
		}
		if err := checkpoint.SetControl(root, "run"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"a", "b"} {
			if err := worker(root, id, model.Options{Timeout: 10 * time.Second}); err != nil {
				t.Fatal(err)
			}
		}
		return runfixture.Read(t, root)
	}},
	{"turn-failed-helper-blocks-waited-continuation", "dev", func(t *testing.T) *model.State {
		replaySetEnv(t)
		bad := replayTask1("h")
		bad.Equals = map[string]string{"answer": runfixture.SentinelEquals}
		next := replayTask1("next")
		owner := runfixture.TurnRequestTask(t, "a", checkpoint.TurnRequest{Continue: &next, Helpers: []model.Task{bad}, WaitForHelpers: true})
		owner.Prompt = runfixture.SentinelPrompt + " " + runfixture.SentinelHomePath() + " " + runfixture.SentinelToken
		root := runfixture.TurnFixture(t, model.TurnShape{Roots: 1, MainTurns: 2, HelpersPerTurn: 1}, owner)
		_, done := clitest.Start(t, root, "--workers", "3")
		replayWait(t, done)
		return runfixture.Read(t, root)
	}},
}

// TestReplayLiveRoundTrip runs every scenario for real, exports it, checks the
// export is redacted, then replays the in-memory fixture. It also writes the
// committed fixtures when IRRIGATOR_REPLAY_REGEN names an output directory.
func TestReplayLiveRoundTrip(t *testing.T) {
	regen := os.Getenv("IRRIGATOR_REPLAY_REGEN")
	for _, sc := range replayScenarios {
		t.Run(sc.name, func(t *testing.T) {
			s := sc.run(t)
			rawState, _ := json.Marshal(s)
			if runfixture.ReplayContainsSentinel(rawState) == "" {
				t.Fatal("scenario state carries no sentinel; redaction proof would be vacuous")
			}
			out, err := debug.BuildReplayFixtureBytes(s, sc.name)
			if err != nil {
				t.Fatal(err)
			}
			if hit := runfixture.ReplayContainsSentinel(out); hit != "" {
				t.Fatalf("fixture leaked %s", hit)
			}
			fx, err := debug.DecodeReplayFixture(out)
			if err != nil {
				t.Fatal(err)
			}
			if os.Getenv("IRRIGATOR_REPLAY_SHOW") != "" {
				t.Logf("%s\n%s", sc.name, out)
			}
			t.Logf("replay path: %s", replayPathOf(fx))
			if diffs := replayRoundTrip(t, fx); len(diffs) > 0 {
				t.Fatalf("round trip diverged:\n  %s", strings.Join(diffs, "\n  "))
			}
			if regen != "" {
				dir := filepath.Join(regen, sc.set)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, sc.name+".json"), out, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

var _ = fmt.Sprintf
var _ = sort.Strings
