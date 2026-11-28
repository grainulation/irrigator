package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseMemoryAndHIDOutput(t *testing.T) {
	if ParsePressureLevel("1\n") != 1 || ParsePressureLevel("4") != 4 || ParsePressureLevel("garbage") != -1 {
		t.Error("pressure level parsing")
	}
	if got := ParseSwapUsage("vm.swapusage: total = 2048.00M  used = 1024.00M  free = 1024.00M  (encrypted)"); got != 50 {
		t.Errorf("swap usage = %d, want 50", got)
	}
	if ParseSwapUsage("total = 0.00M  used = 0.00M  free = 0.00M") != 0 || ParseSwapUsage("nonsense") != -1 {
		t.Error("swap usage with no swap or unreadable")
	}
	if got := ParseHIDIdle(`    | |   "HIDIdleTime" = 4500000000`); got != 4.5 {
		t.Errorf("idle = %v", got)
	}
	if ParseHIDIdle("no idle field") != -1 {
		t.Error("an absent idle time is unknown")
	}
}

func memProbe(info MemoryInfo, err error) MemoryProbe {
	return func(context.Context) (MemoryInfo, error) { return info, err }
}

func TestMemoryLimitsLocalWorkOnPressureAndHoldsWhenCritical(t *testing.T) {
	cfg := MemoryConfig{WarnLocalSlots: 2, CriticalHold: true, SwapUsedPercentLimit: 60, SwapLocalSlots: 1}
	sample := func(c MemoryConfig, info MemoryInfo) Reading {
		return NewMemory(c, memProbe(info, nil)).Sample(context.Background(), RunSnapshot{})
	}
	if r := sample(cfg, MemoryInfo{PressureLevel: 1, SwapUsedPct: 10}); restricted(r) {
		t.Errorf("normal pressure: %+v", r)
	}
	if r := sample(cfg, MemoryInfo{PressureLevel: 2, SwapUsedPct: 10}); r.MaxLocalSlots != 2 || r.MaxRemoteSlots != Unlimited {
		t.Errorf("warning pressure limits local work only: %+v", r)
	}
	if r := sample(cfg, MemoryInfo{PressureLevel: 4, SwapUsedPct: 10}); r.Hold != HoldTransient || r.Severity != Hard {
		t.Errorf("critical pressure must hold: %+v", r)
	}
	if r := sample(MemoryConfig{WarnLocalSlots: 2}, MemoryInfo{PressureLevel: 4}); r.MaxLocalSlots != 0 || r.Hold != HoldNone {
		t.Errorf("critical without critical_hold still stops local starts: %+v", r)
	}
	if r := sample(cfg, MemoryInfo{PressureLevel: 1, SwapUsedPct: 75}); r.MaxLocalSlots != 1 {
		t.Errorf("heavy swap: %+v", r)
	}
	if r := sample(MemoryConfig{WarnLocalSlots: 2}, MemoryInfo{PressureLevel: 1, SwapUsedPct: 99}); restricted(r) {
		t.Errorf("a zero swap limit turns the swap rule off: %+v", r)
	}
	if r := NewMemory(cfg, memProbe(MemoryInfo{}, errors.New("sysctl failed"))).Sample(context.Background(), RunSnapshot{}); r.Probe != ProbeFailed || restricted(r) {
		t.Errorf("a failed probe says nothing: %+v", r)
	}
}

func TestDiskHoldsBelowTheCriticalFloorAndFailsClosed(t *testing.T) {
	cfg := DiskConfig{LowFreeMB: 5000, CriticalFreeMB: 1000, LowLocalSlots: 1}
	free := func(mb uint64) DiskProbe {
		return func(context.Context) (DiskInfo, error) { return DiskInfo{FreeBytes: mb << 20}, nil }
	}
	sample := func(mb uint64) Reading { return NewDisk(cfg, free(mb)).Sample(context.Background(), RunSnapshot{}) }
	if restricted(sample(20000)) {
		t.Error("plenty of space")
	}
	if r := sample(3000); r.MaxLocalSlots != 1 || r.Hold != HoldNone {
		t.Errorf("low space limits local work: %+v", r)
	}
	if r := sample(500); r.Hold != HoldTransient || r.Severity != Hard {
		t.Errorf("below the critical floor must hold: %+v", r)
	}
	// An unreadable disk fails closed through the evaluator's policy.
	clk := newFakeClock()
	broken := NewDisk(cfg, func(context.Context) (DiskInfo, error) { return DiskInfo{}, errors.New("statfs failed") })
	env := NewEvaluator(Options{Clock: clk, Workers: 4, ExitDwell: time.Second, RampThrottle: time.Second, Sync: true},
		Registration{Sensor: broken, Policy: FailClosed, TTL: time.Second}).Evaluate(context.Background(), RunSnapshot{})
	if env.Hold != HoldTransient {
		t.Errorf("an unreadable disk must hold new admission: %+v", env)
	}
}

func TestDiskStatfsReadsRealFreeSpace(t *testing.T) {
	info, err := DiskStatfs(t.TempDir())(context.Background())
	if err != nil || info.FreeBytes == 0 {
		t.Fatalf("free space = %d, err %v", info.FreeBytes, err)
	}
	if _, err = DiskStatfs(filepath.Join(t.TempDir(), "missing"))(context.Background()); err == nil {
		t.Fatal("a missing path must be an error")
	}
}

func TestLockWaitLimitsAdmissionWhenTheCheckpointLockIsContended(t *testing.T) {
	cfg := DiskConfig{LockWaitMS: 300, LockWaitLocalSlots: 1}
	wait := func(d time.Duration) Reading {
		return NewLockWait(cfg, func(context.Context) (time.Duration, error) { return d, nil }).Sample(context.Background(), RunSnapshot{})
	}
	if restricted(wait(50 * time.Millisecond)) {
		t.Error("a quick lock must not limit anything")
	}
	if r := wait(400 * time.Millisecond); r.MaxLocalSlots != 1 || r.MaxRemoteSlots != 1 || r.Severity != Soft {
		t.Errorf("a contended lock limits both localities: %+v", r)
	}
	if r := NewLockWait(cfg, func(context.Context) (time.Duration, error) { return 0, errors.New("no lock file") }).Sample(context.Background(), RunSnapshot{}); r.Probe != ProbeFailed {
		t.Errorf("probe error: %+v", r)
	}
}

func TestLockWaitProbeMeasuresARealHeldLock(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "state.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	quick, err := lockWaitProbe(root)(context.Background())
	if err != nil || quick > 200*time.Millisecond {
		t.Fatalf("an uncontended lock took %v (%v)", quick, err)
	}
}

func TestPresenceYieldsSoftlyWhileTheUserIsActive(t *testing.T) {
	cfg := PresenceConfig{ActiveWithinSeconds: 120, ActiveLocalSlots: 1}
	idle := func(s float64) Reading {
		return NewPresence(cfg, func(context.Context) (PresenceInfo, error) { return PresenceInfo{IdleSeconds: s}, nil }).Sample(context.Background(), RunSnapshot{})
	}
	if r := idle(10); r.MaxLocalSlots != 1 || r.Severity != Soft || r.MaxRemoteSlots != Unlimited {
		t.Errorf("active user: %+v", r)
	}
	if restricted(idle(600)) {
		t.Error("an idle machine is free to use")
	}
	if restricted(idle(-1)) {
		t.Error("an unknown idle time must not restrict")
	}
	if r := NewPresence(cfg, func(context.Context) (PresenceInfo, error) { return PresenceInfo{}, errors.New("ioreg") }).Sample(context.Background(), RunSnapshot{}); r.Probe != ProbeFailed || restricted(r) {
		t.Errorf("a failed probe fails open: %+v", r)
	}
}

func TestParseWindowAndInWindowIncludingWrapAcrossMidnight(t *testing.T) {
	for _, bad := range []string{"", "9-17", "25:00-07:00", "22:60-07:00", "08:00-08:00", "22:00 - 07:00"} {
		if _, _, err := ParseWindow(bad); err == nil {
			t.Errorf("window %q was accepted", bad)
		}
	}
	start, end, err := ParseWindow("22:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 3, 1, h, m, 0, 0, time.UTC) }
	for _, c := range []struct {
		h, m int
		in   bool
	}{{21, 59, false}, {22, 0, true}, {23, 30, true}, {0, 0, true}, {6, 59, true}, {7, 0, false}, {12, 0, false}} {
		if got := InWindow(at(c.h, c.m), start, end); got != c.in {
			t.Errorf("%02d:%02d in 22:00-07:00 = %v, want %v", c.h, c.m, got, c.in)
		}
	}
	s2, e2, _ := ParseWindow("09:00-17:00")
	if !InWindow(at(9, 0), s2, e2) || InWindow(at(17, 0), s2, e2) || InWindow(at(3, 0), s2, e2) {
		t.Error("a same-day window")
	}
}

func TestOffPeakLimitsAdmissionOutsideTheWindowWithAFakeClock(t *testing.T) {
	clk := newFakeClock() // 12:00 UTC
	op, err := NewOffPeak(OffPeakConfig{Window: "22:00-07:00", OutsideLocalSlots: 1, OutsideRemoteSlots: 2}, clk)
	if err != nil {
		t.Fatal(err)
	}
	if r := op.Sample(context.Background(), RunSnapshot{}); r.MaxLocalSlots != 1 || r.MaxRemoteSlots != 2 || r.Severity != Soft {
		t.Fatalf("midday is outside the window: %+v", r)
	}
	clk.Advance(10*time.Hour + 30*time.Minute) // 22:30
	if r := op.Sample(context.Background(), RunSnapshot{}); restricted(r) {
		t.Fatalf("22:30 is inside the window: %+v", r)
	}
	clk.Advance(9 * time.Hour) // 07:30 next day
	if r := op.Sample(context.Background(), RunSnapshot{}); !restricted(r) {
		t.Fatalf("07:30 is outside the window again: %+v", r)
	}
	if _, err = NewOffPeak(OffPeakConfig{Window: "bogus"}, clk); err == nil {
		t.Fatal("an invalid window was accepted")
	}
}

func TestOffPeakSignalFileHookUsesTheOverrideFormatAndFailsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "carbon.json")
	sig := NewOverrideNamed("offpeak-signal", path)
	if r := sig.Sample(context.Background(), RunSnapshot{}); restricted(r) || sig.Name() != "offpeak-signal" {
		t.Fatalf("no file means no opinion: %+v", r)
	}
	if err := os.WriteFile(path, []byte(`{"max_local_slots":1,"max_remote_slots":1,"reason":"grid carbon is high"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if r := sig.Sample(context.Background(), RunSnapshot{}); r.MaxLocalSlots != 1 || r.MaxRemoteSlots != 1 || r.Reason != "grid carbon is high" {
		t.Fatalf("signal: %+v", r)
	}
	// An external hook that writes garbage must not stall the run.
	if err := os.WriteFile(path, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock()
	env := NewEvaluator(Options{Clock: clk, Workers: 4, ExitDwell: time.Second, RampThrottle: time.Second, Sync: true},
		Registration{Sensor: sig, Policy: FailOpen, TTL: time.Second}).Evaluate(context.Background(), RunSnapshot{})
	if env.Restricted() || len(env.Degraded) != 1 {
		t.Fatalf("a broken external hook should be shown degraded without restricting: %+v", env)
	}
}

func TestOverrideMaxTierSetsTheCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), OverrideFile)
	if err := os.WriteFile(path, []byte(`{"max_tier":"reduced"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if r := NewOverride(path).Sample(context.Background(), RunSnapshot{}); r.ModelTier != TierReduced {
		t.Fatalf("tier = %d", r.ModelTier)
	}
	if err := os.WriteFile(path, []byte(`{"max_tier":"enormous"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if r := NewOverride(path).Sample(context.Background(), RunSnapshot{}); r.Probe != ProbeFailed {
		t.Fatalf("an unknown tier name must fail the probe: %+v", r)
	}
}
