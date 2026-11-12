//go:build integration

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestControllerFatalReadErrorDrainsRealMockWorkers(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 10), runfixture.Task("b", 10))
	_, done := clitest.Start(t, root, "--workers", "2")
	waitFor(t, root, func(s *model.State) bool {
		return s.Tasks["a"].Status == "running" && s.Tasks["b"].Status == "running"
	})
	checkpoint := filepath.Join(root, "state.json")
	original, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(checkpoint, original, 0600) })
	if err = os.WriteFile(checkpoint, []byte("{broken checkpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	clitest.Finished(t, done, false)
	runfixture.RequireCellLeaseReleased(t, root, "a")
	runfixture.RequireCellLeaseReleased(t, root, "b")
}
