package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

func TestCellNamesRejectCaseAliasesBeforeInitialization(t *testing.T) {
	p := model.Plan{Tasks: []model.Task{task("A", 0), task("a", 0)}}
	root := filepath.Join(t.TempDir(), "run")
	err := Initialize(p, root, "mock")
	if err == nil || !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("expected cell collision rejection, got %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("rejected plan created run: %v", err)
	}
	if err := Validate(model.Plan{Tasks: []model.Task{task("A", 0), task("b", 0)}}); err != nil {
		t.Fatalf("distinct mixed-case names rejected: %v", err)
	}
}

func TestCheckpointRejectsHistoricalCaseAliasedCells(t *testing.T) {
	root := fixture(t, task("A", 0), task("b", 0))
	s := read(t, root)
	s.Plan.Tasks[1].ID = "a"
	row := s.Tasks["b"]
	delete(s.Tasks, "b")
	row.Spec = s.Plan.Tasks[1]
	s.Tasks["a"] = row
	encoded, _ := json.Marshal(s.Plan)
	s.PlanHash = fmt.Sprintf("%x", sha256.Sum256(encoded))
	if err := jsonio.AtomicJSON(filepath.Join(root, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(root); err == nil || !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("unsafe historical cells accepted: %v", err)
	}
}
