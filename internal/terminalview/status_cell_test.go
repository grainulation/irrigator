package terminalview

import (
	"strings"
	"testing"
)

func TestCellStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		status, state string
		ok            bool
	}{
		{"running", "working", true}, {"produced", "working", true}, {"verified", "done", true},
		{"failed", "error", true}, {"uncertain", "blocked", true}, {"cancelled", "idle", true},
		{"pending", "", false}, {"waiting", "", false}, {"", "", false},
	} {
		got, ok := CellStatus("c1", Row{Status: tc.status, Error: "secret detail"})
		if ok != tc.ok || got.State != tc.state {
			t.Errorf("status %q: got %+v ok=%v, want %q ok=%v", tc.status, got, ok, tc.state, tc.ok)
		}
		if strings.Contains(got.Message, "secret") {
			t.Errorf("status %q: message leaks the row error: %q", tc.status, got.Message)
		}
	}
}

func TestCellOSCCarriesID(t *testing.T) {
	st, _ := CellStatus("build:1=x", Row{Status: "running"})
	osc := st.OSC()
	if !strings.Contains(osc, ":id=build_1_x") || !strings.Contains(osc, "state=working") {
		t.Errorf("child record = %q", osc)
	}
	if strings.Contains((ProgramStatus{State: "done", Title: "t"}).OSC(), "id=") {
		t.Error("the root report has no id")
	}
	if got := CellClear("c1").OSC(); got != "\x1b]7501;state=idle:app=irrigator:id=c1\x1b\\" {
		t.Errorf("clear = %q", got)
	}
}

func TestCellStatusesPlanOrder(t *testing.T) {
	s := snap("run", "running", "pending", "verified")
	got := CellStatuses(s)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("statuses = %+v", got)
	}
}
