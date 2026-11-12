//go:build integration

package cli

import (
	"testing"

	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestControllerStartsCriticalChainBeforeIndependentLeaf(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("leaf", .01), runfixture.Task("root", .01), runfixture.Task("end", .01, "root"))
	_, done := clitest.Start(t, root, "--workers", "1")
	clitest.Finished(t, done, true)
	for _, e := range runfixture.Read(t, root).Events {
		if e.Kind == "started" {
			if e.Task != "root" {
				t.Fatalf("first started %s, expected critical chain root", e.Task)
			}
			return
		}
	}
	t.Fatal("no work started")
}
