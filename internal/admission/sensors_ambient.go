package admission

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/grainulation/irrigator/internal/clock"
)

// memoryInfo is what a memory probe learned. Unknown values are -1.
type memoryInfo struct {
	// pressureLevel is the kernel's memory pressure: 1 normal, 2 warning, 4 critical.
	pressureLevel int
	swapUsedPct   int
}

// memoryProbe reads memory pressure and swap use.
type memoryProbe func(ctx context.Context) (memoryInfo, error)

// memoryConfig sets what the memory sensor does. It reads a saturation signal,
// the kernel's pressure level and swap use, and not CPU percentage: a busy CPU is
// healthy, while a machine that is paging is not.
type memoryConfig struct {
	WarnLocalSlots       int  `json:"warn_local_slots"`
	CriticalHold         bool `json:"critical_hold"`
	SwapUsedPercentLimit int  `json:"swap_used_percent_limit"` // 0 turns the swap rule off
	SwapLocalSlots       int  `json:"swap_local_slots"`
}

// memory limits local admission under memory pressure. Local models are what use
// the memory, so remote work is not limited unless pressure is critical.
type memory struct {
	cfg   memoryConfig
	probe memoryProbe
}

// newMemory returns the sensor over probe.
func newMemory(cfg memoryConfig, probe memoryProbe) *memory { return &memory{cfg: cfg, probe: probe} }

func (m *memory) sensorName() string { return "memory" }

func (m *memory) sample(ctx context.Context, _ runSnapshot) Reading {
	r := OpenReading(m.sensorName())
	info, err := m.probe(ctx)
	if err != nil {
		r.probe, r.probeErr = probeFailed, err.Error()
		return r
	}
	capLocal := func(slots int, why string, sev severity) {
		if s := limit(slots); s < r.MaxLocalSlots {
			r.MaxLocalSlots, r.Reason, r.severity = s, why, sev
		}
	}
	switch {
	case info.pressureLevel >= 4 && m.cfg.CriticalHold:
		r.hold, r.severity, r.Reason = HoldTransient, hard, "memory pressure is critical"
		return r
	case info.pressureLevel >= 4:
		capLocal(0, "memory pressure is critical", hard)
	case info.pressureLevel >= 2:
		capLocal(m.cfg.WarnLocalSlots, "memory pressure is high", soft)
	}
	if m.cfg.SwapUsedPercentLimit > 0 && info.swapUsedPct >= m.cfg.SwapUsedPercentLimit {
		capLocal(m.cfg.SwapLocalSlots, fmt.Sprintf("swap is %d%% used", info.swapUsedPct), soft)
	}
	return r
}

var swapUsage = regexp.MustCompile(`total = ([\d.]+)M\s+used = ([\d.]+)M`)

// parseSwapUsage reads `sysctl -n vm.swapusage` as a percentage, -1 when unknown
// and 0 when no swap is configured.
func parseSwapUsage(out string) int {
	m := swapUsage.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	total, err1 := strconv.ParseFloat(m[1], 64)
	used, err2 := strconv.ParseFloat(m[2], 64)
	if err1 != nil || err2 != nil {
		return -1
	}
	if total <= 0 {
		return 0
	}
	return int(used / total * 100)
}

// parsePressureLevel reads `sysctl -n kern.memorystatus_vm_pressure_level`.
func parsePressureLevel(out string) int {
	v, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1
	}
	return v
}

// diskInfo is the free space of the run's filesystem.
type diskInfo struct{ freeBytes uint64 }

// diskProbe reads free space.
type diskProbe func(ctx context.Context) (diskInfo, error)

// diskConfig sets what the disk sensors do.
type diskConfig struct {
	LowFreeMB          int `json:"low_free_mb"`
	CriticalFreeMB     int `json:"critical_free_mb"`
	LowLocalSlots      int `json:"low_local_slots"`
	LockWaitMS         int `json:"lock_wait_ms"` // 0 turns the lock-wait rule off
	LockWaitLocalSlots int `json:"lock_wait_local_slots"`
}

// disk holds all new admission when the run's filesystem is nearly full, because
// checkpoint commits and attempt logs would start failing, and limits local work
// when it is getting low. It fails closed: if free space cannot be read, nothing
// new is started.
type disk struct {
	cfg   diskConfig
	probe diskProbe
}

// newDisk returns the sensor over probe.
func newDisk(cfg diskConfig, probe diskProbe) *disk { return &disk{cfg: cfg, probe: probe} }

func (d *disk) sensorName() string { return "disk" }

func (d *disk) sample(ctx context.Context, _ runSnapshot) Reading {
	r := OpenReading(d.sensorName())
	info, err := d.probe(ctx)
	if err != nil {
		r.probe, r.probeErr = probeFailed, err.Error()
		return r
	}
	freeMB := int(info.freeBytes >> 20)
	switch {
	case freeMB < d.cfg.CriticalFreeMB:
		r.hold, r.severity = HoldTransient, hard
		r.Reason = fmt.Sprintf("%d MB free is below the %d MB critical floor", freeMB, d.cfg.CriticalFreeMB)
	case freeMB < d.cfg.LowFreeMB:
		r.MaxLocalSlots, r.severity = limit(d.cfg.LowLocalSlots), soft
		r.Reason = fmt.Sprintf("%d MB free is below the %d MB low mark", freeMB, d.cfg.LowFreeMB)
	}
	return r
}

// diskStatfs reads the free space available to unprivileged writers on the
// filesystem holding path.
func diskStatfs(path string) diskProbe {
	return func(context.Context) (diskInfo, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err != nil {
			return diskInfo{}, err
		}
		return diskInfo{freeBytes: uint64(st.Bavail) * uint64(st.Bsize)}, nil //nolint:unconvert // field widths differ by platform
	}
}

// lockWaitFn measures how long the checkpoint lock takes to become available
// to a reader, which is how long a committing writer is holding it.
type lockWaitFn func(ctx context.Context) (time.Duration, error)

// lockWait limits admission when the checkpoint lock is persistently contended:
// adding workers to a lock that is already the bottleneck only lengthens the
// queue. It is off unless the config sets lock_wait_ms.
type lockWait struct {
	cfg   diskConfig
	probe lockWaitFn
}

// newLockWait returns the sensor over probe.
func newLockWait(cfg diskConfig, probe lockWaitFn) *lockWait {
	return &lockWait{cfg: cfg, probe: probe}
}

func (l *lockWait) sensorName() string { return "lock-wait" }

func (l *lockWait) sample(ctx context.Context, _ runSnapshot) Reading {
	r := OpenReading(l.sensorName())
	wait, err := l.probe(ctx)
	if err != nil {
		r.probe, r.probeErr = probeFailed, err.Error()
		return r
	}
	if int(wait/time.Millisecond) >= l.cfg.LockWaitMS {
		s := limit(l.cfg.LockWaitLocalSlots)
		r.MaxLocalSlots, r.maxRemoteSlots, r.severity = s, s, soft
		r.Reason = fmt.Sprintf("the checkpoint lock took %s to acquire", wait.Round(time.Millisecond))
	}
	return r
}

// presenceInfo is how long the user has been idle. Unknown is -1.
type presenceInfo struct{ idleSeconds float64 }

// presenceProbe reads the idle time.
type presenceProbe func(ctx context.Context) (presenceInfo, error)

// presenceConfig sets what the presence sensor does.
type presenceConfig struct {
	ActiveWithinSeconds float64 `json:"active_within_seconds"`
	ActiveLocalSlots    int     `json:"active_local_slots"`
}

// presence yields softly while a person is using the machine, by limiting local
// work (the work that competes for the machine) for as long as there was input
// within the active window. It fails open.
type presence struct {
	cfg   presenceConfig
	probe presenceProbe
}

// newPresence returns the sensor over probe.
func newPresence(cfg presenceConfig, probe presenceProbe) *presence {
	return &presence{cfg: cfg, probe: probe}
}

func (p *presence) sensorName() string { return "presence" }

func (p *presence) sample(ctx context.Context, _ runSnapshot) Reading {
	r := OpenReading(p.sensorName())
	info, err := p.probe(ctx)
	if err != nil {
		r.probe, r.probeErr = probeFailed, err.Error()
		return r
	}
	if info.idleSeconds >= 0 && info.idleSeconds < p.cfg.ActiveWithinSeconds {
		r.MaxLocalSlots, r.severity = limit(p.cfg.ActiveLocalSlots), soft
		r.Reason = fmt.Sprintf("the machine is in use (idle %.0fs)", info.idleSeconds)
	}
	return r
}

var hidIdle = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

// parseHIDIdle reads `ioreg -c IOHIDSystem` as seconds idle, -1 when absent.
func parseHIDIdle(out string) float64 {
	m := hidIdle.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	ns, _ := strconv.ParseFloat(m[1], 64)
	return ns / 1e9
}

// offPeakConfig sets the off-peak window. Outside it, admission is limited: the
// run still makes progress, slowly, and speeds up inside the window.
type offPeakConfig struct {
	// Window is "HH:MM-HH:MM" in local time; it may wrap midnight ("22:00-07:00").
	Window             string `json:"window"`
	OutsideLocalSlots  int    `json:"outside_local_slots"`
	OutsideRemoteSlots int    `json:"outside_remote_slots"`
	// SignalFile, when set, is a path to an override-format file written by an
	// external hook (carbon intensity, energy price). It is read like the operator
	// override; a missing file means no opinion.
	SignalFile string `json:"signal_file,omitempty"`
}

var windowRe = regexp.MustCompile(`^(\d{2}):(\d{2})-(\d{2}):(\d{2})$`)

// parseWindow reads "HH:MM-HH:MM" into minutes since midnight.
func parseWindow(w string) (start, end int, err error) {
	m := windowRe.FindStringSubmatch(w)
	if m == nil {
		return 0, 0, fmt.Errorf("window %q must look like 22:00-07:00", w)
	}
	at := func(h, mm string) (int, error) {
		hv, _ := strconv.Atoi(h)
		mv, _ := strconv.Atoi(mm)
		if hv > 23 || mv > 59 {
			return 0, fmt.Errorf("window %q has an invalid time", w)
		}
		return hv*60 + mv, nil
	}
	if start, err = at(m[1], m[2]); err != nil {
		return 0, 0, err
	}
	if end, err = at(m[3], m[4]); err != nil {
		return 0, 0, err
	}
	if start == end {
		return 0, 0, fmt.Errorf("window %q is empty", w)
	}
	return start, end, nil
}

// inWindow reports whether t falls inside the window, handling a wrap past midnight.
func inWindow(t time.Time, start, end int) bool {
	m := t.Hour()*60 + t.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// offPeak limits admission outside its time window. Time comes from the injected
// clock, so tests are deterministic.
type offPeak struct {
	cfg        offPeakConfig
	start, end int
	clk        clock.Clock
}

// newOffPeak returns the sensor; the window must already be valid.
func newOffPeak(cfg offPeakConfig, clk clock.Clock) (*offPeak, error) {
	start, end, err := parseWindow(cfg.Window)
	if err != nil {
		return nil, err
	}
	return &offPeak{cfg: cfg, start: start, end: end, clk: clk}, nil
}

func (o *offPeak) sensorName() string { return "offpeak" }

func (o *offPeak) sample(context.Context, runSnapshot) Reading {
	r := OpenReading(o.sensorName())
	if inWindow(o.clk.Now(), o.start, o.end) {
		return r
	}
	r.MaxLocalSlots, r.maxRemoteSlots, r.severity = limit(o.cfg.OutsideLocalSlots), limit(o.cfg.OutsideRemoteSlots), soft
	r.Reason = "outside the off-peak window " + o.cfg.Window
	return r
}
