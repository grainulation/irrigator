package terminalview

import (
	"strings"
	"testing"
)

func TestTitleOSCSanitizes(t *testing.T) {
	got := TitleOSC("irrigator run\x1b]0;evil\x07\u009c · é\xff")
	if got != "\x1b]2;irrigator run ]0;evil   · é?\x1b\\" {
		t.Fatalf("title = %q", got)
	}
}

func TestProgressOSC(t *testing.T) {
	cases := map[string]string{
		"working": "\x1b]9;4;1;40\x1b\\",
		"blocked": "\x1b]9;4;4;40\x1b\\",
		"error":   "\x1b]9;4;2;40\x1b\\",
		"done":    ProgressClear,
		"idle":    ProgressClear,
	}
	for state, want := range cases {
		if got := ProgressOSC(ProgramStatus{State: state, Progress: 40}); got != want {
			t.Errorf("%s: %q, want %q", state, got, want)
		}
	}
	if got := ProgressOSC(ProgramStatus{State: "working", Progress: 250}); got != "\x1b]9;4;1;100\x1b\\" {
		t.Errorf("clamp: %q", got)
	}
}

func TestNotifyFlavor(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, ok := NotifyFlavor("off", env(nil)); ok {
		t.Error("off should disable")
	}
	if f, _ := NotifyFlavor("", env(map[string]string{"TERM": "xterm-ghostty"})); f != NotifyOSC9 {
		t.Errorf("ghostty flavor %q", f)
	}
	if f, _ := NotifyFlavor("", env(map[string]string{"VTE_VERSION": "7600"})); f != NotifyOSC777 {
		t.Errorf("vte flavor %q", f)
	}
	if f, _ := NotifyFlavor("osc777", env(nil)); f != NotifyOSC777 {
		t.Errorf("explicit flavor %q", f)
	}
}

func TestNotifyOSC(t *testing.T) {
	if got := NotifyOSC(NotifyOSC9, "irrigator x", "run complete"); got != "\x1b]9;irrigator x: run complete\x1b\\" {
		t.Errorf("osc9 = %q", got)
	}
	got := NotifyOSC(NotifyOSC777, "a;b", "c;d\x07")
	if got != "\x1b]777;notify;a,b;c,d \x1b\\" {
		t.Errorf("osc777 = %q", got)
	}
	// A title that looks like a subcommand still cannot form 9;4.
	if got := NotifyOSC(NotifyOSC9, "4", "x"); strings.HasPrefix(got, "\x1b]9;4;") {
		t.Errorf("osc9 parsed as progress: %q", got)
	}
}

func TestNotice(t *testing.T) {
	run := func(state string) ProgramStatus {
		return ProgramStatus{State: state, Title: "irrigator r", Message: "2/2 verified"}
	}
	if _, _, ok := Notice(ProgramStatus{}, run("done")); ok {
		t.Error("first report notified")
	}
	if _, _, ok := Notice(run("working"), run("working")); ok {
		t.Error("unchanged state notified")
	}
	if _, body, ok := Notice(run("working"), run("done")); !ok || body != "run complete · 2/2 verified" {
		t.Errorf("done: %v %q", ok, body)
	}
	if _, body, ok := Notice(run("working"), run("blocked")); !ok || !strings.HasPrefix(body, "needs you") {
		t.Errorf("blocked: %v %q", ok, body)
	}
	if _, _, ok := Notice(run("blocked"), run("working")); ok {
		t.Error("unblocking notified")
	}
	cell := ProgramStatus{ID: "build", State: "blocked", Message: "outcome uncertain; needs a decision"}
	if _, body, ok := CellNotice("irrigator r", "working", cell); !ok || !strings.HasPrefix(body, "build: ") {
		t.Errorf("cell: %v %q", ok, body)
	}
	if _, _, ok := CellNotice("irrigator r", "blocked", cell); ok {
		t.Error("cell re-notified")
	}
}
