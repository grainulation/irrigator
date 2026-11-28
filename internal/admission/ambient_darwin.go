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
func hostMemoryProbe(ctx context.Context) (memoryInfo, error) {
	out, err := runCmd(ctx, "sysctl", "-n", "kern.memorystatus_vm_pressure_level")
	if err != nil {
		return memoryInfo{}, err
	}
	info := memoryInfo{pressureLevel: parsePressureLevel(out), swapUsedPct: -1}
	if swap, err := runCmd(ctx, "sysctl", "-n", "vm.swapusage"); err == nil {
		info.swapUsedPct = parseSwapUsage(swap)
	}
	return info, nil
}

// hostPresenceProbe reads the HID idle time with ioreg.
func hostPresenceProbe(ctx context.Context) (presenceInfo, error) {
	out, err := runCmd(ctx, "ioreg", "-c", "IOHIDSystem")
	if err != nil {
		return presenceInfo{}, err
	}
	return presenceInfo{idleSeconds: parseHIDIdle(out)}, nil
}
