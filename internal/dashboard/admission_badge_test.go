package dashboard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grainulation/irrigator/internal/admission"
	"github.com/grainulation/irrigator/internal/jsonio"
	"github.com/grainulation/irrigator/internal/model"
)

func writeAdmissionStatus(t *testing.T, root, reason string) {
	t.Helper()
	r := admission.OpenReading("power")
	r.MaxLocalSlots, r.Reason = 2, reason
	st := admission.Status{Schema: "irrigator.admission-status/v1", Workers: 6, Envelope: admission.Compose([]admission.Reading{r})}
	if err := jsonio.AtomicJSON(filepath.Join(root, admission.StatusFile), st); err != nil {
		t.Fatal(err)
	}
}

func holdControllerLock(t *testing.T, root string) func() {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, "controller.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
}

func TestDisplayShowsTheAdmissionBadgeOnlyWhileTheControllerIsAlive(t *testing.T) {
	root := t.TempDir()
	writeAdmissionStatus(t, root, "thermal serious")
	s := &model.State{Backend: "mock", Control: "run", Tasks: map[string]*model.TaskState{}}

	var dead bytes.Buffer
	displayTo(&dead, s, root, 10)
	if strings.Contains(dead.String(), "admission") {
		t.Fatalf("a dead run showed stale throttle state:\n%s", dead.String())
	}

	release := holdControllerLock(t, root)
	defer release()
	var live bytes.Buffer
	displayTo(&live, s, root, 10)
	if !strings.Contains(live.String(), "admission: local 2/6 (power: thermal serious)") {
		t.Fatalf("a live run's badge is missing:\n%s", live.String())
	}
}

func TestDisplayNeverPrintsTerminalEscapesFromAnAdmissionReason(t *testing.T) {
	root := t.TempDir()
	writeAdmissionStatus(t, root, "bad\x1b[31m red \x1b]0;pwned\x07 text")
	release := holdControllerLock(t, root)
	defer release()
	var out bytes.Buffer
	displayTo(&out, &model.State{Backend: "mock", Control: "run", Tasks: map[string]*model.TaskState{}}, root, 10)
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("a raw control sequence reached the terminal: %q", out.String())
	}
	if !strings.Contains(out.String(), "admission:") {
		t.Fatal("the badge itself should still be shown")
	}
}

func TestTerminalUIBadgeIsReReadAtMostOncePerSecond(t *testing.T) {
	root := t.TempDir()
	writeAdmissionStatus(t, root, "first")
	release := holdControllerLock(t, root)
	defer release()
	u := &TerminalUI{root: root}
	now := time.Now()
	if got := u.admissionBadge(now); !strings.Contains(got, "first") {
		t.Fatalf("badge = %q", got)
	}
	writeAdmissionStatus(t, root, "second")
	if got := u.admissionBadge(now.Add(300 * time.Millisecond)); !strings.Contains(got, "first") {
		t.Fatalf("the badge was re-read inside a second: %q", got)
	}
	if got := u.admissionBadge(now.Add(1100 * time.Millisecond)); !strings.Contains(got, "second") {
		t.Fatalf("the badge was not refreshed after a second: %q", got)
	}
}
