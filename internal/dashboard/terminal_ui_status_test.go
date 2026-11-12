package dashboard

import (
	"slices"
	"strings"
	"testing"

	"github.com/grainulation/irrigator/internal/executor"
	"github.com/grainulation/irrigator/internal/terminalview"
)

func cellRecords(statuses ...string) []terminalview.ProgramStatus {
	var out []terminalview.ProgramStatus
	for i, s := range statuses {
		if st, ok := terminalview.CellStatus(string(rune('a'+i)), terminalview.Row{Status: s}); ok {
			out = append(out, st)
		}
	}
	return out
}

func TestTerminalUICellReportsOnlyChanges(t *testing.T) {
	u := &TerminalUI{}
	first := u.cellReports(cellRecords("running", "pending"))
	if strings.Count(first, "\x1b]7501;") != 1 || !strings.Contains(first, "id=a") {
		t.Fatalf("first = %q", first)
	}
	if again := u.cellReports(cellRecords("running", "pending")); again != "" {
		t.Errorf("unchanged records re-sent: %q", again)
	}
	next := u.cellReports(cellRecords("verified", "running"))
	if strings.Count(next, "\x1b]7501;") != 2 || !strings.Contains(next, "state=done:app=irrigator:id=a") {
		t.Errorf("next = %q", next)
	}
	// A cell that stops being reported (retry) is cleared once.
	cleared := u.cellReports(cellRecords("verified", "pending"))
	if cleared != terminalview.CellClear("b").OSC() {
		t.Errorf("clear = %q", cleared)
	}
	if again := u.cellReports(cellRecords("verified", "pending")); again != "" {
		t.Errorf("clear re-sent: %q", again)
	}
}

func TestTerminalUIClearCellsLeavesFinishedRecords(t *testing.T) {
	u := &TerminalUI{}
	u.cellReports(cellRecords("verified", "running", "uncertain"))
	if got := u.clearCells(); got != terminalview.CellClear("b").OSC() {
		t.Errorf("clearCells = %q, want only the working cell cleared", got)
	}
}

func TestHerdrWorkerEnvEnablesProgress(t *testing.T) {
	got := executor.HerdrWorkerEnv([]string{"PATH=/bin", "IRRIGATOR_WORKER_PROGRESS=0", "ANTHROPIC_API_KEY=secret"})
	want := []string{"PATH=/bin", "IRRIGATOR_WORKER_PROGRESS=1"}
	if !slices.Equal(got, want) {
		t.Fatalf("env %v, want %v", got, want)
	}
}

func runReport(state string, progress int) terminalview.ProgramStatus {
	return terminalview.ProgramStatus{State: state, Progress: progress, Title: "irrigator r", Message: "1/2 verified · USD 0.10", Counts: "1/2 verified"}
}

func TestTerminalUIRunSignals(t *testing.T) {
	u := &TerminalUI{notifications: true, notifyFlavor: terminalview.NotifyOSC9}
	first := u.runSignals(runReport("working", 50))
	if !strings.HasPrefix(first, terminalview.TitlePush) || !strings.Contains(first, "\x1b]2;irrigator r · 1/2 verified\x1b\\") ||
		!strings.Contains(first, "\x1b]9;4;1;50\x1b\\") {
		t.Fatalf("first = %q", first)
	}
	if strings.Contains(first, "\x1b]9;irrigator") {
		t.Errorf("first report notified: %q", first)
	}
	if again := u.runSignals(runReport("working", 50)); again != "" {
		t.Errorf("unchanged signals re-sent: %q", again)
	}
	done := u.runSignals(runReport("done", 0))
	if strings.Count(done, terminalview.TitlePush) != 0 || !strings.Contains(done, terminalview.ProgressClear) ||
		!strings.Contains(done, "\x1b]9;irrigator r: run complete · 1/2 verified · USD 0.10\x1b\\") {
		t.Errorf("done = %q", done)
	}
	if got := u.closeSignals(); got != terminalview.TitlePop {
		t.Errorf("close after done = %q, want only the title restored", got)
	}
}

func TestTerminalUICloseClearsProgress(t *testing.T) {
	u := &TerminalUI{}
	u.runSignals(runReport("working", 10))
	if got := u.closeSignals(); got != terminalview.ProgressClear+terminalview.TitlePop {
		t.Errorf("close = %q", got)
	}
}

func TestTerminalUINotificationsOff(t *testing.T) {
	u := &TerminalUI{}
	u.runSignals(runReport("working", 10))
	if got := u.runSignals(runReport("blocked", 10)); strings.Contains(got, "\x1b]9;irrigator") || strings.Contains(got, "\x1b]777;") {
		t.Errorf("notified while off: %q", got)
	}
}

func TestTerminalUICellNotices(t *testing.T) {
	u := &TerminalUI{notifications: true, notifyFlavor: terminalview.NotifyOSC777}
	// First render: a cell that is already uncertain does not alert.
	if got := u.cellNotices("irrigator r", cellRecords("uncertain")); got != "" {
		t.Errorf("first render notified: %q", got)
	}
	u.runSignals(runReport("working", 0))
	u.cellReports(cellRecords("running"))
	got := u.cellNotices("irrigator r", cellRecords("uncertain"))
	if !strings.HasPrefix(got, "\x1b]777;notify;irrigator r;a: outcome uncertain, needs a decision") {
		t.Errorf("notice = %q", got)
	}
	u.cellReports(cellRecords("uncertain"))
	if again := u.cellNotices("irrigator r", cellRecords("uncertain")); again != "" {
		t.Errorf("re-notified: %q", again)
	}
}
