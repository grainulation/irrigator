package terminalview

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

// ProgramStatus is one OSC 7501 (Program Status Protocol) report. Terminals
// and agent workspaces that support it, such as Ghostty and herdr-core, show
// the run's state without reading the screen. Others ignore the sequence.
type ProgramStatus struct {
	State    string // idle, working, done, blocked or error
	Progress int    // 0..100; only sent while working
	Title    string
	Message  string
	// Counts is Message without the cost, short enough for a tab title.
	Counts string
	// ID marks a child record for one worker cell. Empty is the run's root report.
	ID string
}

// Status maps a run snapshot to a report. ok is false for states with no
// meaningful report, such as a run that has not started.
func Status(s Snapshot, root string) (ProgramStatus, bool) {
	total, verified, running, failed := 0, 0, 0, 0
	for _, t := range s.Tasks {
		total++
		r, ok := s.row(t.ID)
		if !ok {
			continue
		}
		switch r.Status {
		case "verified":
			verified++
		case "running", "produced":
			running++
		case "failed", "uncertain":
			failed++
		}
	}
	parts := []string{fmt.Sprintf("%d/%d verified", verified, total)}
	if running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", running))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d need attention", failed))
	}
	counts := strings.Join(parts, " · ")
	if s.Cost != "" {
		parts = append(parts, s.Cost)
	}
	st := ProgramStatus{Title: "irrigator " + filepath.Base(root), Message: strings.Join(parts, " · "), Counts: counts}
	switch s.Control {
	case "run":
		st.State = "working"
		if total > 0 {
			st.Progress = verified * 100 / total
		}
	case "complete":
		st.State = "done"
	case "blocked":
		// Blocked runs wait for a person: retry, resolve or raise the budget.
		st.State = "blocked"
	case "pause", "paused", "down":
		st.State = "idle"
	default:
		return ProgramStatus{}, false
	}
	return st, true
}

// OSC encodes a report as ESC ] 7501 ; key=value:... ESC \.
// Title and message are base64, as the protocol requires for free text.
func (p ProgramStatus) OSC() string {
	fields := []string{"state=" + p.State, "app=irrigator"}
	if p.ID != "" {
		fields = append(fields, "id="+safeID(p.ID))
	}
	if p.State == "working" {
		fields = append(fields, fmt.Sprintf("progress=%d", clamp(p.Progress, 0, 100)))
	}
	if p.Title != "" {
		fields = append(fields, "title="+base64.StdEncoding.EncodeToString([]byte(p.Title)))
	}
	if p.Message != "" {
		fields = append(fields, "msg="+base64.StdEncoding.EncodeToString([]byte(p.Message)))
	}
	return "\x1b]7501;" + strings.Join(fields, ":") + "\x1b\\"
}

// safeID keeps a cell id inside one ':'-separated, unquoted field.
func safeID(id string) string {
	return strings.Map(func(r rune) rune {
		if r == ':' || r == '=' || r < 0x21 || r > 126 {
			return '_'
		}
		return r
	}, id)
}

// CellStatus maps one cell's row to a child record. ok is false for cells with
// nothing to show yet (pending, waiting, unknown). Blocked is reserved for
// cells whose outcome is uncertain and so needs a person to resolve it; a
// plain failure is an error. Messages carry only the status, never output.
func CellStatus(id string, r Row) (ProgramStatus, bool) {
	st := ProgramStatus{ID: id, Title: id}
	switch r.Status {
	case "running":
		st.State, st.Message = "working", "running"
	case "produced":
		st.State, st.Message = "working", "verifying"
	case "verified":
		st.State, st.Message = "done", "verified"
	case "failed":
		st.State, st.Message = "error", "failed"
	case "uncertain":
		st.State, st.Message = "blocked", "outcome uncertain; needs a decision"
	case "cancelled":
		st.State, st.Message = "idle", "cancelled"
	default:
		return ProgramStatus{}, false
	}
	return st, true
}

// CellClear is the record that withdraws a cell that is no longer reported.
func CellClear(id string) ProgramStatus {
	return ProgramStatus{ID: id, State: "idle"}
}

// CellStatuses returns the child records for every reportable task, in plan order.
func CellStatuses(s Snapshot) []ProgramStatus {
	var out []ProgramStatus
	for _, t := range s.Tasks {
		if r, ok := s.row(t.ID); ok {
			if st, ok := CellStatus(t.ID, r); ok {
				out = append(out, st)
			}
		}
	}
	return out
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
