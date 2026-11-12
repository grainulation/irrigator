package debug

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func debugTestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			out[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		out[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDebugProjectUsesSharedRecoveryAndHidesErrors(t *testing.T) {
	secret := "sk-secret-do-not-print"
	s := &model.State{RunID: "r1", Control: "run", Backend: "mock", Tasks: map[string]*model.TaskState{
		"p":  {Spec: model.Task{ID: "p"}, Status: "waiting", Children: []string{"c1", "c2", "c3"}},
		"c1": {Spec: model.Task{ID: "c1", Parent: "p"}, Status: "uncertain", Attempt: 2, Generation: 1, Error: model.UncertainOutcomeMessage(&model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: "deadline"})},
		"c2": {Spec: model.Task{ID: "c2", Parent: "p"}, Status: "running"},
		"c3": {Spec: model.Task{ID: "c3", Parent: "p"}, Status: "failed", Error: secret},
	}}
	rep, err := debugProject(s, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.WorkersKnown || rep.WorkersActive != nil {
		t.Fatalf("lease probe must be skipped without a root: %+v", rep)
	}
	if rep.AttentionTotal != 3 || len(rep.Attention) != 3 || rep.Attention[0].ID != "c1" || rep.Attention[1].ID != "c3" || rep.Attention[2].ID != "p" {
		t.Fatalf("attention order/count wrong: %+v", rep.Attention)
	}
	c1 := rep.Attention[0]
	if c1.ErrorCode != "outcome_uncertain_deadline" || c1.Attempt != 2 || c1.Classification != model.RecoveryUncertain || !c1.ErrorRecorded {
		t.Fatalf("uncertain projection wrong: %+v", c1)
	}
	if rep.Attention[1].ErrorCode != "failed_error_not_projected" {
		t.Fatalf("failed projection wrong: %+v", rep.Attention[1])
	}
	p := rep.Attention[2]
	if p.Recovery == nil || p.Recovery.Classification != model.RecoveryUncertain || p.Recovery.Activity != model.RecoveryActive || len(p.Recovery.ActiveIDs) != 1 || p.Recovery.ActiveIDs[0] != "c2" {
		t.Fatalf("waiting owner must reuse lifecycleRecoveryFor: %+v", p.Recovery)
	}
	data, _ := json.Marshal(rep)
	if strings.Contains(string(data), secret) {
		t.Fatal("raw error text leaked into JSON")
	}
	if _, err := debugProject(s, "", "missing"); err == nil {
		t.Fatal("unknown node must error")
	}
}

func TestDebugRunReadOnlyOnRealRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	mk := func(id string) model.Task { return model.Task{ID: id, Prompt: "p", Requires: []string{"x"}} }
	if err := checkpoint.Initialize(model.Plan{Name: "debug", Tasks: []model.Task{mk("a"), mk("b"), mk("c")}}, root, "mock"); err != nil {
		t.Fatal(err)
	}
	secret := "sk-secret-do-not-print"
	if err := checkpoint.Update(root, func(s *model.State) error {
		s.Tasks["a"].Status, s.Tasks["a"].Error = "failed", secret
		s.Tasks["b"].Status, s.Tasks["b"].Error = "uncertain", model.UncertainOutcomeMessage(&model.OutcomeError{Outcome: model.OutcomeUncertain, Reason: "malformed_response"})
		s.Tasks["c"].Status = "verified"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := debugTestTree(t, root)
	var jsonOut, textOut, nodeOut bytes.Buffer
	if code, err := DebugRun([]string{root, "--json"}, &jsonOut); code != 0 || err != nil {
		t.Fatalf("json run: %d %v", code, err)
	}
	if code, err := DebugRun([]string{root}, &textOut); code != 0 || err != nil {
		t.Fatalf("text run: %d %v", code, err)
	}
	if code, err := DebugRun([]string{"--node", "c", root, "--json"}, &nodeOut); code != 0 || err != nil {
		t.Fatalf("node run: %d %v", code, err)
	}
	if after := debugTestTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatal("debug modified the run directory")
	}
	for _, out := range []string{jsonOut.String(), textOut.String(), nodeOut.String()} {
		if strings.Contains(out, secret) {
			t.Fatal("raw error text leaked")
		}
	}
	var rep debugReport
	if err := json.Unmarshal(jsonOut.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Schema != debugSchema || !rep.ReadOnly || rep.StatusCounts["failed"] != 1 || rep.StatusCounts["uncertain"] != 1 || rep.StatusCounts["verified"] != 1 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if len(rep.Attention) != 2 || rep.Attention[0].ID != "a" || rep.Attention[1].ErrorCode != "outcome_uncertain_malformed_response" {
		t.Fatalf("unexpected attention: %+v", rep.Attention)
	}
	if !strings.Contains(textOut.String(), "status=failed") || !strings.Contains(textOut.String(), "nothing was executed") {
		t.Fatalf("text diagnosis incomplete:\n%s", textOut.String())
	}
	var sel debugReport
	if err := json.Unmarshal(nodeOut.Bytes(), &sel); err != nil || sel.Selected == nil || sel.Selected.ID != "c" || sel.Selected.Status != "verified" {
		t.Fatalf("selected node wrong: %v %+v", err, sel.Selected)
	}
	if code, err := DebugRun([]string{root, "--node", "nope"}, &bytes.Buffer{}); code != 1 || err == nil {
		t.Fatalf("unknown node must exit 1: %d %v", code, err)
	}
}

func TestDebugRunUsage(t *testing.T) {
	var buf bytes.Buffer
	if code, err := DebugRun(nil, &buf); code != 2 || err == nil {
		t.Fatalf("missing run: %d %v", code, err)
	}
	if code, err := DebugRun([]string{"--bogus", "x"}, &buf); code != 2 || err == nil {
		t.Fatalf("unknown flag: %d %v", code, err)
	}
	if code, err := DebugRun([]string{"a", "b"}, &buf); code != 2 || err == nil {
		t.Fatalf("two runs: %d %v", code, err)
	}
	buf.Reset()
	if code, err := DebugRun([]string{"--help"}, &buf); code != 0 || err != nil || !strings.Contains(buf.String(), "irrigator debug") {
		t.Fatalf("help: %d %v %q", code, err, buf.String())
	}
	if code, err := DebugRun([]string{filepath.Join(t.TempDir(), "absent")}, &buf); code != 1 || err == nil {
		t.Fatalf("absent run: %d %v", code, err)
	}
}
