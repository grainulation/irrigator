package admission

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MemoryInfo is what a memory probe learned. Unknown values are -1.
type MemoryInfo struct {
	// PressureLevel is the kernel's memory pressure: 1 normal, 2 warning, 4 critical.
	PressureLevel int
	SwapUsedPct   int
}

// MemoryProbe reads memory pressure and swap use.
type MemoryProbe func(ctx context.Context) (MemoryInfo, error)

// MemoryConfig sets what the memory sensor does. It reads a saturation signal,
// the kernel's pressure level and swap use, and not CPU percentage: a busy CPU is
// healthy, while a machine that is paging is not.
type MemoryConfig struct {
	WarnLocalSlots       int  `json:"warn_local_slots"`
	CriticalHold         bool `json:"critical_hold"`
	SwapUsedPercentLimit int  `json:"swap_used_percent_limit"` // 0 turns the swap rule off
	SwapLocalSlots       int  `json:"swap_local_slots"`
}

// Memory limits local admission under memory pressure. Local models are what use
// the memory, so remote work is not limited unless pressure is critical.
type Memory struct {
	cfg   MemoryConfig
	probe MemoryProbe
}

// NewMemory returns the sensor over probe.
func NewMemory(cfg MemoryConfig, probe MemoryProbe) *Memory { return &Memory{cfg: cfg, probe: probe} }

func (m *Memory) Name() string { return "memory" }

func (m *Memory) Sample(ctx context.Context, _ RunSnapshot) Reading {
	r := OpenReading(m.Name())
	info, err := m.probe(ctx)
	if err != nil {
		r.Probe, r.ProbeErr = ProbeFailed, err.Error()
		return r
	}
	capLocal := func(slots int, why string, sev Severity) {
		if s := limit(slots); s < r.MaxLocalSlots {
			r.MaxLocalSlots, r.Reason, r.Severity = s, why, sev
		}
	}
	switch {
	case info.PressureLevel >= 4 && m.cfg.CriticalHold:
		r.Hold, r.Severity, r.Reason = HoldTransient, Hard, "memory pressure is critical"
		return r
	case info.PressureLevel >= 4:
		capLocal(0, "memory pressure is critical", Hard)
	case info.PressureLevel >= 2:
		capLocal(m.cfg.WarnLocalSlots, "memory pressure is high", Soft)
	}
	if m.cfg.SwapUsedPercentLimit > 0 && info.SwapUsedPct >= m.cfg.SwapUsedPercentLimit {
		capLocal(m.cfg.SwapLocalSlots, fmt.Sprintf("swap is %d%% used", info.SwapUsedPct), Soft)
	}
	return r
}

var swapUsage = regexp.MustCompile(`total = ([\d.]+)M\s+used = ([\d.]+)M`)

// ParseSwapUsage reads `sysctl -n vm.swapusage` as a percentage, -1 when unknown
// and 0 when no swap is configured.
func ParseSwapUsage(out string) int {
	m := swapUsage.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	total, _ := strconv.ParseFloat(m[1], 64)
	used, _ := strconv.ParseFloat(m[2], 64)
	if total <= 0 {
		return 0
	}
	return int(used / total * 100)
}

// ParsePressureLevel reads `sysctl -n kern.memorystatus_vm_pressure_level`.
func ParsePressureLevel(out string) int {
	v, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1
	}
	return v
}

// DiskInfo is the free space of the run's filesystem.
type DiskInfo struct{ FreeBytes uint64 }

// DiskProbe reads free space.
type DiskProbe func(ctx context.Context) (DiskInfo, error)

// DiskConfig sets what the disk sensors do.
type DiskConfig struct {
	LowFreeMB          int `json:"low_free_mb"`
	CriticalFreeMB     int `json:"critical_free_mb"`
	LowLocalSlots      int `json:"low_local_slots"`
	LockWaitMS         int `json:"lock_wait_ms"` // 0 turns the lock-wait rule off
	LockWaitLocalSlots int `json:"lock_wait_local_slots"`
}

// Disk holds all new admission when the run's filesystem is nearly full, because
// checkpoint commits and attempt logs would start failing, and limits local work
// when it is getting low. It fails closed: if free space cannot be read, nothing
// new is started.
type Disk struct {
	cfg   DiskConfig
	probe DiskProbe
}

// NewDisk returns the sensor over probe.
func NewDisk(cfg DiskConfig, probe DiskProbe) *Disk { return &Disk{cfg: cfg, probe: probe} }

func (d *Disk) Name() string { return "disk" }

func (d *Disk) Sample(ctx context.Context, _ RunSnapshot) Reading {
	r := OpenReading(d.Name())
	info, err := d.probe(ctx)
	if err != nil {
		r.Probe, r.ProbeErr = ProbeFailed, err.Error()
		return r
	}
	freeMB := int(info.FreeBytes >> 20)
	switch {
	case freeMB < d.cfg.CriticalFreeMB:
		r.Hold, r.Severity = HoldTransient, Hard
		r.Reason = fmt.Sprintf("%d MB free is below the %d MB critical floor", freeMB, d.cfg.CriticalFreeMB)
	case freeMB < d.cfg.LowFreeMB:
		r.MaxLocalSlots, r.Severity = limit(d.cfg.LowLocalSlots), Soft
		r.Reason = fmt.Sprintf("%d MB free is below the %d MB low mark", freeMB, d.cfg.LowFreeMB)
	}
	return r
}

// DiskStatfs reads the free space available to unprivileged writers on the
// filesystem holding path.
func DiskStatfs(path string) DiskProbe {
	return func(context.Context) (DiskInfo, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err != nil {
			return DiskInfo{}, err
		}
		return DiskInfo{FreeBytes: uint64(st.Bavail) * uint64(st.Bsize)}, nil //nolint:unconvert // field widths differ by platform
	}
}

// LockWaitProbe measures how long the checkpoint lock takes to become available
// to a reader, which is how long a committing writer is holding it.
type LockWaitProbe func(ctx context.Context) (time.Duration, error)

// LockWait limits admission when the checkpoint lock is persistently contended:
// adding workers to a lock that is already the bottleneck only lengthens the
// queue. It is off unless the config sets lock_wait_ms.
type LockWait struct {
	cfg   DiskConfig
	probe LockWaitProbe
}

// NewLockWait returns the sensor over probe.
func NewLockWait(cfg DiskConfig, probe LockWaitProbe) *LockWait {
	return &LockWait{cfg: cfg, probe: probe}
}

func (l *LockWait) Name() string { return "lock-wait" }

func (l *LockWait) Sample(ctx context.Context, _ RunSnapshot) Reading {
	r := OpenReading(l.Name())
	wait, err := l.probe(ctx)
	if err != nil {
		r.Probe, r.ProbeErr = ProbeFailed, err.Error()
		return r
	}
	if int(wait/time.Millisecond) >= l.cfg.LockWaitMS {
		s := limit(l.cfg.LockWaitLocalSlots)
		r.MaxLocalSlots, r.MaxRemoteSlots, r.Severity = s, s, Soft
		r.Reason = fmt.Sprintf("the checkpoint lock took %s to acquire", wait.Round(time.Millisecond))
	}
	return r
}

// PresenceInfo is how long the user has been idle. Unknown is -1.
type PresenceInfo struct{ IdleSeconds float64 }

// PresenceProbe reads the idle time.
type PresenceProbe func(ctx context.Context) (PresenceInfo, error)

// PresenceConfig sets what the presence sensor does.
type PresenceConfig struct {
	ActiveWithinSeconds float64 `json:"active_within_seconds"`
	ActiveLocalSlots    int     `json:"active_local_slots"`
}

// Presence yields softly while a person is using the machine, by limiting local
// work (the work that competes for the machine) for as long as there was input
// within the active window. It fails open.
type Presence struct {
	cfg   PresenceConfig
	probe PresenceProbe
}

// NewPresence returns the sensor over probe.
func NewPresence(cfg PresenceConfig, probe PresenceProbe) *Presence {
	return &Presence{cfg: cfg, probe: probe}
}

func (p *Presence) Name() string { return "presence" }

func (p *Presence) Sample(ctx context.Context, _ RunSnapshot) Reading {
	r := OpenReading(p.Name())
	info, err := p.probe(ctx)
	if err != nil {
		r.Probe, r.ProbeErr = ProbeFailed, err.Error()
		return r
	}
	if info.IdleSeconds >= 0 && info.IdleSeconds < p.cfg.ActiveWithinSeconds {
		r.MaxLocalSlots, r.Severity = limit(p.cfg.ActiveLocalSlots), Soft
		r.Reason = fmt.Sprintf("the machine is in use (idle %.0fs)", info.IdleSeconds)
	}
	return r
}

var hidIdle = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

// ParseHIDIdle reads `ioreg -c IOHIDSystem` as seconds idle, -1 when absent.
func ParseHIDIdle(out string) float64 {
	m := hidIdle.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	ns, _ := strconv.ParseFloat(m[1], 64)
	return ns / 1e9
}

// OffPeakConfig sets the off-peak window. Outside it, admission is limited: the
// run still makes progress, slowly, and speeds up inside the window.
type OffPeakConfig struct {
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

// ParseWindow reads "HH:MM-HH:MM" into minutes since midnight.
func ParseWindow(w string) (start, end int, err error) {
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

// InWindow reports whether t falls inside the window, handling a wrap past midnight.
func InWindow(t time.Time, start, end int) bool {
	m := t.Hour()*60 + t.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// OffPeak limits admission outside its time window. Time comes from the injected
// clock, so tests are deterministic.
type OffPeak struct {
	cfg        OffPeakConfig
	start, end int
	clk        interface{ Now() time.Time }
}

// NewOffPeak returns the sensor; the window must already be valid.
func NewOffPeak(cfg OffPeakConfig, clk interface{ Now() time.Time }) (*OffPeak, error) {
	start, end, err := ParseWindow(cfg.Window)
	if err != nil {
		return nil, err
	}
	return &OffPeak{cfg: cfg, start: start, end: end, clk: clk}, nil
}

func (o *OffPeak) Name() string { return "offpeak" }

func (o *OffPeak) Sample(context.Context, RunSnapshot) Reading {
	r := OpenReading(o.Name())
	if InWindow(o.clk.Now(), o.start, o.end) {
		return r
	}
	r.MaxLocalSlots, r.MaxRemoteSlots, r.Severity = limit(o.cfg.OutsideLocalSlots), limit(o.cfg.OutsideRemoteSlots), Soft
	r.Reason = "outside the off-peak window " + o.cfg.Window
	return r
}
