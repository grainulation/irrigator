package runfixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
)

func CampaignTestPlan(t *testing.T, budget string) model.Plan {
	t.Helper()
	r := ManagedStateFixture(t, "mock", budget, 1)
	s, e := checkpoint.ReadState(r)
	if e != nil {
		t.Fatal(e)
	}
	p := s.Plan
	p.APIVersion = launch.UnknownCostPlanVersion
	p.Execution.CostPolicy = "retain_unknown"
	return p
}

func ExecutionFixture(t *testing.T, kind string) (string, model.ExecutionCatalog, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter := model.ExecutionAdapter{Kind: "mock", Protocol: model.ProtocolVersion}
	instance := model.ExecutionInstance{Adapter: "adapter", CredentialRefs: []model.ExecutionCredentialRef{}}
	profile := model.ExecutionProfile{Instance: "account", Model: "mock", Effort: "none", RequiredCapabilities: []string{"structured_strings", "outcome_certainty"}, Limits: model.ExecutionLimits{InputBytes: 1024, OutputBytes: 1024}, Tools: []string{}, OperationBudgetUSD: "0"}
	marker := filepath.Join(root, "must-not-execute")
	if kind == "claude_cli" {
		exe := filepath.Join(root, "claude-fixture")
		data := []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\n", marker))
		if err := os.WriteFile(exe, data, 0700); err != nil {
			t.Fatal(err)
		}
		hash, err := launch.ExecutableDigest(exe)
		if err != nil {
			t.Fatal(err)
		}
		adapter = model.ExecutionAdapter{Kind: kind, Protocol: model.ProtocolVersion, Command: []string{exe}, ExecutableSHA256: hash}
		instance.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "env", Name: "PROFILE_TEST_SECRET"}}
		profile.Model = "requested-model"
		profile.Effort = "low"
		profile.OperationBudgetUSD = "0.25"
	}
	c := model.ExecutionCatalog{Schema: launch.ExecutionCatalogSchema, Adapters: map[string]model.ExecutionAdapter{"adapter": adapter}, Instances: map[string]model.ExecutionInstance{"account": instance}, Profiles: map[string]model.ExecutionProfile{"small": profile}, DefaultProfile: "small"}
	path := filepath.Join(root, "catalog.json")
	WriteExecutionFixture(t, path, c)
	return path, c, marker
}

func WriteExecutionFixture(t *testing.T, path string, c model.ExecutionCatalog) {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func HaikuClaudeStub(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	return dir
}

func HaikuRoot(t *testing.T) string {
	root := New(t, Task("a", 0))
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Backend = "haiku"
		model.
			// These fake-provider recovery tests have explicit call admission;
			// changing a mock fixture's backend alone is not budget consent.
			SaveCallLimits(s, model.Options{Budget: .25, Timeout: 120 * time.Second})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return root
}

func MailboxMessageFor(s *model.State, key, kind string) model.MailboxMessage {
	r := s.Tasks["a"]
	return model.MailboxMessage{Schema: "irrigator.message/v1", Key: key, RunID: s.RunID, TaskID: "a", OperationID: r.OperationID, Generation: r.Generation, Attempt: r.Attempt, Kind: kind, Payload: "explicit local instruction"}
}

func ManagedAdapterFixture(t *testing.T, script string) (model.ExecutionBinding, model.WorkRequest, string, string) {
	t.Helper()
	path, c, _ := ExecutionFixture(t, "claude_cli")
	a := c.Adapters["adapter"]
	if err := os.WriteFile(a.Command[0], []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > argv.txt\ncat > captured.json\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a.ExecutableSHA256, _ = launch.ExecutableDigest(a.Command[0])
	c.Adapters["adapter"] = a
	i := c.Instances["account"]
	i.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "host_session", Name: "claude"}}
	c.Instances["account"] = i
	p := c.Profiles["small"]
	p.Limits = model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}
	c.Profiles["small"] = p
	b, err := launch.ResolveExecutionCatalog(c, "inline", "small")
	if err != nil {
		t.Fatal(err)
	}
	req := model.WorkRequest{Protocol: model.ProtocolVersion, Kind: "task", RunID: "run", OperationID: "op", RequestID: "request", Attempt: 1, Task: model.Task{ID: "node", Prompt: "return answer", Requires: []string{"answer"}}, Context: map[string]string{"marker": "current"}, MaxCostUSD: .25, TimeoutMS: 5000}
	dir := filepath.Dir(path)
	return b, req, dir, filepath.Join(dir, "1")
}

// Paid-shaped profiles use a local shell fixture only. This exercises the real
// worker, supervisor, checkpoint reservations, and adapter without vendor calls.
func CompletionManagedFixture(t *testing.T, script string) (string, model.ExecutionCatalog) {
	t.Helper()
	_, c, _ := ExecutionFixture(t, "claude_cli")
	a := c.Adapters["adapter"]
	data := []byte("#!/bin/sh\ncat > invoked.json\n" + script + "\n")
	if err := os.WriteFile(a.Command[0], data, 0700); err != nil {
		t.Fatal(err)
	}
	a.ExecutableSHA256, _ = launch.ExecutableDigest(a.Command[0])
	c.Adapters["adapter"] = a
	i := c.Instances["account"]
	i.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "host_session", Name: "claude"}}
	c.Instances["account"] = i
	p := c.Profiles["small"]
	p.Limits = model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}
	c.Profiles["small"] = p
	c.Profiles["other"] = p
	shape := model.TurnShape{Roots: 2, MainTurns: 1, HelpersPerTurn: 0}
	mains := []model.Task{Task("a", 0), Task("b", 0)}
	for k := range mains {
		mains[k].Turn = &model.TurnIdentity{AgentID: mains[k].ID, Role: "main", Index: 1}
		mains[k].Profile = []string{"small", "other"}[k]
	}
	plan := model.Plan{APIVersion: launch.ManagedPlanVersion, Name: "independent managed test", TurnShape: &shape, MaxTasks: 2, Tasks: mains, Execution: &model.ManagedExecutionPlan{Catalog: c, AllowedProfiles: []string{"small", "other"}, RunBudgetUSD: "0.25"}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(plan, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	return root, c
}

func CompletionCodexFixture(t *testing.T, budget string, reject bool) string {
	t.Helper()
	base, _ := CompletionManagedFixture(t, `exit 0`)
	p := Read(t, base).Plan
	a := p.Execution.Catalog.Adapters["adapter"]
	a.Kind = "codex_cli"
	script := "#!/bin/sh\ncat >/dev/null\ncat <<'FIXTURE'\n" + CodexFixtureSuccess + "FIXTURE\n"
	if err := os.WriteFile(a.Command[0], []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a.ExecutableSHA256, _ = launch.ExecutableDigest(a.Command[0])
	p.Execution.Catalog.Adapters["adapter"] = a
	i := p.Execution.Catalog.Instances["account"]
	i.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "host_session", Name: "codex"}}
	p.Execution.Catalog.Instances["account"] = i
	p.APIVersion = launch.UnknownCostPlanVersion
	p.Execution.CostPolicy = "retain_unknown"
	p.Execution.RunBudgetUSD = budget
	if reject {
		p.Tasks[0].Checks = []model.Check{{Kind: "contains", Field: "answer", Value: "different"}}
	}
	root := filepath.Join(t.TempDir(), "codex")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	return root
}

func ManagedStateFixture(t *testing.T, kind, budget string, count int) string {
	t.Helper()
	_, catalog, _ := ExecutionFixture(t, kind)
	if kind == "claude_cli" {
		i := catalog.Instances["account"]
		i.CredentialRefs = []model.ExecutionCredentialRef{{Kind: "host_session", Name: "claude"}}
		catalog.Instances["account"] = i
	}
	tasks := []model.Task{}
	for i := 0; i < count; i++ {
		id := string(rune('a' + i))
		n := Task(id, .01)
		n.Turn = &model.TurnIdentity{AgentID: id, Role: "main", Index: 1}
		tasks = append(tasks, n)
	}
	shape := model.TurnShape{Roots: count, MainTurns: 1, HelpersPerTurn: 0}
	report, err := model.ReportTurnShape(shape)
	if err != nil {
		t.Fatal(err)
	}
	p := model.Plan{APIVersion: launch.ManagedPlanVersion, Name: "managed", TurnShape: &shape, MaxTasks: report.MaxExecutions, Tasks: tasks, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, RunBudgetUSD: budget}}
	root := filepath.Join(t.TempDir(), "run")
	if e := checkpoint.Initialize(p, root, "managed"); e != nil {
		t.Fatal(e)
	}
	if e := checkpoint.SetControl(root, "run"); e != nil {
		t.Fatal(e)
	}
	return root
}

// CostAmount is the response's reported cost in micros for display and tests
// only. It maps an invalid cost to zero, which is NOT a safe accounting value:
// reservation and settlement code must call resolveCost and handle the error so
// an invalid cost can never shrink a held reservation (see settleResponseCost).
func CostAmount(r model.WorkResponse) int64 {
	m, err := r.ResolveCost()
	if err != nil {
		return 0
	}
	return m
}

func UnbudgetedTestOptions() model.Options {
	return model.Options{Workers: 1, Timeout: 5 * time.Second, Budget: 0.25}
}

func MailboxFixture(t *testing.T) string {
	t.Helper()
	_, catalog, _ := ExecutionFixture(t, "mock")
	node := Task("a", 0)
	node.Turn = &model.TurnIdentity{AgentID: "a", Role: "main", Index: 1}
	p := model.Plan{APIVersion: model.GovernedPlanVersion, Name: "mailbox fixture", MaxTasks: 1, TurnShape: &model.TurnShape{Roots: 1, MainTurns: 1}, Tasks: []model.Task{node}, Execution: &model.ManagedExecutionPlan{Catalog: catalog, AllowedProfiles: []string{"small"}, CostPolicy: "retain_unknown", RunBudgetUSD: "0"}}
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	return root
}

const CodexFixtureSuccess = `{"type":"thread.started","thread_id":"fixture-thread"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"{\"answer\":\"ok\"}"}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0}}
`

func MigrationFixture(t *testing.T) (string, string, string) {
	t.Helper()
	prior := ManagedStateFixture(t, "claude_cli", "1", 2)
	p := Read(t, prior).Plan
	profile := p.Execution.Catalog.Profiles["small"]
	profile.Limits = model.ExecutionLimits{InputBytes: 65536, OutputBytes: 65536}
	p.Execution.Catalog.Profiles["small"] = profile
	old := p.Execution.Catalog.Adapters["adapter"].Command[0]
	script := []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"type\":\"result\",\"structured_output\":{\"answer\":\"ok\"},\"total_cost_usd\":0.01}'\n")
	if err := os.WriteFile(old, script, 0700); err != nil {
		t.Fatal(err)
	}
	a := p.Execution.Catalog.Adapters["adapter"]
	a.ExecutableSHA256, _ = launch.ExecutableDigest(old)
	p.Execution.Catalog.Adapters["adapter"] = a
	root := filepath.Join(t.TempDir(), "run")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "new-claude")
	if err := os.WriteFile(replacement, append(script, []byte("# newer build\n")...), 0700); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "pause"); err != nil {
		t.Fatal(err)
	}
	return root, old, replacement
}

func UnknownCostFixture(t *testing.T) string {
	t.Helper()
	old := ManagedStateFixture(t, "claude_cli", "0.25", 2)
	s, err := checkpoint.ReadState(old)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Plan
	p.APIVersion = launch.UnknownCostPlanVersion
	p.Execution.CostPolicy = "retain_unknown"
	root := filepath.Join(t.TempDir(), "v4")
	if err := checkpoint.Initialize(p, root, "managed"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	return root
}
