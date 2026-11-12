//go:build integration

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/clitest"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestDynamicGraphExecutionAndResume(t *testing.T) {
	nested := runfixture.SpawnTask(t, "branch", runfixture.Task("leaf", .01))
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", nested, runfixture.Task("sibling", .01)), runfixture.Task("join", .01, "parent"))
	_, done := clitest.Start(t, root)
	if err := <-done; err != nil {
		b, _ := json.MarshalIndent(runfixture.Read(t, root), "", "  ")
		t.Fatalf("graph exit: %v\n%s", err, b)
	}
	s := runfixture.Read(t, root)
	if len(s.Tasks) != 5 || s.Tasks["parent__branch__leaf"] == nil {
		t.Fatalf("wrong expanded graph: %+v", s.Dynamic)
	}
	seq := map[string]int{}
	for i, e := range s.Events {
		seq[e.Task+"/"+e.Kind] = i
	}
	if seq["join/started"] <= seq["parent/graph_verified"] || seq["parent/graph_verified"] <= seq["parent__branch/graph_verified"] {
		t.Fatal("join released before descendant graph completion")
	}
	_, done = clitest.Start(t, root)
	clitest.Finished(t, done, true)
	for id, r := range runfixture.Read(t, root).Tasks {
		if r.Attempt != 1 {
			t.Fatalf("%s replayed: %d", id, r.Attempt)
		}
	}
}
func TestExpansionCommitRecoveryAndIdempotency(t *testing.T) {
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0), runfixture.Task("b", 0, "a")))
	runfixture.StageExpansion(t, root, "parent")
	// Directory creation before the checkpoint transaction is harmless after a crash.
	if err := os.MkdirAll(filepath.Join(root, "cells", "parent__a"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := commitExpansion(root, "parent", "structural"); err != nil {
			t.Fatal(err)
		}
	}
	s := runfixture.Read(t, root)
	op := s.Tasks["parent__a"].OperationID
	if len(s.Dynamic) != 2 || len(s.Tasks["parent"].Children) != 2 || s.Tasks["parent"].Status != "waiting" {
		t.Fatal("duplicate or incomplete expansion")
	}
	if err := checkpoint.Update(root, func(s *model.State) error { s.Tasks["parent"].Status = "produced"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := commitExpansion(root, "parent", "structural"); err != nil {
		t.Fatal(err)
	}
	if runfixture.Read(t, root).Tasks["parent"].Status != "waiting" {
		t.Fatal("recovery left produced expansion stuck")
	}
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, true)
	if runfixture.Read(t, root).Tasks["parent__a"].OperationID != op {
		t.Fatal("recovery changed logical child operation")
	}
}
func TestSubgraphFailureAndScopedRetry(t *testing.T) {
	bad := runfixture.Task("bad", 0)
	bad.Equals = map[string]string{"answer": "required"}
	bad.MockResult = map[string]string{"answer": "wrong"}
	root := runfixture.New(t, runfixture.SpawnTask(t, "parent", runfixture.Task("good", 0), bad), runfixture.Task("join", 0, "parent"))
	_, done := clitest.Start(t, root)
	clitest.Finished(t, done, false)
	s := runfixture.Read(t, root)
	if s.Tasks["parent"].Status != "waiting" || s.Tasks["join"].Attempt != 0 {
		t.Fatal("failed child released graph join")
	}
	goodOp := s.Tasks["parent__good"].OperationID
	badOp := s.Tasks["parent__bad"].OperationID
	if err := retryNode(root, "parent", true, false); err != nil {
		t.Fatal(err)
	}
	s = runfixture.Read(t, root)
	if s.Tasks["parent__good"].OperationID != goodOp || s.Tasks["parent__good"].Status != "verified" || s.Tasks["parent__bad"].OperationID == badOp {
		t.Fatal("recursive recovery did not preserve verified child")
	}
}
