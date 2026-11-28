package admission

import (
	"fmt"
	"os"
	"time"

	"github.com/grainulation/irrigator/internal/jsonio"
)

// StatusFile is the name of the live status sidecar in the run directory.
const StatusFile = "admission-status.json"

// Status is the ephemeral live picture written next to the run. It is advisory:
// readers must treat it as stale unless the controller still holds its lock.
type Status struct {
	Schema        string    `json:"schema"`
	Preset        string    `json:"preset"`
	UpdatedAt     time.Time `json:"updated_at"`
	ControllerPID int       `json:"controller_pid"`
	Workers       int       `json:"workers"`
	Summary       string    `json:"summary"`
	Envelope      Envelope  `json:"envelope"`
	LocalBusy     int       `json:"local_busy"`
	RemoteBusy    int       `json:"remote_busy"`
}

// statusWriter writes the status file only when the picture changes, and not more
// often than the debounce, except that a hold starting or ending is written at
// once. Evaluation happens every scheduling pass; the file must not.
type statusWriter struct {
	path     string
	debounce time.Duration
	preset   string

	wroteAt  time.Time
	wroteSig string
	hold     HoldKind
	wrote    bool
	Err      error
}

func signature(e Envelope) string {
	return fmt.Sprintf("%d|%d|%t|%d|%s|%v|%v", e.MaxLocalSlots, e.MaxRemoteSlots, e.AllowRemote, e.ModelTier, e.Hold, e.Bindings, e.Degraded)
}

func (w *statusWriter) observe(now time.Time, env Envelope, snap runSnapshot, workers int) {
	if w.path == "" {
		return
	}
	sig := signature(env)
	holdChanged := w.wrote && env.Hold != w.hold
	switch {
	case !w.wrote && !w.wroteAt.IsZero() && now.Sub(w.wroteAt) < w.debounce:
		return // the first write failed; try again after the debounce
	case !w.wrote:
	case sig == w.wroteSig:
		return
	case !holdChanged && now.Sub(w.wroteAt) < w.debounce:
		return
	}
	st := Status{Schema: "irrigator.admission-status/v1", Preset: w.preset, UpdatedAt: now.UTC(), ControllerPID: os.Getpid(), Workers: workers, Summary: env.summary(workers), Envelope: env, LocalBusy: snap.localBusy, RemoteBusy: snap.remoteBusy}
	if w.Err = jsonio.AtomicJSON(w.path, st); w.Err != nil {
		// Keep what was last written (and so what differs from it) and try again
		// after the debounce, instead of treating the new envelope as on disk.
		w.wroteAt = now
		return
	}
	w.wrote, w.wroteAt, w.wroteSig, w.hold = true, now, sig, env.Hold
}
