package sizing

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
)

func TestTurnShapeConfirmedCounts(t *testing.T) {
	for _, tt := range []struct {
		input                  string
		agents, calls, helpers int
		sentinel               bool
	}{
		{"1x1x1", 1, 1, 0, true}, {"2x2x1", 2, 4, 0, true},
		{"1x1x2", 3, 3, 2, false}, {"2x2x2", 10, 12, 2, false},
		{"2×2×0", 2, 4, 0, false}, {" 2X2X2 ", 10, 12, 2, false},
	} {
		t.Run(tt.input, func(t *testing.T) {
			r, e := model.ParseTurnShape(tt.input)
			if e != nil {
				t.Fatal(e)
			}
			if r.MaxAgents != tt.agents || r.MaxExecutions != tt.calls || r.Shape.HelpersPerTurn != tt.helpers || r.LegacyOneSentinel != tt.sentinel {
				t.Fatalf("unexpected report: %+v", r)
			}
			if r.APIVersion != model.TurnShapeVersion {
				t.Fatal("missing version")
			}
		})
	}
}
func TestTurnShapeCanonicalOneHelperIsNotSentinel(t *testing.T) {
	r, e := model.ReportTurnShape(model.TurnShape{Roots: 2, MainTurns: 2, HelpersPerTurn: 1})
	if e != nil {
		t.Fatal(e)
	}
	if r.MaxAgents != 6 || r.MaxExecutions != 8 || r.LegacyOneSentinel {
		t.Fatalf("canonical one helper lost: %+v", r)
	}
	raw, e := json.Marshal(r.Shape)
	if e != nil {
		t.Fatal(e)
	}
	var shape model.TurnShape
	if e = json.Unmarshal(raw, &shape); e != nil || shape.HelpersPerTurn != 1 {
		t.Fatalf("canonical round trip: %s %v", raw, e)
	}
}
func TestTurnShapeRejectsInvalidAndOverflow(t *testing.T) {
	for _, input := range []string{"", "2x2", "222", "2x2x2x2", "0x1x0", "1x0x0", "1x17x0", "-1x2x2", "+1x2x2", "1x2x-1", "1x 2x2", "1x2x2.5", "1x2x", "999999999999999999999999x1x0", "1025x1x0", "1x1x1024", "2x16x32"} {
		if r, e := model.ParseTurnShape(input); e == nil {
			t.Errorf("accepted %q: %+v", input, r)
		}
	}
	for _, shape := range []model.TurnShape{{Roots: math.MaxInt, MainTurns: 16, HelpersPerTurn: math.MaxInt}, {Roots: 1, MainTurns: 1, HelpersPerTurn: math.MaxInt}, {Roots: 1, MainTurns: math.MaxInt, HelpersPerTurn: 0}, {Roots: -1, MainTurns: 1, HelpersPerTurn: 0}, {Roots: 1, MainTurns: 1, HelpersPerTurn: -1}} {
		if r, e := model.ReportTurnShape(shape); e == nil {
			t.Errorf("accepted %+v: %+v", shape, r)
		}
	}
}
func TestTurnShapeBoundariesAndLegacyIsolation(t *testing.T) {
	for _, shape := range []model.TurnShape{{Roots: 1024, MainTurns: 1, HelpersPerTurn: 0}, {Roots: 64, MainTurns: 16, HelpersPerTurn: 0}, {Roots: 1, MainTurns: 1, HelpersPerTurn: 1023}, {Roots: 2, MainTurns: 16, HelpersPerTurn: 31}} {
		r, e := model.ReportTurnShape(shape)
		if e != nil || r.MaxExecutions != 1024 || r.MaxAgents > 1024 {
			t.Errorf("boundary %+v: %+v %v", shape, r, e)
		}
	}
	old, e := ParseShape("2x2x2")
	if e != nil {
		t.Fatal(e)
	}
	n, e := checkpoint.ShapeMaxNodes(old)
	if e != nil || n != 6 {
		t.Fatalf("legacy recursive shape changed: %d %v", n, e)
	}
	current, e := model.ParseTurnShape("2x2x2")
	if e != nil || current.MaxExecutions != 12 {
		t.Fatalf("new turn report: %+v %v", current, e)
	}
}
