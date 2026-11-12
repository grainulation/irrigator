//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/launch"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/session"
)

const (
	gcKillHelperLimit = 60 * time.Second
)

// TestGCKillHelperProcess is a subprocess body, inert unless the role marker
// is set by the parent test.
func TestGCKillHelperProcess(t *testing.T) {
	role := os.Getenv(gcKillRoleEnv)
	if role == "" {
		t.Skip("subprocess helper")
	}
	markers := os.Getenv(gcKillMarkersEnv)
	deadline := time.Now().Add(gcKillHelperLimit)
	switch role {
	case "controller":
		lock, err := checkpoint.FileLock(filepath.Join(os.Getenv(gcKillRunEnv), "controller.lock"), true)
		if err != nil {
			_ = gcKillPublish(filepath.Join(markers, "controller.err"), err.Error())
			os.Exit(3)
		}
		// Held until the kernel releases it at process death (KeepAlive below
		// prevents a finalizer from closing it early). It is close-on-exec, so
		// the child never inherits it.
		child := exec.Command(os.Args[0], gcKillHelperRun)
		child.Dir = os.Getenv(gcKillWorktreeEnv)
		child.Env = append(os.Environ(), gcKillRoleEnv+"=child")
		if err := child.Start(); err != nil {
			_ = gcKillPublish(filepath.Join(markers, "controller.err"), err.Error())
			os.Exit(4)
		}
		if _, ok := gcKillWaitFile(filepath.Join(markers, "child.ready"), 15*time.Second); !ok {
			_ = child.Process.Kill()
			_ = gcKillPublish(filepath.Join(markers, "controller.err"), "child not ready")
			os.Exit(5)
		}
		if err := gcKillPublish(filepath.Join(markers, "controller.ready"), strconv.Itoa(child.Process.Pid)); err != nil {
			os.Exit(6)
		}
		for time.Now().Before(deadline) { // bounded: the parent kills us long before
			time.Sleep(50 * time.Millisecond)
		}
		runtime.KeepAlive(lock)
		os.Exit(0)
	case "child":
		wd, err := os.Getwd()
		if err != nil {
			os.Exit(7)
		}
		if err := os.WriteFile(filepath.Join(wd, gcKillSentinel), []byte("owned by a continuing worker descendant\n"), 0o600); err != nil {
			os.Exit(8)
		}
		if err := gcKillPublish(filepath.Join(markers, "child.ready"), strconv.Itoa(os.Getpid())); err != nil {
			os.Exit(9)
		}
		for beat := 1; time.Now().Before(deadline); beat++ {
			if _, err := os.Stat(filepath.Join(markers, "stop")); err == nil {
				os.Exit(0)
			}
			if err := gcKillPublish(filepath.Join(markers, "heartbeat"), strconv.Itoa(beat)); err != nil {
				os.Exit(0) // markers removed: the parent test is gone
			}
			time.Sleep(20 * time.Millisecond)
		}
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

// Runs in a real provider subprocess. Checks the received identity and selected
// data before acknowledging, so this is not mere environment inheritance.
func TestContextProviderHelper(t *testing.T) {
	mode := os.Getenv("IRRIGATOR_CONTEXT_TEST_HELPER")
	if mode == "" {
		return
	}
	var req model.WorkRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Exit(21)
	}
	c := req.LaunchContext
	if c == nil || c.Schema != launch.WorkerContextSchema || c.RunID != req.RunID || c.TaskID != req.Task.ID || c.ParentID != req.Task.Parent || c.OperationID != req.OperationID || c.Attempt != req.Attempt || c.RequestID != req.RequestID || c.Kind != req.Kind || c.Digest != launch.WorkContextDigest(*c) {
		os.Exit(22)
	}
	if req.Context["allowed"] != "chosen" || req.Context["hidden"] != "" || len(c.ContextKeys) != 1 || c.ContextKeys[0] != "allowed" {
		os.Exit(23)
	}
	if mode == "capsule" && (c.Capsule == nil || c.Capsule.Schema != launch.LaunchContextSchema) {
		os.Exit(24)
	}
	output := map[string]string{"answer": req.Task.ID + "|" + req.Task.Parent + "|" + req.Context["allowed"]}
	if req.Task.Spawn != nil {
		output["children"] = `{"tasks":[{"id":"child","prompt":"Use selected context","requires":["answer"],"context":["allowed"]}]}`
	}
	receipt := &model.ContextReceipt{Schema: launch.ContextReceiptSchema, Digest: c.Digest, Consumer: "independent-local-fixture"}
	if mode == "mismatch" {
		receipt.Digest = strings.Repeat("0", 64)
	}
	if mode == "legacy" {
		receipt = nil
	}
	_ = json.NewEncoder(os.Stdout).Encode(model.WorkResponse{Protocol: model.ProtocolVersion, RequestID: req.RequestID, Output: output, ContextReceipt: receipt})
	os.Exit(0)
}

// changeTestRepo has main plus two agent branches that both touch app.go.
func changeTestRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	reviewTestGit(t, dir, "init", "-q", "-b", "main")
	write("app.go", "a\nb\nc\n")
	write("go.mod", "module x\n")
	reviewTestGit(t, dir, "add", ".")
	reviewTestGit(t, dir, "commit", "-qm", "base")
	reviewTestGit(t, dir, "switch", "-qc", "agent-1")
	write("app.go", "a\nB\nc\n")
	write("db/migrations/1.sql", "create table t();\n")
	reviewTestGit(t, dir, "add", ".")
	reviewTestGit(t, dir, "commit", "-qm", "agent 1")
	reviewTestGit(t, dir, "switch", "-q", "main")
	reviewTestGit(t, dir, "switch", "-qc", "agent-2")
	write("app.go", "a\nb\nc\nd\n")
	reviewTestGit(t, dir, "commit", "-qam", "agent 2")
	reviewTestGit(t, dir, "switch", "-q", "main")
	return dir
}

func reviewTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(session.ReviewEnv(), "GIT_AUTHOR_NAME=Review Test", "GIT_AUTHOR_EMAIL=review@example.test", "GIT_COMMITTER_NAME=Review Test", "GIT_COMMITTER_EMAIL=review@example.test")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}
