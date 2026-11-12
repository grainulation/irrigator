//go:build integration

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/dashboard"
	"github.com/grainulation/irrigator/internal/model"
)

func TestStatusWatchStopsAfterControllerCrash(t *testing.T) {
	root := fixture(t, task("a", 30))
	cmd, done := start(t, root)
	waitFor(t, root, func(s *model.State) bool { return s.Tasks["a"].Status == "running" })
	if err := dashboard.StatusWatchController(root); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	finished(t, done, false)
	before := read(t, root)
	if before.Control != "run" {
		t.Fatal("fixture did not retain stale run state")
	}
	result := make(chan error, 1)
	go func() {
		code, err := runCommand([]string{"status", root, "--watch", "--json", "--interval", "20ms"})
		if code != 1 {
			result <- nil
			return
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "no active controller") || !strings.Contains(err.Error(), "irrigator resume") {
			t.Fatalf("missing actionable controller diagnosis: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = checkpoint.SetControl(root, "down")
		t.Fatal("watch kept repainting a stale run")
	}
	after := read(t, root)
	if after.Control != before.Control || after.Tasks["a"].Attempt != before.Tasks["a"].Attempt {
		t.Fatal("watch changed execution state")
	}
}
