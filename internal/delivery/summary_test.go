package delivery

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func summaryFixture(t *testing.T, maps ...map[string]string) string {
	t.Helper()
	tasks := []model.Task{}
	for i := range maps {
		tasks = append(tasks, model.Task{ID: string(rune('a' + i)), Prompt: "make files", Requires: []string{"files"}})
	}
	root := runfixture.New(t, tasks...)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Control = "complete"
		for i, m := range maps {
			id := string(rune('a' + i))
			raw, _ := json.Marshal(m)
			s.Tasks[id].Status = "verified"
			s.Tasks[id].Attempt = 1
			s.Tasks[id].Validation = "structural"
			s.Tasks[id].Result = map[string]string{"files": string(raw)}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := runfixture.Read(t, root)
	nodes := []string{}
	for _, task := range tasks {
		nodes = append(nodes, task.ID)
	}
	if err := jsonio.AtomicJSON(filepath.Join(root, "delivery.json"), graph.DeliveryRecord{RunID: s.RunID, Nodes: nodes, Field: "files", Status: "accepted"}); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDeliverySummaryIdentityConflictsAndReadOnly(t *testing.T) {
	root := summaryFixture(t, map[string]string{"a.txt": "one"}, map[string]string{"b.txt": "two"})
	before, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Summarize(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.CandidateSHA256 == nil || got.Files != 2 || len(got.Included) != 2 || got.Acceptance != "not_configured" || got.Tasks[0].CheckOutcome != "passed_node_contract" {
		t.Fatalf("summary: %+v", got)
	}
	again, err := Summarize(root)
	if err != nil || *again.CandidateSHA256 != *got.CandidateSHA256 {
		t.Fatal("unstable candidate")
	}
	after, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("summary changed state")
	}
	conflictRoot := summaryFixture(t, map[string]string{"same": "one"}, map[string]string{"same": "two"})
	got, err = Summarize(conflictRoot)
	if err != nil || got.CandidateSHA256 != nil || len(got.Conflicts) != 1 || got.Conflicts[0].Kind != "different_content" {
		t.Fatalf("conflict: %+v %v", got, err)
	}
	fileDirRoot := summaryFixture(t, map[string]string{"a": "file"}, map[string]string{"a/b": "child"})
	got, err = Summarize(fileDirRoot)
	if err != nil || len(got.Conflicts) != 1 || got.Conflicts[0].Kind != "file_directory_collision" {
		t.Fatalf("file collision: %+v %v", got, err)
	}
}

func TestDeliverySummaryDoesNotClaimCheckOrStaleAcceptance(t *testing.T) {
	root := summaryFixture(t, map[string]string{"a": "one"})
	s := runfixture.Read(t, root)
	if err := jsonio.AtomicJSON(filepath.Join(root, "delivery.json"), graph.DeliveryRecord{RunID: s.RunID, GraphRevision: 1, Nodes: []string{"a"}, Field: "files", Status: "accepted", Check: []string{"false"}}); err != nil {
		t.Fatal(err)
	}
	got, err := Summarize(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Acceptance == "passed" || got.CandidateSHA256 != nil || len(got.Issues) == 0 {
		t.Fatalf("stale acceptance reused: %+v", got)
	}
}

func TestDeliverySummaryCheckOutcomesStopAtRecordedFailure(t *testing.T) {
	row := &model.TaskState{Spec: model.Task{Checks: []model.Check{{Kind: "contains"}, {Kind: "command"}, {Kind: "min_length"}}}, Status: "failed", Validation: "task-checks", Error: "check 1 (command): validator did not pass candidate"}
	got := projectCheckOutcomes(row)
	if got[0].Outcome != "passed" || got[1].Outcome != "failed" || got[2].Outcome != "not_run" {
		t.Fatalf("invented check outcomes: %+v", got)
	}
	row.Status = "uncertain"
	got = projectCheckOutcomes(row)
	for _, check := range got {
		if check.Outcome != "unknown" {
			t.Fatal("uncertain check became conclusive")
		}
	}
}
