//go:build integration

package cli

import (
	"os"
	"testing"

	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestExecutableValidatorBlocksDependents(t *testing.T) {
	binary, _ := os.Executable()
	a := runfixture.Task("a", .01)
	a.Checks = []model.Check{{Kind: "command", Command: []string{binary, "_protocol_fixture", "reject"}}}
	root := runfixture.New(t, a, runfixture.Task("b", .01, "a"))
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, false)
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "failed" || s.Tasks["b"].Attempt != 0 {
		t.Fatal("validator failed to block dependent")
	}
}
