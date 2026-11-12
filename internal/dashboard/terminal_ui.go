package dashboard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/provider"
	"github.com/grainulation/irrigator/internal/terminalview"
)

// terminalUI is presentation only. It never changes scheduling or durable state.
type TerminalUI struct {
	out           io.Writer
	root          string
	started, last time.Time
	lines         int
	color         bool
	WatchOnly     bool
	errors        map[string]string
	// statusReports sends OSC 7501 run state to terminals that show it.
	statusReports bool
	lastStatus    string
	// cellStatus is the last OSC record sent per cell, so only changes are sent.
	cellStatus map[string]string
	// Companion signals: title (OSC 2), progress bar (OSC 9;4) and desktop
	// notifications (OSC 9 or 777). run is the last root report.
	run           terminalview.ProgramStatus
	lastTitle     string
	lastProgress  string
	titlePushed   bool
	notifyFlavor  string
	notifications bool
}

func NewTerminalUI(out *os.File, root string) *TerminalUI {
	info, err := out.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 || os.Getenv("TERM") == "dumb" || os.Getenv("IRRIGATOR_UI") == "plain" {
		return nil
	}
	u := &TerminalUI{out: out, root: root, started: time.Now(), color: os.Getenv("NO_COLOR") == "", errors: map[string]string{},
		statusReports: os.Getenv("IRRIGATOR_STATUS") != "off"}
	u.notifyFlavor, u.notifications = terminalview.NotifyFlavor(os.Getenv("IRRIGATOR_NOTIFY"), os.Getenv)
	return u
}
func terminalBound(name string, fallback, minimum, maximum int) int {
	n, e := strconv.Atoi(os.Getenv(name))
	if e != nil || n <= 0 {
		return fallback
	}
	if n < minimum {
		return minimum
	}
	if n > maximum {
		return maximum
	}
	return n
}
func (u *TerminalUI) Render(s *model.State, now time.Time) {
	if u == nil {
		return
	}
	for id, row := range s.Tasks {
		if row != nil && row.Error != "" {
			u.errors[id] = row.Error
		}
	}
	if !u.last.IsZero() && now.Sub(u.last) < 120*time.Millisecond && s.Control == "run" {
		return
	}
	u.last = now
	// Leave one column unused: writing the final column may trigger auto-wrap.
	width, height := terminalDimensions(u.out)
	frame := terminalFrame(s, u.root, now.Sub(u.started), width, height)
	if u.WatchOnly && len(frame) > 0 {
		frame[len(frame)-1] = terminalClip("Ctrl-C stops watching; running work continues", width)
	}
	var b strings.Builder
	if u.statusReports {
		snap := terminalSnapshot(s)
		cells := terminalview.CellStatuses(snap)
		st, ok := terminalview.Status(snap, u.root)
		if ok {
			if osc := st.OSC(); osc != u.lastStatus {
				b.WriteString(osc)
				u.lastStatus = osc
			}
			b.WriteString(u.cellNotices(st.Title, cells))
			b.WriteString(u.runSignals(st))
		}
		b.WriteString(u.cellReports(cells))
	}
	b.WriteString("\x1b[?25l")
	if u.lines > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", u.lines)
	}
	for _, line := range frame {
		b.WriteString("\r\x1b[2K")
		if u.color {
			b.WriteString(terminalLineColor(line))
		}
		b.WriteString(line)
		if u.color {
			b.WriteString("\x1b[0m")
		}
		b.WriteByte('\n')
	}
	for i := len(frame); i < u.lines; i++ {
		b.WriteString("\r\x1b[2K\n")
	}
	if extra := u.lines - len(frame); extra > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", extra)
	}
	u.lines = len(frame)
	fmt.Fprint(u.out, b.String())
}

// runSignals returns the title, progress bar and notification for a root
// report, each only when it changed.
func (u *TerminalUI) runSignals(st terminalview.ProgramStatus) string {
	var b strings.Builder
	if title := terminalview.TitleOSC(st.Title + " · " + st.Counts); title != u.lastTitle {
		if !u.titlePushed {
			b.WriteString(terminalview.TitlePush)
			u.titlePushed = true
		}
		b.WriteString(title)
		u.lastTitle = title
	}
	if p := terminalview.ProgressOSC(st); p != u.lastProgress {
		b.WriteString(p)
		u.lastProgress = p
	}
	if title, body, ok := terminalview.Notice(u.run, st); ok && u.notifications {
		b.WriteString(terminalview.NotifyOSC(u.notifyFlavor, title, body))
	}
	u.run = st
	return b.String()
}

// cellNotices notifies once when a cell newly needs a person. It must run
// before cellReports records the new states, and stays quiet on the first
// render so watching an existing run does not replay old alerts.
func (u *TerminalUI) cellNotices(runTitle string, cells []terminalview.ProgramStatus) string {
	if !u.notifications || u.run.State == "" {
		return ""
	}
	var b strings.Builder
	for _, c := range cells {
		prev := ""
		if strings.Contains(u.cellStatus[c.ID], "state=blocked") {
			prev = "blocked"
		}
		if title, body, ok := terminalview.CellNotice(runTitle, prev, c); ok {
			b.WriteString(terminalview.NotifyOSC(u.notifyFlavor, title, body))
		}
	}
	return b.String()
}

// closeSignals removes the progress bar and restores the saved title.
func (u *TerminalUI) closeSignals() string {
	var b strings.Builder
	if u.lastProgress != "" && u.lastProgress != terminalview.ProgressClear {
		b.WriteString(terminalview.ProgressClear)
	}
	if u.titlePushed {
		b.WriteString(terminalview.TitlePop)
	}
	return b.String()
}

// cellReports returns OSC child records for cells whose record changed, and a
// clear for cells that were reported but no longer are (for example a retry).
func (u *TerminalUI) cellReports(now []terminalview.ProgramStatus) string {
	if u.cellStatus == nil {
		u.cellStatus = map[string]string{}
	}
	var b strings.Builder
	seen := make(map[string]bool, len(now))
	for _, st := range now {
		seen[st.ID] = true
		if osc := st.OSC(); osc != u.cellStatus[st.ID] {
			b.WriteString(osc)
			u.cellStatus[st.ID] = osc
		}
	}
	var gone []string
	for id := range u.cellStatus {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		b.WriteString(terminalview.CellClear(id).OSC())
		delete(u.cellStatus, id)
	}
	return b.String()
}

// clearCells withdraws child records that have not reached a final state, so a
// stopped view leaves no stale "working" cells in the workspace.
func (u *TerminalUI) clearCells() string {
	var ids []string
	for id, osc := range u.cellStatus {
		if strings.Contains(osc, "state=working") {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(terminalview.CellClear(id).OSC())
	}
	u.cellStatus = nil
	return b.String()
}

func (u *TerminalUI) Close() {
	if u == nil {
		return
	}
	if u.statusReports {
		fmt.Fprint(u.out, u.clearCells())
		fmt.Fprint(u.out, u.closeSignals())
	}
	fmt.Fprint(u.out, "\x1b[0m\x1b[?25h")
	// Keep every observed diagnostic after the compact dashboard disappears.
	ids := make([]string, 0, len(u.errors))
	for id := range u.errors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(u.out, "%s: %s\n", terminalSafe(id), terminalSafe(u.errors[id]))
	}
	fmt.Fprint(u.out, u.locationLines())
}

// locationLines names the run directory and its per-cell logs. On a terminal
// with links on, each path opens in the file manager or editor.
func (u *TerminalUI) locationLines() string {
	f, _ := u.out.(*os.File)
	links := terminalview.LinksEnabled(f)
	abs, err := filepath.Abs(u.root)
	if err != nil {
		return ""
	}
	cells := filepath.Join(abs, "cells")
	return "run   " + terminalview.PathLink(links, abs, terminalSafe(abs)) + "\ncells " + terminalview.PathLink(links, cells, terminalSafe(cells)) + "\n"
}
func terminalSafe(s string) string {
	return terminalview.Safe(s)
}
func terminalClip(s string, width int) string {
	return terminalview.Clip(s, width)
}

// terminalSnapshot copies presentation data out of State.
func terminalSnapshot(s *model.State) terminalview.Snapshot {
	all := s.AllTasks()
	tasks := make([]terminalview.Task, len(all))
	for i, t := range all {
		tasks[i] = terminalview.Task{ID: t.ID, Parent: t.Parent, DependsOn: append([]string(nil), t.DependsOn...)}
	}
	rows := make(map[string]terminalview.Row, len(s.Tasks))
	for id, r := range s.Tasks {
		if r == nil {
			rows[id] = terminalview.Row{Nil: true}
			continue
		}
		rows[id] = terminalview.Row{Status: r.Status, Error: r.Error}
	}
	return terminalview.Snapshot{
		Control:       s.Control,
		GraphRevision: int64(s.GraphRevision),
		Cost:          provider.CostStatusText(s),
		Static:        os.Getenv("IRRIGATOR_UI") == "static",
		Tasks:         tasks,
		Rows:          rows,
	}
}

func terminalFrame(s *model.State, root string, elapsed time.Duration, width, height int) []string {
	return terminalview.Frame(terminalSnapshot(s), root, elapsed, width, height)
}

// TIOCGWINSZ is available on supported macOS/Linux terminals; no external stty.
func terminalDimensions(out io.Writer) (int, int) {
	width := terminalBound("COLUMNS", 80, 1, 240)
	height := terminalBound("LINES", 24, 1, 100)
	if f, ok := out.(interface{ Fd() uintptr }); ok {
		var size struct{ Rows, Columns, X, Y uint16 }
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
		if errno == 0 {
			if size.Columns > 0 {
				width = int(size.Columns)
			}
			if size.Rows > 0 {
				height = int(size.Rows)
			}
		}
	}
	if width > 240 {
		width = 240
	}
	if height > 100 {
		height = 100
	}
	width--
	height -= 2
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return width, height
}
func terminalLineColor(line string) string {
	return terminalview.LineColor(line)
}
