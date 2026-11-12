package dashboard

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func terminalUIState() *model.State {
	s := &model.State{Control: "run", Tasks: map[string]*model.TaskState{}}
	for _, status := range []string{"running", "produced", "uncertain", "failed", "verified", "waiting", "pending", "cancelled"} {
		id := "node-" + status
		s.Plan.Tasks = append(s.Plan.Tasks, model.Task{ID: id})
		s.Tasks[id] = &model.TaskState{Status: status}
	}
	return s
}

func TestTerminalUIUntrustedTextAndBounds(t *testing.T) {
	s := terminalUIState()
	s.Tasks["node-failed"].Error = "bad\x1b]52;c;clipboard\a\r\n\x9b2J\u202ehidden"
	s.Plan.Tasks[0].Parent = "parent\x1b[2J\n"
	for _, size := range [][2]int{{1, 1}, {2, 3}, {7, 4}, {40, 8}, {120, 30}} {
		frame := terminalFrame(s, "/tmp/run\x1b[2J\n", time.Second, size[0], size[1])
		if len(frame) > size[1] {
			t.Fatalf("height exceeded: %d", len(frame))
		}
		for _, line := range frame {
			if len(line) > size[0] {
				t.Fatalf("width exceeded: %q", line)
			}
			for _, r := range line {
				if r < 32 || r > 126 {
					t.Fatalf("unsafe terminal rune %U in %q", r, line)
				}
			}
		}
	}
}

func TestTerminalUIRenderDoesNotMutateState(t *testing.T) {
	s := terminalUIState()
	before := append([]model.Task(nil), s.Plan.Tasks...)
	statuses := map[string]string{}
	for id, row := range s.Tasks {
		statuses[id] = row.Status
	}
	for _, elapsed := range []time.Duration{-time.Second, 0, time.Second, 24 * time.Hour} {
		terminalFrame(s, "demo", elapsed, 100, 30)
	}
	if !reflect.DeepEqual(before, s.Plan.Tasks) {
		t.Fatal("presentation reordered durable plan")
	}
	for id, status := range statuses {
		if s.Tasks[id].Status != status {
			t.Fatal("presentation changed task state")
		}
	}
}

func TestTerminalUIRealPipeStaysPlain(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for _, mode := range []string{"", "plain", "static"} {
		t.Setenv("IRRIGATOR_UI", mode)
		if NewTerminalUI(w, "demo") != nil {
			t.Fatalf("pipe enabled dashboard for mode %q", mode)
		}
	}
}

func TestTerminalUIDiagnosticsSurviveThrottlingSafely(t *testing.T) {
	var b bytes.Buffer
	now := time.Unix(100, 0)
	u := &TerminalUI{out: &b, root: "demo", started: now, errors: map[string]string{}}
	s := terminalUIState()
	u.Render(s, now)
	s.Tasks["node-running"].Error = "transient\x1b]52;c;secret\a\nnext"
	u.Render(s, now.Add(time.Millisecond))
	s.Tasks["node-running"].Error = ""
	b.Reset()
	u.Close()
	got := b.String()
	if !strings.Contains(got, "node-running: transient") {
		t.Fatal("lost throttled diagnostic")
	}
	if strings.Contains(got, "\x1b]52") || strings.Contains(got, "\a") {
		t.Fatal("diagnostic injected terminal control")
	}
	if !strings.Contains(got, "\x1b[?25h") {
		t.Fatal("cursor not restored")
	}
}

func TestTerminalUIStaticAndCompletedWorkDoNotAnimate(t *testing.T) {
	s := terminalUIState()
	t.Setenv("IRRIGATOR_UI", "static")
	a := terminalFrame(s, "demo", 0, 120, 30)
	b := terminalFrame(s, "demo", 120*time.Millisecond, 120, 30)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("static mode animates within unchanged elapsed second")
	}
	t.Setenv("IRRIGATOR_UI", "")
	for _, status := range []string{"produced", "verified", "uncertain", "failed", "waiting", "pending", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			state := &model.State{Control: "run", Plan: model.Plan{Tasks: []model.Task{{ID: "single"}}}, Tasks: map[string]*model.TaskState{"single": {Status: status}}}
			a := terminalFrame(state, "demo", 0, 120, 30)
			b := terminalFrame(state, "demo", 120*time.Millisecond, 120, 30)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("%s animates despite no running task", status)
			}
		})
	}
}

func TestTerminalUIStableMapAndHonestStateCounts(t *testing.T) {
	t.Setenv("IRRIGATOR_UI", "static")
	s := terminalUIState()
	frame := terminalFrame(s, "demo", 0, 120, 30)
	text := strings.Join(frame, "\n")
	for _, count := range []string{"1 active", "1 produced", "1 failed", "1 uncertain", "1/8 verified"} {
		if !strings.Contains(text, count) {
			t.Fatalf("missing truthful count %q: %s", count, text)
		}
	}
	mapLine := func(lines []string) string {
		for _, line := range lines {
			if strings.HasPrefix(line, "map  ") {
				return strings.TrimPrefix(line, "map  ")
			}
		}
		t.Fatal("missing task map")
		return ""
	}
	before := mapLine(frame)
	s.Tasks["node-running"].Status = "verified"
	after := mapLine(terminalFrame(s, "demo", 0, 120, 30))
	if len(before) != len(after) || before[1:] != after[1:] || after[0] != '+' {
		t.Fatalf("status transition moved unrelated slots: %q -> %q", before, after)
	}
	if before[0] == before[1] || before[2] == before[3] {
		t.Fatalf("running/produced or uncertain/failed conflated: %q", before)
	}
}

func TestTerminalUITruncatedMapNeverClaimsComplete(t *testing.T) {
	s := &model.State{Control: "run", Tasks: map[string]*model.TaskState{}}
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("node-%03d", i)
		s.Plan.Tasks = append(s.Plan.Tasks, model.Task{ID: id})
		s.Tasks[id] = &model.TaskState{Status: "pending"}
	}
	frame := strings.Join(terminalFrame(s, "demo", 0, 100, 24), "\n")
	if !strings.Contains(frame, "285/500 nodes shown") {
		t.Fatal("truncated map omitted its visible coverage count")
	}
	if strings.Contains(frame, "map shows all") {
		t.Fatal("bounded map falsely claims to show all 500 tasks")
	}
}
