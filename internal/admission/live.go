package admission

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// probeLock is a read-only, non-creating, non-waiting probe of a flock file: it
// reports whether another holder has it.
func probeLock(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	return false
}

// readLive returns the run's admission status only while a controller holds the
// run's controller.lock. The status file is advisory and a dead controller leaves
// it behind, so without a live controller there is no throttle state to show: a
// finished or crashed run must never appear throttled.
func readLive(root string) (Status, bool) {
	if !probeLock(filepath.Join(root, "controller.lock")) {
		return Status{}, false
	}
	raw, err := os.ReadFile(filepath.Join(root, StatusFile))
	if err != nil {
		return Status{}, false
	}
	var st Status
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err = dec.Decode(&st); err != nil || st.Schema != "irrigator.admission-status/v1" {
		return Status{}, false
	}
	return st, true
}

// line is a compact badge for dashboards: what is limiting admission and which
// sensor bound it, plus any probe that could not be read. It is empty when
// nothing is limited and every probe is healthy, so an unrestricted run shows
// nothing extra.
func (s Status) line() string {
	var parts []string
	if s.Envelope.restricted() {
		parts = append(parts, "admission: "+s.Envelope.summary(s.Workers))
	}
	if len(s.Envelope.Degraded) > 0 {
		parts = append(parts, "degraded probes: "+strings.Join(s.Envelope.Degraded, ", "))
	}
	return strings.Join(parts, " | ")
}

// LiveLine returns the badge for a run, or "" when there is none to show.
func LiveLine(root string) string {
	if st, ok := readLive(root); ok {
		return st.line()
	}
	return ""
}
