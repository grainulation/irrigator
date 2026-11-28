//go:build integration

package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func lifecycleStdio(t *testing.T, profile string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.Command(os.Args[0], "mcp", "--workspace", t.TempDir(), "--profile", profile, "--allow-control")
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "lifecycle-stdio", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestMCPLifecycleRealStdioCancelStopAssembleAndCheck(t *testing.T) {
	task := runfixture.Task("cell", 0)
	task.Requires = []string{"files"}
	root := runfixture.New(t, task)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	args := map[string]any{"run": root, "request_id": "cancel", "expected_run_id": s.RunID, "expected_plan_sha256": s.PlanHash, "expected_graph_revision": 0, "cell": "cell", "expected_operation_id": s.Tasks["cell"].OperationID}
	execution := lifecycleStdio(t, "execution")
	out, _ := mcpMustCall(t, execution, "cancel_cell", args)
	if out["cleanup_confirmed"] != false || runfixture.Read(t, root).Tasks["cell"].Status != "cancelled" {
		t.Fatal(out)
	}
	delete(args, "cell")
	delete(args, "expected_operation_id")
	args["request_id"] = "stop"
	mcpMustCall(t, execution, "stop_run", args)
	if runfixture.Read(t, root).Control != "down" {
		t.Fatal("stop did not record down")
	}
	files, _ := json.Marshal(map[string]string{"source.txt": "accepted mock output"})
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "complete"
		s.Tasks["cell"].Status = "verified"
		s.Tasks["cell"].CancelRequested = false
		s.Tasks["cell"].Result = map[string]string{"files": string(files)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delivery := lifecycleStdio(t, "delivery")
	args["request_id"] = "assembly"
	args["directory"] = filepath.Join(t.TempDir(), "candidate")
	args["nodes"] = []string{"cell"}
	args["field"] = "files"
	out, _ = mcpMustCall(t, delivery, "assemble_delivery", args)
	if out["status"] != "exported" || out["check_started"] != false || out["dispatched"] != false {
		t.Fatal(out)
	}
	dir := args["directory"].(string)
	delete(args, "directory")
	delete(args, "nodes")
	delete(args, "field")
	args["request_id"] = "check"
	args["expected_candidate_sha256"] = out["candidate_sha256"]
	args["check"] = []string{"sh", "-c", "echo ran >> count"}
	args["timeout_seconds"] = 5
	out, _ = mcpMustCall(t, delivery, "check_delivery", args)
	if out["status"] != "accepted" || out["dispatched"] != false {
		t.Fatal(out)
	}
	mcpMustCall(t, delivery, "check_delivery", args)
	count, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil || string(count) != "ran\n" {
		t.Fatal("checker repeated", string(count), err)
	}
}
