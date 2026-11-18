//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

// Follow-up commands must work for a source checkout without installing the CLI
// on PATH, and must preserve literal paths when pasted into a shell.
func TestTerminalDemoPrintedInspectionCommands(t *testing.T) {
	t.Setenv("IRRIGATOR_LAUNCH_CONTEXT", "")
	t.Setenv("IRRIGATOR_SESSION_REGISTRY", "")
	t.Setenv("IRRIGATOR_UI", "plain")
	t.Setenv("IRRIGATOR_HINT", "expanded")
	dir := runfixture.CanonicalContextDir(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invocation := filepath.Join(dir, "irrigator ' $IRRIGATOR_HINT `literal`")
	if err := os.Symlink(binary, invocation); err != nil {
		t.Fatal(err)
	}
	oldInvocation, oldPlan := os.Args[0], terminalDemoPlan
	os.Args[0] = invocation
	terminalDemoPlan, _ = json.Marshal(model.Plan{APIVersion: "swarm/v1", Name: "demo hints", Tasks: []model.Task{
		{ID: "discover", Prompt: "Return a mock result", Requires: []string{"answer"}},
	}})
	defer func() { os.Args[0], terminalDemoPlan = oldInvocation, oldPlan }()
	root := filepath.Join(dir, "run ' $IRRIGATOR_HINT `literal`")
	var code int
	var runErr error
	out := runfixture.CaptureStdout(t, func() {
		code, runErr = terminalDemo([]string{"--run", root})
	})
	if code != 0 || runErr != nil {
		t.Fatalf("demo failed: code=%d err=%v\n%s", code, runErr, out)
	}
	_, hints, ok := strings.Cut(out, "Inspect this run:\n")
	if !ok {
		t.Fatalf("demo did not print inspection commands:\n%s", out)
	}
	commands := strings.SplitN(hints, "\n", 3)
	if len(commands) < 3 {
		t.Fatalf("demo inspection commands incomplete:\n%s", hints)
	}
	for _, command := range commands[:2] {
		cmd := exec.Command("/bin/sh", "-c", strings.TrimSpace(command))
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("printed command failed: %s\n%v\n%s", command, err, output)
		}
	}
}
