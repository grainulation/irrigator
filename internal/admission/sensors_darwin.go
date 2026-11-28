//go:build darwin

package admission

import (
	"context"
	"errors"
	"os/exec"
	"time"
)

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// hostPowerProbe reads battery, Low Power Mode and thermal limit with pmset.
func hostPowerProbe(ctx context.Context) (powerInfo, error) {
	batt, err := run(ctx, "pmset", "-g", "batt")
	if err != nil {
		return powerInfo{}, err
	}
	onAC, pct := parsePmsetBatt(batt)
	info := powerInfo{onAC: onAC, batteryPct: pct, thermalLimitPct: -1}
	if out, err := run(ctx, "pmset", "-g", "therm"); err == nil {
		info.thermalLimitPct = parsePmsetTherm(out)
	}
	if out, err := run(ctx, "pmset", "-g"); err == nil {
		info.lowPowerMode = parseLowPowerMode(out)
	}
	return info, nil
}

// hostNetworkProbe treats a missing default route as offline, and a tethering
// gateway as metered.
func hostNetworkProbe(ctx context.Context) (networkInfo, error) {
	out, err := run(ctx, "route", "-n", "get", "default")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return networkInfo{online: false, detail: "no default route"}, nil
		}
		return networkInfo{}, err
	}
	iface, gateway := parseDefaultRoute(out)
	return networkInfo{online: true, metered: isHotspotGateway(gateway), detail: iface + " via " + gateway}, nil
}
