package dashboard

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/model"
)

func TestTerminalFrameBoundsAndPriority(t *testing.T) {
	s := &model.State{Control: "run", Tasks: map[string]*model.TaskState{}}
	for _, id := range []string{"done", "work", "broken", "child", "queued", "extra"} {
		s.Plan.Tasks = append(s.Plan.Tasks, model.Task{ID: id})
		s.Tasks[id] = &model.TaskState{Status: "verified"}
	}
	s.Tasks["work"].Status = "running"
	s.Tasks["broken"].Status = "failed"
	s.Tasks["broken"].Error = "bad\x1b[2J\nresult"
	s.Plan.Tasks[3].Parent = "work"
	frame := terminalFrame(s, "/tmp/demo", time.Second, 40, 8)
	if len(frame) > 8 {
		t.Fatal(frame)
	}
	for _, line := range frame {
		if len(line) > 40 || strings.Contains(line, "\x1b") || strings.Contains(line, "\n") {
			t.Fatalf("unsafe line %q", line)
		}
	}
	if !strings.Contains(strings.Join(frame, "\n"), "broken") {
		t.Fatal("failed node hidden", frame)
	}
}

func TestTerminalRendererRestoresCursorAndDiagnostics(t *testing.T) {
	var b bytes.Buffer
	now := time.Now()
	u := &TerminalUI{out: &b, root: "demo", started: now, errors: map[string]string{}}
	s := &model.State{Control: "run", Plan: model.Plan{Tasks: []model.Task{{ID: "child", Parent: "parent"}}}, Tasks: map[string]*model.TaskState{"child": {Status: "running", Error: "diagnostic"}}}
	u.Render(s, now)
	first := b.Len()
	u.Render(s, now.Add(time.Millisecond))
	if b.Len() != first {
		t.Fatal("render not throttled")
	}
	s.Control = "blocked"
	u.Render(s, now.Add(time.Second))
	u.Close()
	out := b.String()
	if !strings.Contains(out, "child of parent") || !strings.Contains(out, "\x1b[?25h") || !strings.Contains(out, "child: diagnostic\n") {
		t.Fatal(out)
	}
}

func TestTerminalPipeUsesExistingLogs(t *testing.T) {
	f, e := os.CreateTemp(t.TempDir(), "log")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if NewTerminalUI(f, "run") != nil {
		t.Fatal("regular files must not animate")
	}
}

func TestTerminalSpinnerAndCost(t *testing.T) {
	s := &model.State{Control: "run", Tasks: map[string]*model.TaskState{"x": {Status: "running", Cost: .25}}, Plan: model.Plan{Tasks: []model.Task{{ID: "x"}}}}
	a := strings.Join(terminalFrame(s, "demo", 0, 80, 20), "\n")
	b := strings.Join(terminalFrame(s, "demo", 120*time.Millisecond, 80, 20), "\n")
	if a == b || !strings.Contains(a, "USD 0.2500") || !strings.Contains(a, "1 active") {
		t.Fatal(a, b)
	}
}

func TestTerminalTinyAndObserver(t *testing.T) {
	s := &model.State{Control: "run", Tasks: map[string]*model.TaskState{}}
	for _, size := range []int{1, 2, 3, 8} {
		f := terminalFrame(s, "demo", 0, size, size)
		if len(f) > size {
			t.Fatal(f)
		}
		for _, line := range f {
			if len(line) > size {
				t.Fatal(line)
			}
		}
	}
	var b bytes.Buffer
	now := time.Now()
	u := &TerminalUI{out: &b, started: now, WatchOnly: true, errors: map[string]string{}}
	u.Render(s, now)
	if !strings.Contains(b.String(), "running work continues") {
		t.Fatal(b.String())
	}
	s.Tasks["x"] = &model.TaskState{Error: "transient"}
	u.Render(s, now.Add(time.Millisecond))
	u.Close()
	if !strings.Contains(b.String(), "x: transient") {
		t.Fatal("throttled error lost")
	}
}
