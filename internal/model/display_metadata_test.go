package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDisplayPublicationInstructionsMapOutcomeField(t *testing.T) {
	if DisplayPublicationInstructions(Task{}) != nil || DisplayPublicationInstructions(Task{Display: &TaskDisplay{Title: "x"}}) != nil {
		t.Fatal("instructions require an explicit outcome field")
	}
	b, err := json.Marshal(DisplayPublicationInstructions(Task{Display: &TaskDisplay{OutcomeField: "public_summary"}}))
	if err != nil || !strings.Contains(string(b), `"public_outcome_field":"public_summary"`) || !strings.Contains(string(b), "160") {
		t.Fatalf("missing field mapping: %s %v", b, err)
	}
}
