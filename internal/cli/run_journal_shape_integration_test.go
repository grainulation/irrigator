//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/buildinfo"
	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Mock-backend acceptance fixtures. No provider, network or paid call is used.
// Helpers (fixture, task, read, start, finished, dstat*) come from existing tests.

// A real dynamically admitted child: durable ownership, children list and
// ordered expanded/started/verified provenance. Uses the existing spawn contract.
func TestRunShapeSpawnedChildHasOwnershipAndEventProvenance(t *testing.T) {
	parent := model.Task{ID: "parent", Prompt: "plan", Requires: []string{"children"}, Spawn: &model.SpawnPolicy{Field: "children", MaxChildren: 2},
		MockResult: map[string]string{"children": `[{"id":"kid","prompt":"do it","requires":["answer"],"mock_result":{"answer":"ok"}}]`}}
	root := runfixture.New(t, parent)
	_, done := clitest.Start(t, root, "--workers", "2")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	child := s.Tasks["parent__kid"]
	if child == nil {
		t.Fatalf("child not admitted; tasks=%d", len(s.Tasks))
	}
	if child.Spec.Parent != "parent" || child.Status != "verified" || s.Tasks["parent"].Status != "verified" {
		t.Fatalf("child %#v parent %s", child, s.Tasks["parent"].Status)
	}
	if s.Tasks["parent"].ExpansionHash == "" {
		t.Fatal("parent has no durable expansion record")
	}
	listed := false
	for _, id := range s.Tasks["parent"].Children {
		listed = listed || id == "parent__kid"
	}
	if !listed {
		t.Fatalf("owner children list %v", s.Tasks["parent"].Children)
	}
	if len(s.Plan.Tasks) != 1 || len(s.AllTasks()) != 2 {
		t.Fatalf("original plan must stay immutable: plan=%d all=%d", len(s.Plan.Tasks), len(s.AllTasks()))
	}
	expanded, started, childDone, parentDone := -1, -1, -1, -1
	for i, e := range s.Events {
		switch {
		case e.Task == "parent" && strings.Contains(e.Kind, "expand") && expanded < 0:
			expanded = i
		case e.Task == "parent__kid" && e.Kind == "started":
			started = i
		case e.Task == "parent__kid" && e.Kind == "verified":
			childDone = i
		case e.Task == "parent" && e.Kind == "graph_verified":
			parentDone = i
		}
	}
	if expanded < 0 || started < 0 || childDone < 0 || parentDone < 0 || !(expanded < started && started < childDone && childDone < parentDone) {
		t.Fatalf("event order expanded=%d started=%d child_verified=%d parent_verified=%d", expanded, started, childDone, parentDone)
	}
}

// Default scheduling stays silent: no breadth event or state key unless a caller
// explicitly opted in. Also checks journal ordering and dependency release.
func TestRunJournalDefaultIsOrderedAndSchedulingSilent(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", .01), runfixture.Task("b", .01, "a"), runfixture.Task("c", .01))
	_, done := clitest.Start(t, root, "--workers", "1")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, root)
	if len(s.Events) == 0 || s.Events[0].Kind != "created" {
		t.Fatalf("first event %#v", s.Events)
	}
	first := map[string]int{}
	last := map[string]int{}
	for i, e := range s.Events {
		if i > 0 && e.Time.Before(s.Events[i-1].Time) {
			t.Fatalf("event %d goes back in time", i)
		}
		if e.Kind == "scheduling_order" {
			t.Fatal("default run journaled a scheduling_order event")
		}
		if e.Kind == "started" {
			if _, seen := first[e.Task]; !seen {
				first[e.Task] = i
			}
		}
		if e.Kind == "verified" {
			last[e.Task] = i
		}
	}
	for id := range s.Tasks {
		if _, ok := first[id]; !ok || first[id] > last[id] {
			t.Fatalf("%s started=%v verified=%v", id, first[id], last[id])
		}
	}
	if first["b"] < last["a"] {
		t.Fatal("dependent started before its dependency verified")
	}
	raw, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"scheduling"`) {
		t.Fatal("default state carries a scheduling key")
	}
}

// Verified graph output is not delivery: with no delivery journal the stage is
// not_configured even when every node is verified and no file was extracted.
func TestRunShapeVerifiedGraphIsNotDelivery(t *testing.T) {
	e := dstatSetup(t)
	p, err := checkpoint.LoadPlan(e.plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = checkpoint.Initialize(p, e.root, "mock"); err != nil {
		t.Fatal(err)
	}
	_, done := clitest.Start(t, e.root, "--workers", "1")
	clitest.Finished(t, done, true)
	s := runfixture.Read(t, e.root)
	if s.Tasks["ui"].Status != "verified" {
		t.Fatalf("ui %s", s.Tasks["ui"].Status)
	}
	if v := graph.ProjectDelivery(e.root, s); v.Stage != "not_configured" || v.Acceptance != "not_configured" {
		t.Fatalf("verified graph reported as delivery: %#v", v)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "index.html")); !os.IsNotExist(err) {
		t.Fatal("files extracted without deliver")
	}
}

// Deliver extracts the verified artifact and runs the acceptance check once; a
// repeat without --recheck must not blindly rerun the check.
func TestRunShapeDeliveryExtractsAndChecksOnce(t *testing.T) {
	e := dstatSetup(t)
	check := dstatCheck(t)
	if code, err := e.deliver(check, false); err != nil || code != 0 {
		t.Fatalf("deliver: %d %v", code, err)
	}
	data, err := os.ReadFile(filepath.Join(e.dir, "index.html"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("extracted %q %v", data, err)
	}
	if n := e.ran(t); n != 1 {
		t.Fatalf("check ran %d times", n)
	}
	v := graph.ProjectDelivery(e.root, runfixture.Read(t, e.root))
	if v.Stage == "not_configured" || v.Stage == "unknown" || v.Acceptance == "not_configured" {
		t.Fatalf("delivery not visible after deliver: %#v", v)
	}
	_, _ = e.deliver(check, false)
	if n := e.ran(t); n != 1 {
		t.Fatalf("check blindly rerun without --recheck: %d", n)
	}
}

// Consumer contract: documented lifecycle and platform commands exist; bad
// input is refused with exit 2 before any run exists.
func TestRunShapeConsumerCLIContract(t *testing.T) {
	for _, command := range []string{"plan PLAN", "apply PLAN", "resume DIR", "deliver PLAN", "retire RUN_ID", "delivery-status RUN_DIR", "debug DIR", "demo", "compose CONFIG", "backup create", "campaign init", "mailbox enqueue"} {
		if !strings.Contains(Usage, command) {
			t.Fatalf("usage lacks %q", command)
		}
	}
	if buildinfo.Version == "" {
		t.Fatal("empty version")
	}
	for _, args := range [][]string{{"no-such-command"}, {"apply"}, {"apply", "x.json", "--no-such-flag"}, {"campaign"}, {"mailbox"}, {"backup"}, {"compose"}, {"panel"}, {"panel", "--run", "missing-run"}} {
		if code, err := runCommand(args); err == nil || code != 2 {
			t.Fatalf("%v -> %d %v, want exit 2 with error", args, code, err)
		}
	}
}
