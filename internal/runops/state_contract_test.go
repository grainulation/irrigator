package runops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestPlanRejectsTyposAndTrailingData(t *testing.T) {
	for _, source := range []string{
		`{"name":"bad","tasks":[],"workerz":4}`,
		`{"name":"bad","tasks":[{"id":"a","prompt":"p","requires":["answer"]}]} {}`,
		`{"name":"bad","tasks":[{"id":"a","parent":"missing","prompt":"p","requires":["answer"]}]}`,
	} {
		path := filepath.Join(t.TempDir(), "plan.json")
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := checkpoint.LoadPlan(path); err == nil {
			t.Fatalf("accepted invalid input %s", source)
		}
	}
}

func TestPlanFromStdin(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(`{"name":"pipe","tasks":[{"id":"a","prompt":"p","requires":["answer"]}]}`)
	f.Seek(0, 0)
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	p, err := checkpoint.LoadPlan("-")
	if err != nil || p.Name != "pipe" {
		t.Fatalf("stdin: %v %#v", err, p)
	}
}

func TestRecoveryRechecksSnapshotUnderLease(t *testing.T) {
	for _, mode := range []string{"checks", "spawn", "down"} {
		t.Run(mode, func(t *testing.T) {
			node := runfixture.Task("a", 0)
			node.Checks = []model.Check{{Kind: "contains", Field: "answer", Value: "required"}}
			if mode == "spawn" {
				node.Checks = nil
				node.Spawn = &model.SpawnPolicy{Field: "answer", MaxChildren: 1}
			}
			root := runfixture.New(t, node)
			if err := checkpoint.Update(root, func(s *model.State) error { s.Control = "run"; s.Tasks["a"].Status = "running"; return nil }); err != nil {
				t.Fatal(err)
			}
			stale := runfixture.Read(t, root)
			if err := checkpoint.Update(root, func(s *model.State) error {
				s.Tasks["a"].Status = "produced"
				s.Tasks["a"].Result = map[string]string{"answer": "structurally valid but wrong"}
				if mode == "spawn" {
					s.Tasks["a"].Result["answer"] = `{"tasks":[{"id":"child","prompt":"work","requires":["answer"]}]}`
				}
				if mode == "down" {
					s.Control = "down"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := reconcileSnapshot(root, stale); err != nil {
				t.Fatal(err)
			}
			got := runfixture.Read(t, root)
			switch mode {
			case "checks":
				if got.Tasks["a"].Status != "failed" {
					t.Fatal("stale scan bypassed task check")
				}
			case "spawn":
				if got.Tasks["a"].Status != "waiting" || got.Tasks["a__child"] == nil {
					t.Fatal("stale scan bypassed expansion")
				}
			case "down":
				if got.Tasks["a"].Status != "produced" {
					t.Fatal("stale scan ignored down request")
				}
			}
		})
	}
}
