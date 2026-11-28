package admission

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeStatus(t *testing.T, root string, env Envelope) {
	t.Helper()
	w := statusWriter{path: filepath.Join(root, StatusFile), debounce: time.Second, preset: "balanced"}
	w.observe(time.Now(), env, runSnapshot{}, 6)
	if w.Err != nil {
		t.Fatal(w.Err)
	}
}

func holdController(t *testing.T, root string) func() {
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

func throttled() Envelope {
	r := OpenReading("power")
	r.MaxLocalSlots, r.Reason = 2, "thermal serious"
	d := OpenReading("network")
	d.probe, d.probeErr = probeFailed, "route failed"
	return Compose([]Reading{r, d})
}

// The status file is advisory and a dead controller leaves it behind. A dead
// run must never look throttled.
func TestAStaleStatusFileOfADeadRunIsNeverShown(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, root, throttled())
	if _, ok := readLive(root); ok {
		t.Fatal("a status file with no live controller was reported")
	}
	if LiveLine(root) != "" {
		t.Fatal("a dead run produced a badge")
	}
	// An empty controller.lock file with nobody holding it is also a dead run.
	if err := os.WriteFile(filepath.Join(root, "controller.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if LiveLine(root) != "" {
		t.Fatal("an unheld controller.lock counted as a live controller")
	}
}

func TestALiveControllersStatusIsShownWithItsBindingAndDegradedProbes(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, root, throttled())
	release := holdController(t, root)
	st, ok := readLive(root)
	if !ok {
		t.Fatal("a live controller's status was hidden")
	}
	line := st.line()
	for _, part := range []string{"admission:", "local 2/6", "power: thermal serious", "degraded probes: network"} {
		if !strings.Contains(line, part) {
			t.Errorf("badge %q lacks %q", line, part)
		}
	}
	release()
	if LiveLine(root) != "" {
		t.Fatal("the badge outlived the controller")
	}
}

func TestAnUnrestrictedHealthyRunShowsNothing(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, root, open())
	release := holdController(t, root)
	defer release()
	if line := LiveLine(root); line != "" {
		t.Fatalf("an open envelope with healthy probes must show nothing, got %q", line)
	}
}

func TestADegradedProbeAloneIsShownEvenWhenNothingIsLimited(t *testing.T) {
	root := t.TempDir()
	d := OpenReading("power")
	d.probe, d.probeErr = probeFailed, "pmset missing"
	writeStatus(t, root, Compose([]Reading{d}))
	release := holdController(t, root)
	defer release()
	if line := LiveLine(root); line != "degraded probes: power" {
		t.Fatalf("badge = %q", line)
	}
}

func TestACorruptOrForeignStatusFileIsIgnored(t *testing.T) {
	root := t.TempDir()
	release := holdController(t, root)
	defer release()
	for name, body := range map[string]string{"corrupt": "{not json", "foreign": `{"schema":"something/else","summary":"x"}`} {
		if err := os.WriteFile(filepath.Join(root, StatusFile), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, ok := readLive(root); ok {
			t.Errorf("%s status file was accepted", name)
		}
	}
}

func TestEvaluatorStatusRoundTripsThroughReadLive(t *testing.T) {
	root := t.TempDir()
	clk := newFakeClock()
	s := newFake("power")
	r := OpenReading("power")
	r.hold, r.Reason = HoldTransient, "battery 9% is below the 15% floor"
	s.set(r)
	e := newEvaluator(options{clock: clk, workers: 6, exitDwell: time.Second, rampThrottle: time.Second, sync: true, statusPath: filepath.Join(root, StatusFile), preset: "balanced"},
		registration{sensor: s, policy: failOpen, ttl: time.Hour})
	e.evaluate(context.Background(), runSnapshot{})
	release := holdController(t, root)
	defer release()
	st, ok := readLive(root)
	if !ok || st.Workers != 6 || st.Envelope.Hold != HoldTransient || !strings.Contains(st.line(), "battery 9%") {
		t.Fatalf("round trip: %+v %v", st, ok)
	}
}
