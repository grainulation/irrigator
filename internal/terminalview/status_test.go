package terminalview

import (
	"encoding/base64"
	"strings"
	"testing"
)

func snap(control string, statuses ...string) Snapshot {
	s := Snapshot{Control: control, Rows: map[string]Row{}}
	for i, st := range statuses {
		id := string(rune('a' + i))
		s.Tasks = append(s.Tasks, Task{ID: id})
		s.Rows[id] = Row{Status: st}
	}
	return s
}

func TestStatusMapsControl(t *testing.T) {
	for _, tc := range []struct {
		control, want string
	}{
		{"run", "working"}, {"complete", "done"}, {"blocked", "blocked"},
		{"pause", "idle"}, {"paused", "idle"}, {"down", "idle"},
	} {
		got, ok := Status(snap(tc.control, "verified"), "/runs/x")
		if !ok || got.State != tc.want {
			t.Errorf("control %q: got %+v ok=%v, want state %q", tc.control, got, ok, tc.want)
		}
	}
	if _, ok := Status(snap(""), "/runs/x"); ok {
		t.Error("an unstarted run should not report")
	}
}

func TestStatusSummary(t *testing.T) {
	s := snap("run", "verified", "verified", "running", "failed", "pending")
	s.Cost = "$1.42 / $5"
	got, _ := Status(s, "/runs/feat-auth")
	if got.Progress != 40 {
		t.Errorf("progress = %d, want 40", got.Progress)
	}
	if want := "2/5 verified · 1 running · 1 need attention · $1.42 / $5"; got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
	if got.Title != "irrigator feat-auth" {
		t.Errorf("title = %q", got.Title)
	}
}

func TestOSCFormat(t *testing.T) {
	p := ProgramStatus{State: "working", Progress: 140, Title: "irrigator r1", Message: "1/2 verified: a:b"}
	got := p.OSC()
	if !strings.HasPrefix(got, "\x1b]7501;") || !strings.HasSuffix(got, "\x1b\\") {
		t.Fatalf("bad framing: %q", got)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b]7501;"), "\x1b\\")
	fields := map[string]string{}
	for _, f := range strings.Split(body, ":") {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			t.Fatalf("field without '=': %q (free text must be base64)", f)
		}
		fields[k] = v
	}
	if fields["state"] != "working" || fields["app"] != "irrigator" || fields["progress"] != "100" {
		t.Errorf("fields = %v", fields)
	}
	msg, err := base64.StdEncoding.DecodeString(fields["msg"])
	if err != nil || string(msg) != p.Message {
		t.Errorf("msg = %q (%v)", msg, err)
	}
	if strings.ContainsAny(body, "\x07\x1b") {
		t.Error("report body must not contain terminators")
	}
	if strings.Contains((ProgramStatus{State: "done"}).OSC(), "progress=") {
		t.Error("progress is only sent while working")
	}
}
