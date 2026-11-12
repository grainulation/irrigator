package terminalview

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Task is one planned node, in stable plan order.
type Task struct {
	ID        string
	Parent    string
	DependsOn []string
}

// Row is the observed state of one task entry. Nil marks a nil row.
type Row struct {
	Status string
	Error  string
	Nil    bool
}

// Snapshot is the input to Frame. Frame never mutates it.
type Snapshot struct {
	Control       string
	GraphRevision int64
	Cost          string
	Static        bool
	// Tasks are in plan order.
	Tasks []Task
	// Rows is the full state map, including orphan rows and nil rows.
	// A task ID absent from Rows behaves like a nil row.
	Rows map[string]Row
}

func (s Snapshot) row(id string) (Row, bool) {
	r, ok := s.Rows[id]
	if !ok || r.Nil {
		return Row{}, false
	}
	return r, true
}

// Safe replaces control and non-ASCII characters.
func Safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		if r > 126 {
			return '?'
		}
		return r
	}, s)
}

// Clip sanitizes s and shortens it to width bytes.
func Clip(s string, width int) string {
	s = Safe(s)
	if len(s) <= width {
		return s
	}
	if width < 4 {
		return s[:width]
	}
	return s[:width-3] + "..."
}

// glyph returns the one-character marker for a task row. Only running tasks
// pulse (o O @ O, offset by slot); produced work is motionless.
func glyph(row Row, ok bool, slot int, phase int64, animate bool) byte {
	if !ok || row.Nil {
		return '.'
	}
	switch row.Status {
	case "running":
		if !animate {
			return 'O'
		}
		return "oO@O"[(phase+int64(slot))%4]
	case "produced":
		return 'P'
	case "verified":
		return '+'
	case "failed":
		return 'X'
	case "uncertain":
		return '?'
	case "waiting":
		return ':'
	case "cancelled":
		return '-'
	}
	if row.Error != "" {
		return 'X'
	}
	return '.'
}

// LineColor returns the ANSI color prefix for a frame line.
func LineColor(line string) string {
	t := strings.TrimLeft(line, " ")
	if len(line) > 2 && line[0] == ' ' && len(t) > 0 {
		switch t[0] {
		case 'X':
			return "\x1b[31m"
		case '?':
			return "\x1b[35m"
		case '+':
			return "\x1b[32m"
		case 'o', 'O', '@':
			return "\x1b[33m"
		case 'P':
			return "\x1b[34m"
		case '-', '.', ':':
			return "\x1b[2m"
		}
	}
	return "\x1b[36m"
}

// Frame renders the dashboard lines for a snapshot.
func Frame(s Snapshot, root string, elapsed time.Duration, width, height int) []string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	if elapsed < 0 {
		elapsed = 0
	}
	animate := s.Control == "run" && !s.Static
	phase := elapsed.Milliseconds() / 120

	// Stable plan order drives the visual map and the slot numbers.
	order := append([]Task(nil), s.Tasks...)
	slot := make(map[string]int, len(order))
	for i, t := range order {
		slot[t.ID] = i
	}
	specs := append([]Task(nil), order...)
	rank := func(id string) int {
		r, ok := s.row(id)
		if !ok {
			return 5
		}
		if r.Error != "" || r.Status == "uncertain" || r.Status == "failed" {
			return 0
		}
		switch r.Status {
		case "running":
			return 1
		case "produced":
			return 2
		case "waiting":
			return 3
		case "pending":
			return 4
		}
		return 5
	}
	sort.SliceStable(specs, func(i, j int) bool { return rank(specs[i].ID) < rank(specs[j].ID) })

	total := len(s.Rows)
	verified, active, produced, failed, uncertain := 0, 0, 0, 0, 0
	for _, r := range s.Rows {
		if r.Nil {
			continue
		}
		switch r.Status {
		case "verified":
			verified++
		case "running":
			active++
		case "produced":
			produced++
		case "failed":
			failed++
		case "uncertain":
			uncertain++
		}
	}
	barWidth := 20
	if width < 60 {
		barWidth = 10
	}
	fill := 0
	if total > 0 {
		fill = verified * barWidth / total
	}
	frame := []string{
		fmt.Sprintf("IRRIGATOR / %s / %s / revision %d", filepath.Base(root), s.Control, s.GraphRevision),
		fmt.Sprintf("[%s%s] %d/%d verified", strings.Repeat("=", fill), strings.Repeat(".", barWidth-fill), verified, total),
		fmt.Sprintf("%d active | %d produced | %d failed | %d uncertain", active, produced, failed, uncertain),
		fmt.Sprintf("elapsed %s | %s", elapsed.Truncate(time.Second), s.Cost),
	}

	// Map: one fixed slot per node in plan order; positions never move.
	maxRows := 3
	if height < 10 {
		maxRows = 1
	}
	perRow := width - 5
	if perRow < 1 {
		perRow = 1
	}
	shown := len(order)
	if shown > perRow*maxRows {
		shown = perRow * maxRows
	}
	if len(order) == 0 {
		frame = append(frame, "map  (no tasks)")
	}
	truncated := shown < len(order)
	if truncated {
		// Explicit coverage on its own line so it is never clipped away.
		frame = append(frame, fmt.Sprintf("map  %d/%d nodes shown", shown, len(order)))
	}
	for start := 0; start < shown; start += perRow {
		end := start + perRow
		if end > shown {
			end = shown
		}
		buf := make([]byte, 0, end-start)
		for i := start; i < end; i++ {
			r, ok := s.row(order[i].ID)
			buf = append(buf, glyph(r, ok, i, phase, animate))
		}
		prefix := "     "
		if start == 0 && !truncated {
			prefix = "map  "
		}
		frame = append(frame, prefix+string(buf))
	}
	if height >= 9 {
		frame = append(frame, "legend . pending : waiting o/O/@ running P produced(unverified) + verified X failed ? uncertain - cancelled")
	}

	avail := height - 1 - len(frame)
	if avail < 0 {
		avail = 0
	}
	capacity := avail
	overflow := false
	if len(specs) > avail && avail >= 2 {
		capacity = avail - 1
		overflow = true
	}
	listed := 0
	for _, spec := range specs {
		if listed >= capacity {
			break
		}
		r, ok := s.row(spec.ID)
		if !ok {
			continue
		}
		relation := ""
		if spec.Parent != "" {
			relation = " child of " + spec.Parent
		}
		if len(spec.DependsOn) > 0 {
			relation += " <- " + strings.Join(spec.DependsOn, ",")
		}
		line := fmt.Sprintf(" %c %-9s #%d %s%s", glyph(r, ok, slot[spec.ID], phase, animate), r.Status, slot[spec.ID]+1, spec.ID, relation)
		if r.Error != "" {
			line += " : " + r.Error
		}
		frame = append(frame, line)
		listed++
	}
	if overflow {
		frame = append(frame, fmt.Sprintf(" ... %d more nodes not listed; errors/running first", len(specs)-listed))
	}

	footer := "Ctrl-C pauses safely | irrigator status <run> for full detail"
	if height < 2 {
		frame = frame[:1]
	} else {
		if len(frame) > height-1 {
			frame = frame[:height-1]
		}
		frame = append(frame, footer)
	}
	for i := range frame {
		frame[i] = Clip(frame[i], width)
	}
	return frame
}
