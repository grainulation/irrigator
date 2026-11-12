//go:build integration

package cli

import (
	"testing"

	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestExecutionProfilesCLIReadOnly(t *testing.T) {
	path, _, _ := runfixture.ExecutionFixture(t, "mock")
	for _, args := range [][]string{{"profiles", "inspect", "--catalog", path}, {"profiles", "resolve", "--catalog", path}, {"profiles", "--help"}} {
		if code, err := runCommand(args); err != nil || code != 0 {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	for _, args := range [][]string{{"profiles", "resolve", "--catalog", path, "--profile", "unknown"}, {"profiles", "inspect", "--catalog", path, "--profile", "small"}, {"profiles", "resolve", "--catalog", "relative.json"}, {"profiles", "run", "--catalog", path}} {
		if code, err := runCommand(args); err == nil || code == 0 {
			t.Fatalf("accepted unsupported command: %v", args)
		}
	}
}
