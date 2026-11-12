package runops

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/graph"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
)

func TestExportCaseAliasesRejectedBeforePublication(t *testing.T) {
	for _, paths := range []map[string]string{
		{"answer": "SWARM-EXPORT.JSON"}, {"answer": "Swarm-Export.Json/nested"},
		{"answer": "Result.txt", "other": "result.TXT"}, {"answer": "SRC", "other": "src/file.txt"},
	} {
		node := runfixture.Task("a", 0)
		node.Requires = []string{"answer", "other"}
		node.Outputs = paths
		if checkpoint.Validate(model.Plan{Tasks: []model.Task{node}}) == nil {
			t.Fatalf("plan accepted alias mapping %v", paths)
		}
		root := acceptedFixture(t, nil)
		s := runfixture.Read(t, root)
		s.Plan.Tasks[0].Outputs = paths
		s.Tasks["a"].Spec.Outputs = paths
		raw, _ := json.Marshal(s.Plan)
		s.PlanHash = fmt.Sprintf("%x", sha256.Sum256(raw))
		if err := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(filepath.Join(root, "state.json"))
		dest := filepath.Join(t.TempDir(), "out")
		if ExportArtifacts(root, "", dest) == nil {
			t.Fatalf("export accepted alias mapping %v", paths)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatal("rejected export published destination")
		}
		after, _ := os.ReadFile(filepath.Join(root, "state.json"))
		if string(before) != string(after) {
			t.Fatal("export mutated accepted evidence")
		}
	}
}

func TestExportFilesystemUnicodeAliasNeverOverwrites(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "caf\u00e9")
	second := filepath.Join(parent, "cafe\u0301")
	if err := os.WriteFile(first, []byte("probe"), 0600); err != nil {
		t.Fatal(err)
	}
	a, _ := os.Stat(first)
	b, err := os.Stat(second)
	aliases := err == nil && os.SameFile(a, b)
	root := acceptedFixture(t, nil)
	s := runfixture.Read(t, root)
	paths := map[string]string{"answer": "caf\u00e9.txt", "other": "cafe\u0301.txt"}
	s.Plan.Tasks[0].Outputs = paths
	s.Tasks["a"].Spec.Outputs = paths
	s.Tasks["a"].Result["other"] = "second accepted artifact"
	raw, _ := json.Marshal(s.Plan)
	s.PlanHash = fmt.Sprintf("%x", sha256.Sum256(raw))
	if err := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(parent, "out")
	err = ExportArtifacts(root, "", dest)
	if aliases {
		if err == nil {
			t.Fatal("normalized alias overwrote an accepted artifact")
		}
		if _, e := os.Lstat(dest); !os.IsNotExist(e) {
			t.Fatal("published partial alias export")
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func TestExpansionRejectsArchivedCaseAlias(t *testing.T) {
	parent := runfixture.SpawnTask(t, "parent", runfixture.Task("a", 0))
	s := &model.State{Plan: model.Plan{Tasks: []model.Task{parent}}, Tasks: map[string]*model.TaskState{}, ArchivedTasks: []model.TaskState{{Spec: runfixture.Task("parent__A", 0)}}}
	row := &model.TaskState{Spec: parent, Result: parent.MockResult}
	s.Tasks[parent.ID] = row
	if _, _, err := graph.Expansion(s, row); err == nil || !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("archived alias expansion accepted: %v", err)
	}
}

func TestHistoricalCaseAliasReadLeavesCheckpointUntouched(t *testing.T) {
	root := runfixture.New(t, runfixture.Task("a", 0))
	s := runfixture.Read(t, root)
	s.ArchivedTasks = append(s.ArchivedTasks, model.TaskState{Spec: runfixture.Task("A", 0)})
	path := filepath.Join(root, "state.json")
	if err := jsonio.AtomicJSON(path, s); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if _, err := checkpoint.ReadState(root); err == nil || !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("historical alias not rejected at cell boundary: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("rejection rewrote historical checkpoint")
	}
}
