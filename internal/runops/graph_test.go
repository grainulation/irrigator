package runops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/managed"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestExpansionRejectsInvalidProposals(t *testing.T) {
	tests := map[string]func(*model.State, *model.TaskState){
		"trailing JSON":    func(s *model.State, r *model.TaskState) { r.Result["graph"] += ` {"tasks":[]}` },
		"task budget":      func(s *model.State, r *model.TaskState) { s.Plan.MaxTasks = 1 },
		"child budget":     func(s *model.State, r *model.TaskState) { r.Spec.Spawn.MaxChildren = 1 },
		"missing parent":   func(s *model.State, r *model.TaskState) { r.Spec.Parent = "absent" },
		"cyclic ownership": func(s *model.State, r *model.TaskState) { r.Spec.Parent = r.Spec.ID },
		"provider escalation": func(s *model.State, r *model.TaskState) {
			r.Result["graph"] = `{"tasks":[{"id":"x","prompt":"x","requires":["answer"],"provider":"haiku"}]}`
		},
		"command injection": func(s *model.State, r *model.TaskState) {
			r.Result["graph"] = `{"tasks":[{"id":"x","prompt":"x","requires":["answer"],"checks":[{"kind":"command","command":["bad"]}]}]}`
		},
		"sibling cycle": func(s *model.State, r *model.TaskState) {
			r.Result["graph"] = `{"tasks":[{"id":"x","prompt":"x","requires":["answer"],"depends_on":["x"]}]}`
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0), runfixture.Task("b", 0)))
			runfixture.StageExpansion(t, root, "parent")
			s := runfixture.Read(t, root)
			r := s.Tasks["parent"]
			mutate(s, r)
			if _, _, err := graph.Expansion(s, r); err == nil {
				t.Fatal("accepted invalid expansion")
			}
		})
	}
}

func TestOwnershipRejectsCorruptCheckpoint(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "hash", "cycle"} {
		t.Run(kind, func(t *testing.T) {
			root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0)))
			runfixture.StageExpansion(t, root, "parent")
			if err := commitExpansion(root, "parent", "structural"); err != nil {
				t.Fatal(err)
			}
			s := runfixture.Read(t, root)
			switch kind {
			case "missing":
				s.Tasks["parent"].Children = nil
			case "duplicate":
				s.Tasks["parent"].Children = append(s.Tasks["parent"].Children, "parent__a")
			case "hash":
				s.Tasks["parent"].Result["graph"] = "changed"
			case "cycle":
				s.Tasks["parent"].Spec.Parent = "parent__a"
			}
			if err := checkpoint.ValidateOwnership(s); err == nil {
				t.Fatal("accepted corrupted ownership")
			}
			if len(managed.Descendants(s, "parent")) > len(s.Tasks) {
				t.Fatal("unbounded traversal")
			}
		})
	}
}

func TestLostNodeRecoveryOperationIdentity(t *testing.T) {
	for _, policy := range []string{"safe", "manual"} {
		t.Run(policy, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "run")
			p := model.Plan{Name: "recover", Tasks: []model.Task{runfixture.Task("a", 0)}, Providers: map[string]model.CommandSpec{"external": {Command: []string{"unused"}, Replay: policy}}}
			if err := checkpoint.Initialize(p, root, "external"); err != nil {
				t.Fatal(err)
			}
			op := runfixture.Read(t, root).Tasks["a"].OperationID
			if err := checkpoint.Update(root, func(s *model.State) error { r := s.Tasks["a"]; r.Status = "running"; r.Attempt = 1; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := Reconcile(root); err != nil {
				t.Fatal(err)
			}
			r := runfixture.Read(t, root).Tasks["a"]
			want := "pending"
			if policy == "manual" {
				want = "uncertain"
			}
			if r.Status != want || r.OperationID != op || r.Attempt != 1 {
				t.Fatal("crash changed logical operation or unsafe recovery policy")
			}
			if policy == "manual" {
				if err := retryNode(root, "a", false, false); err == nil {
					t.Fatal("uncertain operation retried implicitly")
				}
			}
			if err := retryNode(root, "a", false, true, true); err != nil {
				t.Fatal(err)
			}
			r = runfixture.Read(t, root).Tasks["a"]
			if r.OperationID == op || r.Generation != 2 || r.Status != "pending" {
				t.Fatal("explicit retry did not create new operation")
			}
		})
	}
}

func TestNestedSpawnHonorsDepthLimit(t *testing.T) {
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.SpawnTask(t, "branch", runfixture.Task("leaf", 0))))
	runfixture.StageExpansion(t, root, "parent")
	if err := commitExpansion(root, "parent", "structural"); err != nil {
		t.Fatal(err)
	}
	runfixture.StageExpansion(t, root, "parent__branch")
	s := runfixture.Read(t, root)
	s.Plan.MaxDepth = 1
	if _, _, err := graph.Expansion(s, s.Tasks["parent__branch"]); err == nil {
		t.Fatal("nested spawn exceeded depth cap")
	}
}

func TestResolveRequiresConfirmedProcessCleanup(t *testing.T) {
	for _, marker := range []string{`{"status":"active","supervisor_pid":99999999}`, `broken marker`} {
		t.Run(marker, func(t *testing.T) {
			root := runfixture.New(t, runfixture.Task("a", 0))
			if err := checkpoint.Update(root, func(s *model.State) error { r := s.Tasks["a"]; r.Status = "uncertain"; r.Attempt = 1; return nil }); err != nil {
				t.Fatal(err)
			}
			before := runfixture.Read(t, root).Tasks["a"]
			path := filepath.Join(root, "cells", "a", "1.process.json")
			if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			result := map[string]string{"answer": "reconciled external output"}
			if err := ResolveNode(root, "a", result); err == nil {
				t.Fatal("resolved while provider cleanup remained unconfirmed")
			}
			blocked := runfixture.Read(t, root).Tasks["a"]
			if blocked.Status != "uncertain" || blocked.Result != nil {
				t.Fatal("blocked resolution mutated checkpoint")
			}
			if err := ResolveNode(root, "a", result, true); err != nil {
				t.Fatal(err)
			}
			after := runfixture.Read(t, root).Tasks["a"]
			if after.Status != "verified" || after.Attempt != before.Attempt || after.OperationID != before.OperationID || after.Result["answer"] != result["answer"] {
				t.Fatalf("resolution replayed or lost outcome: %+v", after)
			}
			if active, err := launch.UnresolvedProcess(filepath.Dir(path), 1); active || err != nil {
				t.Fatalf("cleanup remains unresolved: %v %v", active, err)
			}
		})
	}
}
