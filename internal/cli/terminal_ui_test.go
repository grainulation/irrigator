package cli

import (
	"encoding/json"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func TestTerminalDemoPlanIsFreeAndValid(t *testing.T) {
	var p model.Plan
	if err := json.Unmarshal(terminalDemoPlan, &p); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Validate(p); err != nil {
		t.Fatal(err)
	}
	for _, task := range p.Tasks {
		if task.Provider != "mock" {
			t.Fatalf("demo task %s may incur spend", task.ID)
		}
	}
}
