package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/model"
)

func TestPlanDiagnosticsIncludeSourceAndLocation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(p, []byte("{\n\"tasks\": ["), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPlan(p)
	if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "line 2, column 11") {
		t.Fatalf("missing useful source location: %v", err)
	}
}
func TestPlanCycleDiagnosticsIdentifyUnresolvedNodes(t *testing.T) {
	p := model.Plan{Tasks: []model.Task{{ID: "a", Prompt: "a", Requires: []string{"answer"}, DependsOn: []string{"b"}}, {ID: "b", Prompt: "b", Requires: []string{"answer"}, DependsOn: []string{"a"}}}}
	err := Validate(p)
	if err == nil || !strings.Contains(err.Error(), "dependency cycle; unresolved nodes: a, b") {
		t.Fatalf("missing cycle nodes: %v", err)
	}
}
