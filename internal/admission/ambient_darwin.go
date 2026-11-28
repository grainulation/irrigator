//go:build darwin

package admission

import (
	"context"
	"os/exec"
	"time"
)

func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// hostMemoryProbe reads the kernel memory pressure level and swap use.
func hostMemoryProbe(ctx context.Context) (MemoryInfo, error) {
	out, err := runCmd(ctx, "sysctl", "-n", "kern.memorystatus_vm_pressure_level")
	if err != nil {
		return MemoryInfo{}, err
	}
	info := MemoryInfo{PressureLevel: ParsePressureLevel(out), SwapUsedPct: -1}
	if swap, err := runCmd(ctx, "sysctl", "-n", "vm.swapusage"); err == nil {
		info.SwapUsedPct = ParseSwapUsage(swap)
	}
	return info, nil
}

// hostPresenceProbe reads the HID idle time with ioreg.
func hostPresenceProbe(ctx context.Context) (PresenceInfo, error) {
	out, err := runCmd(ctx, "ioreg", "-c", "IOHIDSystem")
	if err != nil {
		return PresenceInfo{}, err
	}
	return PresenceInfo{IdleSeconds: ParseHIDIdle(out)}, nil
}
