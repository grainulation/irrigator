//go:build integration

package cli

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestResumeRetryFailedExplainsManualProviderSkip(t *testing.T) {
	root := runfixture.HaikuRoot(t)
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["a"].Status = "failed"; return nil }); err != nil {
		t.Fatal(err)
	}
	binary, _ := os.Executable()
	cmd := exec.Command(binary, "resume", root, "--retry-failed")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("failed run unexpectedly succeeded")
	}
	if !strings.Contains(string(out), "not safe for automatic replay") || !strings.Contains(string(out), "irrigator retry") {
		t.Fatalf("missing actionable skip diagnostic: %s", out)
	}
	if runfixture.Read(t, root).Tasks["a"].Attempt != 0 {
		t.Fatal("paid backend retried automatically")
	}
}
