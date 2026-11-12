package executor

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/checkpoint"
	"github.com/grainulation/irrigator/internal/model"
	"github.com/grainulation/irrigator/internal/runfixture"
	"github.com/grainulation/irrigator/internal/terminalview"
)

func runProgressWorker(t *testing.T, progress string) string {
	t.Helper()
	var buf bytes.Buffer
	old := workerProgressOut
	workerProgressOut = &buf
	t.Cleanup(func() { workerProgressOut = old })
	t.Setenv(workerProgressEnv, progress)
	root := runfixture.New(t, runfixture.Task("a", 0))
	if err := checkpoint.SetControl(root, "run"); err != nil {
		t.Fatal(err)
	}
	e := &serializedAttemptExecutor{}
	err := workerOperationWithExecutor(root, "a", model.Options{Timeout: time.Second, Budget: .25}, "", func(localAttemptExecutor) attemptExecutor { return e })
	if err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestWorkerProgressLines(t *testing.T) {
	out := runProgressWorker(t, "1")
	// OSC 133 marks are checked in TestWorkerProgressSemanticMarks.
	out = strings.NewReplacer("\x1b]133;A\x1b\\", "", "\x1b]133;B\x1b\\", "", "\x1b]133;C\x1b\\", "", "\x1b]133;D;0\x1b\\", "").Replace(out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	for i, want := range []string{"a attempt 1 admitted", "a attempt 1 provider started", "a attempt 1 settled, cost"} {
		// Each line is preceded by an OSC 2 title carrying the same text.
		title, line, ok := strings.Cut(lines[i], "\x1b\\")
		if !ok || title != "\x1b]2;"+line || !strings.HasPrefix(line, "irrigator: ") || !strings.Contains(line, want) {
			t.Errorf("line %d = %q, want title and %q", i, lines[i], want)
		}
	}
}

func TestWorkerProgressSemanticMarks(t *testing.T) {
	out := runProgressWorker(t, "1")
	order := []string{
		"\x1b]133;A\x1b\\\x1b]133;B\x1b\\",
		"attempt 1 admitted",
		"\x1b]133;C\x1b\\",
		"attempt 1 provider started",
		"attempt 1 settled",
		"\x1b]133;D;0\x1b\\",
	}
	at := 0
	for _, want := range order {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order after offset %d in %q", want, at, out)
		}
		at += i + len(want)
	}
	if n := strings.Count(out, "\x1b]133;"); n != 4 {
		t.Errorf("%d OSC 133 marks, want 4 (A B C D) in %q", n, out)
	}
}

func TestWorkerProgressMarkExitCodes(t *testing.T) {
	var buf bytes.Buffer
	old := workerProgressOut
	workerProgressOut = &buf
	t.Cleanup(func() { workerProgressOut = old })
	t.Setenv(workerProgressEnv, "1")
	workerMark(terminalview.MarkCommandEnd(1))
	if buf.String() != "\x1b]133;D;1\x1b\\" {
		t.Errorf("D mark = %q", buf.String())
	}
	buf.Reset()
	t.Setenv(workerProgressEnv, "0")
	workerMark(terminalview.MarkOutputStart)
	if buf.Len() != 0 {
		t.Errorf("mark written with progress off: %q", buf.String())
	}
}

func TestWorkerProgressOffByDefault(t *testing.T) {
	for _, v := range []string{"", "0"} {
		if out := runProgressWorker(t, v); out != "" {
			t.Errorf("IRRIGATOR_WORKER_PROGRESS=%q printed %q", v, out)
		}
	}
}
