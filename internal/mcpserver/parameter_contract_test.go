package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/review"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func forMCPProfiles(t *testing.T, test func(*testing.T, McpConfig)) {
	t.Helper()
	for _, canonical := range []bool{false, true} {
		for _, control := range []bool{false, true} {
			t.Run(fmt.Sprintf("canonical=%t/control=%t", canonical, control), func(t *testing.T) {
				test(t, McpConfig{canonicalNames: canonical, AllowControl: control})
			})
		}
	}
}

func callParameterFixture(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		content, _ := json.Marshal(result.Content)
		t.Fatalf("%s failed: %s", name, content)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMCPRunReferencePrecedenceAcrossProfiles(t *testing.T) {
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		workspace := t.TempDir()
		roots := []string{t.TempDir(), t.TempDir()}
		initialize := func(parent, name string) string {
			t.Helper()
			root := filepath.Join(parent, name)
			if err := checkpoint.Initialize(model.Plan{Name: name, Tasks: []model.Task{runfixture.Task("worker", 0)}}, root, "mock"); err != nil {
				t.Fatal(err)
			}
			return root
		}
		local := initialize(workspace, "r")
		first := initialize(roots[0], "r")
		second := initialize(roots[1], "r")
		initialize(workspace, "workspace-only")
		cfg.Workspace, cfg.RunRoots = workspace, roots
		cs := connectDefinitionTest(t, cfg)
		for _, test := range []struct{ selector, want string }{{"r", first}, {"./r", local}, {second, second}} {
			out := callParameterFixture(t, cs, mcpToolName(cfg, "run_status"), map[string]any{"run": test.selector})
			if out["path"] != test.want {
				t.Fatalf("selector %q resolved to %v, want %s", test.selector, out["path"], test.want)
			}
		}
		missing, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpToolName(cfg, "run_status"), Arguments: map[string]any{"run": "workspace-only"}})
		if err != nil || !missing.IsError {
			t.Fatalf("bare name unexpectedly fell back to workspace: %+v %v", missing, err)
		}
		cfg.RunRoots = nil
		localCS := connectDefinitionTest(t, cfg)
		out := callParameterFixture(t, localCS, mcpToolName(cfg, "run_status"), map[string]any{"run": "r"})
		if out["path"] != local {
			t.Fatalf("bare name without roots did not use workspace: %+v", out)
		}
	})
}

func TestMCPRunLookupDoesNotSkipUninspectableCandidate(t *testing.T) {
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		roots := []string{t.TempDir(), t.TempDir()}
		if err := os.Symlink("r", filepath.Join(roots[0], "r")); err != nil {
			t.Fatal(err)
		}
		second := filepath.Join(roots[1], "r")
		if err := checkpoint.Initialize(model.Plan{Tasks: []model.Task{runfixture.Task("a", 0)}}, second, "mock"); err != nil {
			t.Fatal(err)
		}
		cfg.Workspace, cfg.RunRoots = t.TempDir(), roots
		cs := connectDefinitionTest(t, cfg)
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpToolName(cfg, "run_status"), Arguments: map[string]any{"run": "r"}})
		if err != nil || !result.IsError {
			t.Fatalf("lookup silently selected a later run after a filesystem error: %+v %v", result, err)
		}
		out := callParameterFixture(t, cs, mcpToolName(cfg, "run_status"), map[string]any{"run": second})
		if out["path"] != second {
			t.Fatal("explicit second-root selector stopped working")
		}
	})
}

func TestMCPDotRunReferencesStayWorkspaceRelative(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "plan.json")
	data, err := json.Marshal(model.Plan{Tasks: []model.Task{runfixture.Task("a", 0)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan, data, 0600); err != nil {
		t.Fatal(err)
	}
	started := make(chan []string, 4)
	previous := McpStartDetached
	McpStartDetached = func(args []string) error {
		started <- append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { McpStartDetached = previous })
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		for _, ref := range []string{".", ".."} {
			parent := t.TempDir()
			cfg.Workspace = filepath.Join(parent, "workspace", "leaf")
			cfg.RunRoots = []string{filepath.Join(parent, "discovery")}
			want := cfg.Workspace
			if ref == ".." {
				want = filepath.Dir(want)
			}
			cs := connectDefinitionTest(t, cfg)
			if cfg.AllowControl {
				out := callParameterFixture(t, cs, mcpToolName(cfg, "apply"), map[string]any{"run": ref, "plan": plan, "backend": "mock", "budget": 0.1})
				if out["run"] != want {
					t.Fatalf("apply %q selected %v, want %s", ref, out["run"], want)
				}
				select {
				case args := <-started:
					if len(args) < 2 || args[0] != "resume" || args[1] != want {
						t.Fatalf("controller selected a different run: %v", args)
					}
				default:
					t.Fatal("controller launch was not requested")
				}
			} else if err := checkpoint.Initialize(model.Plan{Tasks: []model.Task{runfixture.Task("a", 0)}}, want, "mock"); err != nil {
				t.Fatal(err)
			}
			out := callParameterFixture(t, cs, mcpToolName(cfg, "run_status"), map[string]any{"run": ref})
			if out["path"] != want {
				t.Fatalf("inspection %q selected %v, want %s", ref, out["path"], want)
			}
			if cfg.AllowControl {
				callParameterFixture(t, cs, mcpToolName(cfg, "pause"), map[string]any{"run": ref})
				state, err := checkpoint.ReadState(want)
				if err != nil || state.Control != "pause" {
					t.Fatalf("pause did not select the initialized run: %+v %v", state, err)
				}
			}
			if _, err := os.Stat(cfg.RunRoots[0]); !os.IsNotExist(err) {
				t.Fatalf("workspace selector initialized the discovery root: %v", err)
			}
		}
	})
}

func TestMCPRejectsUnrepresentableResolvedRunPaths(t *testing.T) {
	bad := filepath.Join(t.TempDir(), string([]byte{0xff}))
	if _, err := newMCPConfig([]string{bad}, t.TempDir(), false); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("startup accepted an unrepresentable run root: %v", err)
	}
	if _, err := newMCPConfig(nil, bad, false); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("startup accepted an unrepresentable workspace: %v", err)
	}
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte(`{"tasks":[{"id":"a","prompt":"Return fixture","requires":["answer"],"mock_result":{"answer":"fixture"}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.LoadPlan(plan); err != nil {
		t.Fatalf("invalid test plan: %v", err)
	}
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		for _, roots := range []bool{false, true} {
			cfg.Workspace, cfg.RunRoots = bad, nil
			selector := "./r"
			if roots {
				cfg.Workspace, cfg.RunRoots, selector = t.TempDir(), []string{bad}, "r"
			}
			cs := connectDefinitionTest(t, cfg)
			calls := []struct {
				name string
				args map[string]any
			}{{mcpToolName(cfg, "run_status"), map[string]any{"run": selector}}}
			if cfg.AllowControl {
				calls = append(calls, struct {
					name string
					args map[string]any
				}{mcpToolName(cfg, "pause"), map[string]any{"run": selector}})
				calls = append(calls, struct {
					name string
					args map[string]any
				}{mcpToolName(cfg, "apply"), map[string]any{"run": selector, "plan": plan, "backend": "mock", "budget": 0.1}})
			}
			for _, call := range calls {
				result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: call.name, Arguments: call.args})
				if err != nil {
					t.Fatal(err)
				}
				message := result.Content[0].(*mcp.TextContent).Text
				if !result.IsError || !strings.Contains(message, "not valid UTF-8") {
					t.Fatalf("%s did not reject its resolved path before storage access: %s", call.name, message)
				}
			}
		}
	})
}

func TestMCPIdentifiersAndFilteredPaginationAcrossProfiles(t *testing.T) {
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		root := runfixture.New(t, runfixture.Task("worker", 0), runfixture.Task("other", 0))
		root, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		var start int
		if err := checkpoint.Update(root, func(s *model.State) error {
			s.Tasks["worker"].Attempt = 2
			start = len(s.Events)
			s.Event("fixture", "other", nil)
			s.Event("fixture", "worker", nil)
			s.Event("fixture", "other", nil)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		capture := filepath.Join(root, "cells", "worker", "1.stdout.json")
		if err := os.MkdirAll(filepath.Dir(capture), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(capture, []byte("captured fixture output"), 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(root, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Workspace = t.TempDir()
		cs := connectDefinitionTest(t, cfg)
		status := callParameterFixture(t, cs, mcpToolName(cfg, "run_status"), map[string]any{"run": root})
		var id string
		for _, cell := range status["cells"].([]any) {
			row := cell.(map[string]any)
			if row["id"] == "worker" {
				id = row["id"].(string)
			}
		}
		if id == "" {
			t.Fatal("status did not supply the task ID")
		}
		cell := callParameterFixture(t, cs, "inspect_cell", map[string]any{"run": root, "cell": id})
		if cell["cell"] != filepath.Join(root, "cells", id) || cell["node"].(map[string]any)["spec"].(map[string]any)["id"] != id {
			t.Fatalf("cell ID/directory/object distinction lost: %+v", cell)
		}
		output := callParameterFixture(t, cs, mcpToolName(cfg, "attempt_output"), map[string]any{"run": root, "task": id, "attempt": 1})
		if output["task"] != id || output["output"] != "captured fixture output" {
			t.Fatalf("status ID did not select historical output: %+v", output)
		}
		advisory := callParameterFixture(t, cs, mcpToolName(cfg, "provider_limit"), map[string]any{"run": root, "node": output["task"], "attempt": output["attempt"]})
		if advisory["task"] != id || advisory["attempt"] != output["attempt"] {
			t.Fatalf("historical task/attempt did not transfer into advisory: %+v", advisory)
		}
		missing, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpToolName(cfg, "attempt_output"), Arguments: map[string]any{"run": root, "task": id, "attempt": 2}})
		if err != nil || !missing.IsError {
			t.Fatalf("attempt count invented an available capture: %+v %v", missing, err)
		}
		page := callParameterFixture(t, cs, mcpToolName(cfg, "run_logs"), map[string]any{"run": root, "cell": id, "since": start, "limit": 1})
		if len(page["events"].([]any)) != 1 || page["next_since"] != float64(start+2) || page["more"] != true {
			t.Fatalf("filtered page lost consumed-event position: %+v", page)
		}
		tail := callParameterFixture(t, cs, mcpToolName(cfg, "run_logs"), map[string]any{"run": root, "cell": id, "since": page["next_since"], "limit": 1})
		if len(tail["events"].([]any)) != 0 || tail["next_since"] != float64(start+3) || tail["more"] != false {
			t.Fatalf("filtered tail did not consume other-cell events: %+v", tail)
		}
		after, err := os.ReadFile(filepath.Join(root, "state.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("inspection changed saved run state")
		}
	})
}

func TestMCPDeliverySummaryBeforeSelectionAcrossProfiles(t *testing.T) {
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		root := runfixture.New(t, runfixture.Task("worker", 0))
		root, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Workspace = t.TempDir()
		cs := connectDefinitionTest(t, cfg)
		out := callParameterFixture(t, cs, mcpToolName(cfg, "delivery_summary"), map[string]any{"run": root})
		if out["candidate_sha256"] != nil || len(out["included"].([]any)) != 0 || len(out["excluded"].([]any)) != 1 || out["dispatched"] != false {
			t.Fatalf("pending run invented a delivery candidate: %+v", out)
		}
		if _, err := os.Stat(filepath.Join(root, "delivery.json")); !os.IsNotExist(err) {
			t.Fatalf("inspection created a delivery selection: %v", err)
		}
	})
}

func TestMCPQueuedBranchComparisonAcrossProfiles(t *testing.T) {
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		workspace, _ := runfixture.FixtureRepo(t)
		workspace, err := filepath.EvalSymlinks(workspace)
		if err != nil {
			t.Fatal(err)
		}
		runfixture.FixtureGit(t, workspace, "switch", "-qc", "topic")
		runfixture.FixtureWrite(t, workspace, "README.md", "first change\n")
		runfixture.FixtureGit(t, workspace, "add", "README.md")
		runfixture.FixtureGit(t, workspace, "commit", "-qm", "first change")
		runfixture.FixtureGit(t, workspace, "switch", "-q", "main")
		cfg.Workspace, _ = runfixture.FixtureRepo(t)
		cs := connectDefinitionTest(t, cfg)
		queue := callParameterFixture(t, cs, mcpToolName(cfg, "review_list"), map[string]any{"workspace": workspace, "base": "main"})["queue"].(map[string]any)
		changes := queue["changes"].([]any)
		if len(changes) != 1 {
			t.Fatalf("unexpected queued branches: %+v", changes)
		}
		branch := changes[0].(map[string]any)["branch"]
		args := map[string]any{"workspace": queue["workspace"], "branch": branch, "base": queue["base"], "file": "README.md"}
		first := callParameterFixture(t, cs, "inspect_review", args)
		comparison := first["review"].(map[string]any)
		if first["kind"] != "branch" || !strings.Contains(first["diff"].(string), "+first change") {
			t.Fatalf("queue selector did not read its branch: %+v", first)
		}
		// A subsequent call follows the moved branch, but the returned IDs still
		// select the original complete comparison for an oversized-patch handoff.
		runfixture.FixtureGit(t, workspace, "switch", "-q", "topic")
		runfixture.FixtureWrite(t, workspace, "README.md", "second change\n")
		runfixture.FixtureGit(t, workspace, "commit", "-qam", "second change")
		runfixture.FixtureGit(t, workspace, "switch", "-q", "main")
		second := callParameterFixture(t, cs, "inspect_review", args)
		if second["review"].(map[string]any)["head"] == comparison["head"] || !strings.Contains(second["diff"].(string), "+second change") {
			t.Fatalf("fresh inspection did not resolve moved branch: %+v", second)
		}
		pinned := runfixture.FixtureGit(t, workspace, "diff", "--no-ext-diff", "--no-textconv", comparison["merge_base"].(string), comparison["head"].(string), "--", "README.md")
		if !strings.Contains(pinned, "+first change") || strings.Contains(pinned, "+second change") {
			t.Fatalf("returned comparison IDs did not preserve original patch: %s", pinned)
		}
		if _, err := os.Stat(filepath.Join(workspace, ".irrigator")); !os.IsNotExist(err) {
			t.Fatalf("reading branch reviews created local review state: %v", err)
		}
	})
}

func TestMCPQueuedDocumentRetainsExplicitWorkspace(t *testing.T) {
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		workspace, _ := runfixture.FixtureRepo(t)
		workspace, err := filepath.EvalSymlinks(workspace)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Workspace, _ = runfixture.FixtureRepo(t)
		body := "Selected-workspace review document"
		snapshot := &review.Snapshot{Version: 1, Kind: "plan", Title: "Fixture plan", Body: body, Workspace: workspace, CapturedAt: "2026-11-24T00:00:00Z", Files: []review.File{{Path: "review.md", Status: "added", After: body, Diff: body, AfterSHA256: review.Digest([]byte(body))}}}
		snapshot.ID = review.Identity(*snapshot)
		parent := filepath.Join(workspace, ".irrigator", "reviews")
		if err := os.MkdirAll(parent, 0700); err != nil {
			t.Fatal(err)
		}
		if err := review.WriteSnapshot(filepath.Join(parent, snapshot.ID), snapshot); err != nil {
			t.Fatal(err)
		}
		cs := connectDefinitionTest(t, cfg)
		queue := callParameterFixture(t, cs, mcpToolName(cfg, "review_list"), map[string]any{"workspace": workspace, "base": "main"})["queue"].(map[string]any)
		documents := queue["documents"].([]any)
		if len(documents) != 1 {
			t.Fatalf("document missing from selected workspace: %#v", documents)
		}
		id := documents[0].(map[string]any)["snapshot"]
		wrong, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "inspect_review", Arguments: map[string]any{"snapshot": id}})
		if err != nil || !wrong.IsError {
			t.Fatalf("omitting the selected workspace did not use the distinct default: %+v %v", wrong, err)
		}
		out := callParameterFixture(t, cs, "inspect_review", map[string]any{"workspace": queue["workspace"], "snapshot": id})
		if out["kind"] != "document" || out["review"].(map[string]any)["body"] != body {
			t.Fatalf("queue document did not round-trip its selected workspace: %#v", out)
		}
	})
}

func TestMCPSchemaReferencesUseSelectedProfile(t *testing.T) {
	forMCPProfiles(t, func(t *testing.T, cfg McpConfig) {
		cfg.Workspace = t.TempDir()
		cs := connectDefinitionTest(t, cfg)
		list, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range list.Tools {
			raw, err := json.Marshal(tool)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "{{") {
				t.Fatalf("unresolved reference in %s: %s", tool.Name, raw)
			}
			if cfg.canonicalNames {
				for legacy, canonical := range canonicalToolNames {
					// The inspection renames are unambiguous tool identifiers.
					// Bare control words such as "pause" also describe preserved
					// result values and events, so they are not reference checks.
					if legacy != canonical && strings.Contains(legacy, "_") && regexp.MustCompile(`\b`+regexp.QuoteMeta(legacy)+`\b`).Match(raw) {
						t.Fatalf("inactive reference %s in %s: %s", legacy, tool.Name, raw)
					}
				}
			}
		}
	})
}
