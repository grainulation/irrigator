//go:build integration

package cli

import (
	"testing"

	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestMalformedFileArtifactBlocksDependent(t *testing.T) {
	a := model.Task{ID: "a", Prompt: "produce files", Requires: []string{"files"}, FileFields: []string{"files"}, MockResult: map[string]string{"files": `{"bad":"unterminated}`}}
	root := runfixture.New(t, a, runfixture.Task("consumer", 0, "a"))
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, false)
	s := runfixture.Read(t, root)
	if s.Tasks["a"].Status != "failed" || s.Tasks["consumer"].Attempt != 0 {
		t.Fatal("malformed artifact released dependent")
	}
}
